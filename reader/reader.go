package reader

import (
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tileid"
)

var ErrNotFound = errors.New("wmtiles: tile not found")

var ErrUnknownVariable = errors.New("wmtiles: unknown variable")

const blockHeaderCacheCap = 64 // LRU cap on parsed block headers + roots, ~1MB worst-case

type Reader struct {
	src    io.ReaderAt
	closer io.Closer

	Header   *format.Header
	Snapshot *Snapshot

	pixSize int

	cacheMu    sync.Mutex
	blockCache map[uint64]*blockCacheEntry
	blockOrder *list.List
	blockTotal int

	leafMu    sync.Mutex
	leafCache map[uint64][]format.BlockTableEntry

	decoderPool sync.Pool
}

type Snapshot struct {
	Header     *format.SnapshotHeader
	Variables  []format.VariableEntry
	TimeCat    format.TimeCatalog
	BlockTable []format.BlockTableEntry

	blockTableLeavesOff uint64

	idByName map[string]uint16

	Metadata map[string]any
}

type blockCacheEntry struct {
	header *format.BlockHeader
	root   []directory.Entry

	// dict is loaded lazily on the first dict-flagged tile read.
	dictMu     sync.Mutex
	dictLoaded bool
	dict       *codec.Dict

	elem *list.Element
}

func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	r.closer = f
	return r, nil
}

func NewReader(src io.ReaderAt) (*Reader, error) {
	r := &Reader{
		src:        src,
		blockCache: map[uint64]*blockCacheEntry{},
		blockOrder: list.New(),
		leafCache:  map[uint64][]format.BlockTableEntry{},
	}
	r.decoderPool.New = func() any {
		d, err := codec.NewDecoder()
		if err != nil {
			return err
		}
		return d
	}

	// one Range request grabs header + (almost always) the full active snapshot: that's the cold-start contract
	cold := make([]byte, format.ColdStartBudget)
	n, err := src.ReadAt(cold, 0)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read cold-start window: %w", err)
	}
	cold = cold[:n]
	if len(cold) < format.HeaderSize {
		return nil, fmt.Errorf("file too short (%d B) for header", len(cold))
	}

	h, err := format.UnmarshalHeader(cold[:format.HeaderSize])
	headerCRCBad := errors.Is(err, format.ErrBadHeaderCRC)
	if err != nil && !headerCRCBad {
		return nil, err
	}
	if h == nil {
		return nil, err
	}
	r.Header = h
	r.pixSize = 1 << h.TilePixelSizeLog2

	if h.FormatVersion != format.FormatVersion {
		return nil, fmt.Errorf("unsupported format version %d (this build expects %d)",
			h.FormatVersion, format.FormatVersion)
	}

	// crash-safety fallback: if the header is good but the active snapshot is corrupt,
	// or the header itself failed CRC, drop back to previous_snapshot if one exists
	if !headerCRCBad {
		snap, err := r.loadSnapshot(cold, h, h.ActiveSnapshotOffset, h.ActiveSnapshotLength)
		if err == nil {
			r.Snapshot = snap
			return r, nil
		}
		if !canFallback(h) {
			return nil, fmt.Errorf("load active snapshot: %w", err)
		}
	}
	if !canFallback(h) {
		return nil, fmt.Errorf("header CRC mismatch and no previous_snapshot to fall back to: %w", err)
	}
	prev, err2 := r.loadSnapshot(cold, h, h.PreviousSnapshotOffset, h.PreviousSnapshotLength)
	if err2 != nil {
		return nil, fmt.Errorf("header/active snapshot bad and previous_snapshot also unreadable: %w", err2)
	}
	r.Snapshot = prev
	return r, nil
}

func canFallback(h *format.Header) bool {
	return h.Flags&format.FlagHasPreviousSnapshot != 0 &&
		h.PreviousSnapshotLength > 0 &&
		h.PreviousSnapshotOffset > 0
}

func (r *Reader) loadSnapshot(cold []byte, h *format.Header, off, length uint64) (*Snapshot, error) {
	var raw []byte
	if off+length <= uint64(len(cold)) {
		raw = cold[off : off+length]
	} else {
		raw = make([]byte, length)
		if _, err := r.src.ReadAt(raw, int64(off)); err != nil {
			return nil, fmt.Errorf("read snapshot: %w", err)
		}
	}
	return parseSnapshotBytes(raw, h)
}

func parseSnapshotBytes(buf []byte, h *format.Header) (*Snapshot, error) {
	if uint64(len(buf)) < uint64(format.SnapshotHeaderSize+format.SnapshotTrailerSize) {
		return nil, fmt.Errorf("snapshot: too short (%d B)", len(buf))
	}
	sh, err := format.UnmarshalSnapshotHeader(buf[:format.SnapshotHeaderSize])
	if err != nil {
		return nil, fmt.Errorf("parse snapshot header: %w", err)
	}
	trailerOff := uint64(len(buf)) - uint64(format.SnapshotTrailerSize)
	tr, err := format.UnmarshalSnapshotTrailer(buf[trailerOff:])
	if err != nil {
		return nil, err
	}
	if tr.SnapshotTotalLength != uint64(len(buf)) {
		return nil, fmt.Errorf("snapshot trailer total %d != buf %d", tr.SnapshotTotalLength, len(buf))
	}
	if got := format.CRC32C(buf[:trailerOff]); got != tr.CRC32C {
		return nil, fmt.Errorf("%w: stored=0x%08X computed=0x%08X", format.ErrBadSnapshotCRC, tr.CRC32C, got)
	}

	varRaw, err := format.Decompress(buf[sh.VariableCatalogOff:sh.VariableCatalogOff+sh.VariableCatalogLen], h.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress catalog: %w", err)
	}
	vars, err := format.UnmarshalVariableCatalog(varRaw, int(sh.NumVariables))
	if err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}

	regular := h.Flags&format.FlagTimeCatalogRegular != 0
	tcRaw := buf[sh.TimeCatalogOff : sh.TimeCatalogOff+sh.TimeCatalogLen]
	if !regular {
		tcRaw, err = format.Decompress(tcRaw, h.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("decompress time catalog: %w", err)
		}
	}
	tc, err := format.UnmarshalTimeCatalog(tcRaw, regular)
	if err != nil {
		return nil, fmt.Errorf("parse time catalog: %w", err)
	}

	rootRaw, err := format.Decompress(buf[sh.BlockTableRootOff:sh.BlockTableRootOff+sh.BlockTableRootLen], h.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress block-table root: %w", err)
	}
	root, err := format.UnmarshalBlockTable(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("parse block-table root: %w", err)
	}

	metadata := map[string]any{}
	if sh.MetadataLen > 0 {
		mdRaw, err := format.Decompress(buf[sh.MetadataOff:sh.MetadataOff+sh.MetadataLen], h.InternalCompression)
		if err == nil {
			_ = json.Unmarshal(mdRaw, &metadata)
		}
	}

	idByName := make(map[string]uint16, len(vars))
	for _, v := range vars {
		idByName[v.Name] = v.VariableID
	}

	return &Snapshot{
		Header:              sh,
		Variables:           vars,
		TimeCat:             *tc,
		BlockTable:          root,
		blockTableLeavesOff: sh.BlockTableLeavesOff,
		idByName:            idByName,
		Metadata:            metadata,
	}, nil
}

func (r *Reader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func (r *Reader) PixelCount() int { return r.pixSize * r.pixSize }

func (r *Reader) VariableID(name string) (uint16, bool) {
	id, ok := r.Snapshot.idByName[name]
	return id, ok
}

func (r *Reader) Variable(name string) (format.VariableEntry, bool) {
	id, ok := r.Snapshot.idByName[name]
	if !ok {
		return format.VariableEntry{}, false
	}
	if int(id) < len(r.Snapshot.Variables) {
		return r.Snapshot.Variables[id], true
	}
	return format.VariableEntry{}, false
}

func (r *Reader) LookupBlock(variableID uint16, timeID uint32) (format.BlockTableEntry, error) {
	entry, ok := format.LookupBlock(r.Snapshot.BlockTable, variableID, timeID)
	if !ok {
		return format.BlockTableEntry{}, ErrNotFound
	}
	if entry.IsLeafPointer {
		leaf, err := r.loadLeaf(entry)
		if err != nil {
			return format.BlockTableEntry{}, err
		}
		entry, ok = format.LookupBlock(leaf, variableID, timeID)
		if !ok || entry.IsLeafPointer {
			return format.BlockTableEntry{}, ErrNotFound
		}
	}
	return entry, nil
}

func (r *Reader) loadLeaf(ptr format.BlockTableEntry) ([]format.BlockTableEntry, error) {
	r.leafMu.Lock()
	if cached, ok := r.leafCache[ptr.BlockOffset]; ok {
		r.leafMu.Unlock()
		return cached, nil
	}
	r.leafMu.Unlock()

	off := r.Snapshot.blockTableLeavesOff + ptr.BlockOffset
	buf := make([]byte, ptr.BlockLength)
	if _, err := r.src.ReadAt(buf, int64(off)); err != nil {
		return nil, fmt.Errorf("read block-table leaf: %w", err)
	}
	raw, err := format.Decompress(buf, r.Header.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress block-table leaf: %w", err)
	}
	entries, err := format.UnmarshalBlockTable(raw)
	if err != nil {
		return nil, fmt.Errorf("parse block-table leaf: %w", err)
	}

	// double-check after the network read: a concurrent caller may have populated the cache
	r.leafMu.Lock()
	if existing, ok := r.leafCache[ptr.BlockOffset]; ok {
		entries = existing
	} else {
		r.leafCache[ptr.BlockOffset] = entries
	}
	r.leafMu.Unlock()
	return entries, nil
}

func (r *Reader) ReadTile(variable string, t uint32, z uint8, x, y uint32, out []float32) error {
	id, ok := r.VariableID(variable)
	if !ok {
		return ErrUnknownVariable
	}
	entry, err := r.LookupBlock(id, t)
	if err != nil {
		return err
	}
	tid := tileid.Encode3D(z, x, y)
	return r.readTileFromBlock(entry, tid, out)
}

func (r *Reader) readTileFromBlock(blk format.BlockTableEntry, tid uint64, out []float32) error {
	hdr, root, cacheEntry, err := r.loadBlockHeaderEntry(blk.BlockOffset, blk.BlockLength)
	if err != nil {
		return err
	}

	dirEntry, ok := directory.FindTile(root, tid)
	if !ok {
		return ErrNotFound
	}
	if dirEntry.IsLeafPointer() {
		leaf, err := r.loadBlockLeaf(blk.BlockOffset, hdr, dirEntry.Offset, dirEntry.Length)
		if err != nil {
			return err
		}
		dirEntry, ok = directory.FindTile(leaf, tid)
		if !ok || dirEntry.IsLeafPointer() {
			return ErrNotFound
		}
	}

	blob := make([]byte, dirEntry.Length)
	if _, err := r.src.ReadAt(blob, int64(blk.BlockOffset+hdr.TileDataOffset+dirEntry.Offset)); err != nil {
		return fmt.Errorf("read tile blob: %w", err)
	}
	if len(out) < r.PixelCount() {
		return errors.New("out buffer too small")
	}
	p := quantize.Params{
		DType:  quantize.DType(blk.DType),
		Scale:  blk.Scale,
		Offset: blk.Offset,
	}

	v := r.decoderPool.Get()
	if err, ok := v.(error); ok {
		return err
	}
	dec := v.(*codec.Decoder)
	defer r.decoderPool.Put(dec)

	if hdr.BlockFlags&format.BlockFlagHasDict != 0 && hdr.DictLength > 0 {
		dict, err := r.loadBlockDict(blk.BlockOffset, blk.BlockLength, hdr, cacheEntry)
		if err != nil {
			return err
		}
		return dec.DecodeToFloat32WithDict(blob, p, r.PixelCount(), dict, out)
	}
	return dec.DecodeToFloat32(blob, p, r.PixelCount(), out)
}

// loadBlockDict reads the dict bytes from the tail of the block on first use
// and caches the digested processor on the block cache entry.
func (r *Reader) loadBlockDict(blockOff, blockLen uint64, hdr *format.BlockHeader, ce *blockCacheEntry) (*codec.Dict, error) {
	ce.dictMu.Lock()
	if ce.dictLoaded {
		d := ce.dict
		ce.dictMu.Unlock()
		return d, nil
	}
	ce.dictMu.Unlock()

	dictOff := hdr.TileDataOffset + hdr.TileDataLength
	if dictOff+uint64(hdr.DictLength) > blockLen {
		return nil, fmt.Errorf("block dict: range %d..%d exceeds block length %d",
			dictOff, dictOff+uint64(hdr.DictLength), blockLen)
	}
	raw := make([]byte, hdr.DictLength)
	if _, err := r.src.ReadAt(raw, int64(blockOff+dictOff)); err != nil {
		return nil, fmt.Errorf("read block dict: %w", err)
	}
	d, err := codec.NewDict(raw, 0)
	if err != nil {
		return nil, fmt.Errorf("init block dict: %w", err)
	}
	ce.dictMu.Lock()
	if !ce.dictLoaded {
		ce.dict = d
		ce.dictLoaded = true
	} else {
		d = ce.dict
	}
	ce.dictMu.Unlock()
	return d, nil
}

func (r *Reader) loadBlockHeader(blockOff, blockLen uint64) (*format.BlockHeader, []directory.Entry, error) {
	hdr, root, _, err := r.loadBlockHeaderEntry(blockOff, blockLen)
	return hdr, root, err
}

func (r *Reader) loadBlockHeaderEntry(blockOff, blockLen uint64) (*format.BlockHeader, []directory.Entry, *blockCacheEntry, error) {
	r.cacheMu.Lock()
	if entry, ok := r.blockCache[blockOff]; ok {
		r.blockOrder.MoveToBack(entry.elem)
		r.cacheMu.Unlock()
		return entry.header, entry.root, entry, nil
	}
	r.cacheMu.Unlock()

	// fetch enough for header + max-sized root in a single request; saves a round trip
	// when the root fits in the prefix (the common case)
	initial := uint64(format.BlockHeaderSize + format.MaxBlockRootBytes)
	if initial > blockLen {
		initial = blockLen
	}
	prefix := make([]byte, initial)
	if _, err := r.src.ReadAt(prefix, int64(blockOff)); err != nil && err != io.EOF {
		return nil, nil, nil, fmt.Errorf("read block header+root: %w", err)
	}
	hdr, err := format.UnmarshalBlockHeader(prefix[:format.BlockHeaderSize])
	if err != nil {
		return nil, nil, nil, err
	}
	rootEnd := hdr.RootDirectoryOffset + uint64(hdr.RootDirectoryLength)
	var rootRaw []byte
	if rootEnd <= uint64(len(prefix)) {
		rootRaw = prefix[hdr.RootDirectoryOffset:rootEnd]
	} else {
		rootRaw = make([]byte, hdr.RootDirectoryLength)
		if _, err := r.src.ReadAt(rootRaw, int64(blockOff+hdr.RootDirectoryOffset)); err != nil {
			return nil, nil, nil, fmt.Errorf("read block root: %w", err)
		}
	}
	rootDecomp, err := format.Decompress(rootRaw, r.Header.InternalCompression)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decompress block root: %w", err)
	}
	root, err := directory.Decode(rootDecomp)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse block root: %w", err)
	}

	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if existing, ok := r.blockCache[blockOff]; ok {
		r.blockOrder.MoveToBack(existing.elem)
		return existing.header, existing.root, existing, nil
	}
	entry := &blockCacheEntry{header: hdr, root: root}
	entry.elem = r.blockOrder.PushBack(blockOff)
	r.blockCache[blockOff] = entry
	r.blockTotal++
	for r.blockTotal > blockHeaderCacheCap {
		oldest := r.blockOrder.Front()
		if oldest == nil {
			break
		}
		r.blockOrder.Remove(oldest)
		delete(r.blockCache, oldest.Value.(uint64))
		r.blockTotal--
	}
	return hdr, root, entry, nil
}

func (r *Reader) loadBlockLeaf(blockOff uint64, hdr *format.BlockHeader, leafOffWithinLeaves uint64, leafLen uint32) ([]directory.Entry, error) {
	buf := make([]byte, leafLen)
	abs := blockOff + hdr.LeafDirectoriesOffset + leafOffWithinLeaves
	if _, err := r.src.ReadAt(buf, int64(abs)); err != nil {
		return nil, fmt.Errorf("read block leaf: %w", err)
	}
	raw, err := format.Decompress(buf, r.Header.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress block leaf: %w", err)
	}
	return directory.Decode(raw)
}

func (r *Reader) SanityCheck() error {
	h := r.Header
	if h.FormatVersion != format.FormatVersion {
		return fmt.Errorf("unsupported format version %d", h.FormatVersion)
	}
	if h.TilePixelSizeLog2 < 7 || h.TilePixelSizeLog2 > 10 {
		return fmt.Errorf("invalid tile pixel size log2 %d", h.TilePixelSizeLog2)
	}
	if h.MaxZoom < h.MinZoom {
		return fmt.Errorf("invalid zoom range [%d, %d]", h.MinZoom, h.MaxZoom)
	}
	if r.Snapshot == nil {
		return errors.New("no snapshot loaded")
	}
	return nil
}

func (r *Reader) EachBlock(fn func(format.BlockTableEntry) error) error {
	for _, e := range r.Snapshot.BlockTable {
		if !e.IsLeafPointer {
			if err := fn(e); err != nil {
				return err
			}
			continue
		}
		leaf, err := r.loadLeaf(e)
		if err != nil {
			return err
		}
		for _, le := range leaf {
			if err := fn(le); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reader) EachTileInBlock(blk format.BlockTableEntry, fn func(tid uint64, e directory.Entry) error) error {
	hdr, root, err := r.loadBlockHeader(blk.BlockOffset, blk.BlockLength)
	if err != nil {
		return err
	}
	return r.eachInLevel(blk.BlockOffset, hdr, root, fn)
}

func (r *Reader) eachInLevel(blockOff uint64, hdr *format.BlockHeader, level []directory.Entry, fn func(uint64, directory.Entry) error) error {
	for _, e := range level {
		if e.IsLeafPointer() {
			leaf, err := r.loadBlockLeaf(blockOff, hdr, e.Offset, e.Length)
			if err != nil {
				return err
			}
			if err := r.eachInLevel(blockOff, hdr, leaf, fn); err != nil {
				return err
			}
			continue
		}
		for k := uint32(0); k < e.RunLength; k++ {
			if err := fn(e.TileID+uint64(k), e); err != nil {
				return err
			}
		}
	}
	return nil
}
