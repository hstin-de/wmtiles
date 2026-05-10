package encoder

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
)

type blockBuilder struct {
	variableID uint16
	timeID     uint32
	variable   string

	params  quantize.Params
	codec   uint8
	nodata  uint32
	vmin    float64
	vmax    float64
	nPixels int

	mu        sync.Mutex
	dedup     map[[32]byte]dedupVal
	records   []recordVal
	tileData  []byte
	uniqueLen uint64
	contents  uint64

	// uniques holds post-transform inner bytes per deduplicated content in
	// single-pass dict mode; finishBlockDict drains it into tileData. Empty
	// when dict mode isn't active.
	uniques []uniqueInner

	rootBytes  []byte
	leavesBlob []byte
	hasLeaves  bool
	rootLen    uint32

	dictBytes []byte
}

type uniqueInner struct {
	tag   byte
	inner []byte
}

func newBlockBuilder(varID uint16, varName string, timeID uint32, params quantize.Params, defaultCodec uint8, nPixels int) *blockBuilder {
	var nodata uint32
	switch params.DType {
	case quantize.DTypeU8:
		nodata = uint32(quantize.SentinelU8)
	case quantize.DTypeU16:
		nodata = uint32(quantize.SentinelU16)
	case quantize.DTypeF32:
		nodata = quantize.CanonicalQuietNaN
	}
	return &blockBuilder{
		variableID: varID,
		timeID:     timeID,
		variable:   varName,
		params:     params,
		codec:      defaultCodec,
		nodata:     nodata,
		nPixels:    nPixels,
		dedup:      make(map[[32]byte]dedupVal),
	}
}

func (b *blockBuilder) addEncoded(tid uint64, key [32]byte, blob []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.dedup[key]; ok {
		b.records = append(b.records, recordVal{tid: tid, offset: v.offset, length: v.length})
		return
	}
	off := b.uniqueLen
	b.tileData = append(b.tileData, blob...)
	ln := uint32(len(blob))
	b.uniqueLen += uint64(ln)
	b.contents++
	b.dedup[key] = dedupVal{offset: off, length: ln}
	b.records = append(b.records, recordVal{tid: tid, offset: off, length: ln})
}

// dedupHit registers a record for a known content hash and returns true,
// letting the caller skip the codec pass entirely on duplicate payloads.
func (b *blockBuilder) dedupHit(tid uint64, key [32]byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.dedup[key]
	if !ok {
		return false
	}
	b.records = append(b.records, recordVal{tid: tid, offset: v.offset, length: v.length})
	return true
}

// addEncodedInner is the dict-mode counterpart to addEncoded: it stores
// post-transform bytes in b.uniques and lets finishBlockDict compress later.
// dedup offsets index into b.uniques until that pass rewrites them.
func (b *blockBuilder) addEncodedInner(tid uint64, key [32]byte, tag byte, inner []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.dedup[key]; ok {
		b.records = append(b.records, recordVal{tid: tid, offset: v.offset, length: v.length})
		return
	}
	idx := uint64(len(b.uniques))
	b.uniques = append(b.uniques, uniqueInner{tag: tag, inner: inner})
	b.contents++
	b.dedup[key] = dedupVal{offset: idx, length: 0}
	b.records = append(b.records, recordVal{tid: tid, offset: idx, length: 0})
}

func (b *blockBuilder) finishBlock(comp format.InternalCompression, dictOpts dictOptions) error {
	if len(b.records) == 0 {
		return fmt.Errorf("block (var=%d time=%d): no tiles", b.variableID, b.timeID)
	}

	if len(b.uniques) > 0 {
		// single-pass dict mode: workers staged inner bytes; train + compress now
		if err := b.finishBlockDict(dictOpts); err != nil {
			return fmt.Errorf("block (var=%d time=%d) dict pass: %w", b.variableID, b.timeID, err)
		}
	} else if dictOpts.enabled {
		// two-pass mode: tiles already compressed; peel + recompress with dict
		if err := b.optimizeWithDict(dictOpts); err != nil {
			return fmt.Errorf("block (var=%d time=%d) dict pass: %w", b.variableID, b.timeID, err)
		}
	}

	// directory entries must be tile-id sorted: FindTile relies on binary search
	sort.Slice(b.records, func(i, j int) bool { return b.records[i].tid < b.records[j].tid })

	var dirBuilder directory.Builder
	for _, r := range b.records {
		dirBuilder.Append(r.tid, r.length, r.offset)
	}
	entries := dirBuilder.Entries()

	rootEntries, leafBlobs, leavesBlob, err := buildBlockHierarchy(entries, comp)
	if err != nil {
		return err
	}
	rootRaw := directory.Encode(rootEntries)
	rootComp, err := format.Compress(rootRaw, comp)
	if err != nil {
		return fmt.Errorf("compress block root dir: %w", err)
	}
	if len(rootComp) > format.MaxBlockRootBytes {
		return fmt.Errorf("block (var=%d time=%d) root %d > limit %d",
			b.variableID, b.timeID, len(rootComp), format.MaxBlockRootBytes)
	}
	b.rootBytes = rootComp
	b.rootLen = uint32(len(rootComp))
	b.leavesBlob = leavesBlob
	b.hasLeaves = len(leafBlobs) > 0
	return nil
}

func (b *blockBuilder) header() *format.BlockHeader {
	flags := uint16(0)
	if b.hasLeaves {
		flags |= format.BlockFlagHasLeafDirectories
	}
	if len(b.dictBytes) > 0 {
		flags |= format.BlockFlagHasDict
	}
	rootOff := uint64(format.BlockHeaderSize)
	leavesOff := rootOff + uint64(b.rootLen)
	leavesLen := uint64(len(b.leavesBlob))
	tileDataOff := leavesOff + leavesLen
	if !b.hasLeaves {
		leavesOff = 0
	}
	addressed := uint32(0)
	for _, r := range b.records {
		_ = r
		addressed++
	}
	return &format.BlockHeader{
		BlockFormatVersion:    format.BlockFormatVersion,
		BlockFlags:            flags,
		RootDirectoryOffset:   rootOff,
		RootDirectoryLength:   b.rootLen,
		DictLength:            uint32(len(b.dictBytes)),
		LeafDirectoriesOffset: leavesOff,
		LeafDirectoriesLength: leavesLen,
		TileDataOffset:        tileDataOff,
		TileDataLength:        b.uniqueLen,
		NumAddressedTiles:     addressed,
		NumDirectoryEntries:   uint32(len(b.records)),
	}
}

// writeBlockTo streams the block layout (header, root, leaves, tile data, dict)
// to w. Streaming keeps peak memory at the largest single part — materialising
// one merged buffer doubled peak resident set on full GFS encodes and OOM'd.
func (b *blockBuilder) writeBlockTo(w io.Writer) (int64, error) {
	var total int64
	hdr := b.header()
	if n, err := w.Write(format.MarshalBlockHeader(hdr)); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if n, err := w.Write(b.rootBytes); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if n, err := w.Write(b.leavesBlob); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if n, err := w.Write(b.tileData); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if len(b.dictBytes) > 0 {
		if n, err := w.Write(b.dictBytes); err != nil {
			return total + int64(n), err
		} else {
			total += int64(n)
		}
	}
	return total, nil
}

// release frees the large per-block buffers so a multi-block Finish doesn't
// hold every block's tileData resident at once. call after the block has been
// written to disk and its block-table entry recorded
func (b *blockBuilder) release() {
	b.tileData = nil
	b.rootBytes = nil
	b.leavesBlob = nil
	b.dedup = nil
	b.records = nil
}

func (b *blockBuilder) blockTableEntry(fileOffset uint64) format.BlockTableEntry {
	return format.BlockTableEntry{
		VariableID:          b.variableID,
		TimeID:              b.timeID,
		BlockOffset:         fileOffset,
		BlockLength:         uint64(format.BlockHeaderSize) + uint64(b.rootLen) + uint64(len(b.leavesBlob)) + b.uniqueLen + uint64(len(b.dictBytes)),
		DType:               uint8(b.params.DType),
		Codec:               b.codec,
		Scale:               b.params.Scale,
		Offset:              b.params.Offset,
		NoData:              b.nodata,
		ValueMin:            b.vmin,
		ValueMax:            b.vmax,
		NumAddressedTiles:   uint64(len(b.records)),
		NumDirectoryEntries: uint64(len(b.records)),
		NumTileContents:     b.contents,
	}
}

func buildBlockHierarchy(entries []directory.Entry, comp format.InternalCompression) (
	rootEntries []directory.Entry, leafBlobs [][]byte, leavesBlob []byte, err error,
) {
	rootRaw := directory.Encode(entries)
	rootComp, err := format.Compress(rootRaw, comp)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(rootComp) <= format.MaxBlockRootBytes {
		return entries, nil, nil, nil // single-level directory fits, no leaves needed
	}
	// start at sqrt(N) leaves and double until the root fits: keeps tree shallow but bounded
	k := isqrt(len(entries))
	if k < 2 {
		k = 2
	}
	for {
		root, leaves, leavesData, ok, err := tryPartitionBlockDir(entries, k, comp)
		if err != nil {
			return nil, nil, nil, err
		}
		if ok {
			return root, leaves, leavesData, nil
		}
		if k >= len(entries) {
			return nil, nil, nil, fmt.Errorf("cannot fit block root within %d bytes (k=%d)",
				format.MaxBlockRootBytes, k)
		}
		k *= 2
	}
}

func tryPartitionBlockDir(entries []directory.Entry, k int, comp format.InternalCompression) (
	root []directory.Entry, leaves [][]byte, leavesData []byte, ok bool, err error,
) {
	if k > len(entries) {
		k = len(entries)
	}
	per := (len(entries) + k - 1) / k
	root = make([]directory.Entry, 0, k)
	leaves = make([][]byte, 0, k)
	var off uint64
	for start := 0; start < len(entries); start += per {
		end := start + per
		if end > len(entries) {
			end = len(entries)
		}
		slice := entries[start:end]
		raw := directory.Encode(slice)
		blob, e := format.Compress(raw, comp)
		if e != nil {
			return nil, nil, nil, false, e
		}
		root = append(root, directory.Entry{
			TileID:    slice[0].TileID,
			RunLength: 0,
			Length:    uint32(len(blob)),
			Offset:    off,
		})
		leaves = append(leaves, blob)
		leavesData = append(leavesData, blob...)
		off += uint64(len(blob))
	}
	rootRaw := directory.Encode(root)
	rootComp, err := format.Compress(rootRaw, comp)
	if err != nil {
		return nil, nil, nil, false, err
	}
	ok = len(rootComp) <= format.MaxBlockRootBytes
	return root, leaves, leavesData, ok, nil
}

var errBlockNotDeclared = errors.New("block was not declared")

func dtypeFor(vmin, vmax, precision float64) quantize.DType {
	return quantize.FitParams(vmin, vmax, precision).DType
}

const defaultCodec = codec.IDBitshuffleZstd

func isqrt(n int) int {
	if n <= 1 {
		return 1
	}
	x := 1
	for x*x <= n {
		x++
	}
	return x - 1
}
