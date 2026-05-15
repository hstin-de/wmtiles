package encoder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/zeebo/xxh3"
)

const (
	// RawGridDefaultChunkSizeLog2 = 8 -> 256x256 chunks. Matches the typical
	// tile size, so memory pressure during decode is comparable, and a point
	// query touches one chunk of <= 256x256 source pixels.
	RawGridDefaultChunkSizeLog2 = 8
)

type RawGridSpec struct {
	Variable string
	TimeStep uint32

	Nx, Ny     int
	Lat0, Lon0 float64
	DY, DX     float64

	// MissingValue marks the source NoData sentinel; NaN means "use NaN
	// directly". Values matching MissingValue are recoded to NaN before
	// quantisation so dedup of all-NoData chunks works regardless of source
	// convention.
	MissingValue float64

	// Precision drives FitParams; 0 means "full-range u16".
	Precision float64

	// ChunkSizeLog2 picks the chunk side in source pixels. 0 means default
	// (RawGridDefaultChunkSizeLog2). Allowed range matches the format spec
	// (4..12, so 16..4096 pixels per side).
	ChunkSizeLog2 uint8
}

type rawGridBlockBuilder struct {
	variableID uint16
	timeID     uint32
	variable   string

	params  quantize.Params
	codec   uint8
	nodata  uint32
	vmin    float64
	vmax    float64
	missing float64

	section      *format.RawGridSection
	sectionBytes []byte // compressed wire bytes of the section (the "root" region)
	tileData     []byte // concatenated chunk payloads
	addressed    uint32
	contents     uint64
}

// Implements writableBlock — see streaming.go for the interface.
func (b *rawGridBlockBuilder) writeBlockTo(w io.Writer) (int64, error) {
	var total int64
	hdr := b.header()
	if n, err := w.Write(format.MarshalBlockHeader(hdr)); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if n, err := w.Write(b.sectionBytes); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	if n, err := w.Write(b.tileData); err != nil {
		return total + int64(n), err
	} else {
		total += int64(n)
	}
	return total, nil
}

func (b *rawGridBlockBuilder) header() *format.BlockHeader {
	rootOff := uint64(format.BlockHeaderSize)
	rootLen := uint32(len(b.sectionBytes))
	tileDataOff := rootOff + uint64(rootLen)
	return &format.BlockHeader{
		BlockFormatVersion:    format.BlockFormatVersion,
		BlockFlags:            format.BlockFlagRawGrid,
		RootDirectoryOffset:   rootOff,
		RootDirectoryLength:   rootLen,
		DictLength:            0,
		LeafDirectoriesOffset: 0,
		LeafDirectoriesLength: 0,
		TileDataOffset:        tileDataOff,
		TileDataLength:        uint64(len(b.tileData)),
		NumAddressedTiles:     b.addressed,
		NumDirectoryEntries:   b.addressed,
	}
}

func (b *rawGridBlockBuilder) blockTableEntry(fileOffset uint64) format.BlockTableEntry {
	blockLen := uint64(format.BlockHeaderSize) + uint64(len(b.sectionBytes)) + uint64(len(b.tileData))
	return format.BlockTableEntry{
		VariableID:          b.variableID,
		TimeID:              b.timeID,
		BlockOffset:         fileOffset,
		BlockLength:         blockLen,
		DType:               uint8(b.params.DType),
		Codec:               b.codec,
		Scale:               b.params.Scale,
		Offset:              b.params.Offset,
		NoData:              b.nodata,
		ValueMin:            b.vmin,
		ValueMax:            b.vmax,
		NumAddressedTiles:   uint64(b.addressed),
		NumDirectoryEntries: uint64(b.addressed),
		NumTileContents:     b.contents,
	}
}

func (b *rawGridBlockBuilder) release() {
	b.sectionBytes = nil
	b.tileData = nil
	b.section = nil
}

// finishBlock is part of the writableBlock interface; raw-grid blocks are
// already finalised at WriteValues time, so this is a no-op.
func (b *rawGridBlockBuilder) finishBlock(comp format.InternalCompression, opts dictOptions) error {
	return nil
}

// chunkSpec is reused across goroutines while encoding chunks in parallel.
type chunkSpec struct {
	cx, cy uint32
	w, h   int
}

// encodeRawGridBlock turns a source-grid array into a finalised raw-grid block
// builder, ready to be written to disk. values is row-major: values[y*Nx + x].
func encodeRawGridBlock(
	spec RawGridSpec,
	variableID uint16,
	values []float32,
	comp format.InternalCompression,
	zstdLevel int,
) (*rawGridBlockBuilder, error) {
	if spec.Nx <= 0 || spec.Ny <= 0 {
		return nil, fmt.Errorf("raw grid block %q: invalid grid %dx%d", spec.Variable, spec.Nx, spec.Ny)
	}
	if len(values) != spec.Nx*spec.Ny {
		return nil, fmt.Errorf("raw grid block %q: data len %d != Nx*Ny %d", spec.Variable, len(values), spec.Nx*spec.Ny)
	}
	chunkLog2 := spec.ChunkSizeLog2
	if chunkLog2 == 0 {
		chunkLog2 = RawGridDefaultChunkSizeLog2
	}
	if chunkLog2 < 4 || chunkLog2 > 12 {
		return nil, fmt.Errorf("raw grid block %q: chunk size log2 %d out of [4, 12]", spec.Variable, chunkLog2)
	}
	chunkSize := 1 << chunkLog2

	canonicalised := canonicaliseValues(values, spec.MissingValue)
	vmin, vmax, hasFinite := finiteRange(canonicalised)
	if !hasFinite {
		// All-NaN block; declare a degenerate range so FitParams returns u16 const(NaN).
		vmin, vmax = 0, 0
	}
	params := quantize.FitParams(vmin, vmax, spec.Precision)
	stride := params.DType.Bytes()

	chunkCountX := uint32((spec.Nx + chunkSize - 1) / chunkSize)
	chunkCountY := uint32((spec.Ny + chunkSize - 1) / chunkSize)
	totalChunks := int(chunkCountX) * int(chunkCountY)

	jobs := make([]chunkSpec, 0, totalChunks)
	for cy := uint32(0); cy < chunkCountY; cy++ {
		yStart := int(cy) * chunkSize
		yEnd := yStart + chunkSize
		if yEnd > spec.Ny {
			yEnd = spec.Ny
		}
		for cx := uint32(0); cx < chunkCountX; cx++ {
			xStart := int(cx) * chunkSize
			xEnd := xStart + chunkSize
			if xEnd > spec.Nx {
				xEnd = spec.Nx
			}
			jobs = append(jobs, chunkSpec{cx: cx, cy: cy, w: xEnd - xStart, h: yEnd - yStart})
		}
	}

	type encoded struct {
		blob []byte
		hash [16]byte
		tag  byte
		w, h int
	}
	results := make([]encoded, totalChunks)

	var workerErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if workerErr == nil {
			workerErr = err
		}
	}
	getErr := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return workerErr
	}

	jobCh := make(chan int, len(jobs))
	for i := range jobs {
		jobCh <- i
	}
	close(jobCh)

	numWorkers := goMaxParallelEncoders()
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enc, err := codec.NewEncoderWithOpts(zstdLevel, false)
			if err != nil {
				setErr(err)
				return
			}
			defer enc.Close()
			var quantBuf []byte
			var chunkBuf []float32
			for idx := range jobCh {
				if getErr() != nil {
					return
				}
				j := jobs[idx]
				n := j.w * j.h
				if cap(chunkBuf) < n {
					chunkBuf = make([]float32, n)
				}
				chunkBuf = chunkBuf[:n]
				copyChunk(canonicalised, spec.Nx, int(j.cx)*chunkSize, int(j.cy)*chunkSize, j.w, j.h, chunkBuf)

				if cap(quantBuf) < n*stride {
					quantBuf = make([]byte, n*stride)
				}
				quant := quantBuf[:n*stride]
				quantize.Encode(chunkBuf, params, quant)

				blob, tag, err := encodeChunkBlob(enc, quant, params, n)
				if err != nil {
					setErr(err)
					return
				}
				h := xxh3.Hash128(blob)
				var hash [16]byte
				binary.LittleEndian.PutUint64(hash[0:8], h.Lo)
				binary.LittleEndian.PutUint64(hash[8:16], h.Hi)
				results[idx] = encoded{blob: blob, hash: hash, tag: tag, w: j.w, h: j.h}
			}
		}()
	}
	wg.Wait()
	if err := getErr(); err != nil {
		return nil, fmt.Errorf("raw grid block %q: chunk encode: %w", spec.Variable, err)
	}

	dedup := make(map[[16]byte]struct {
		off, ln uint64
	}, totalChunks)
	chunkOffsets := make([]uint64, totalChunks)
	chunkLengths := make([]uint64, totalChunks)
	tileData := make([]byte, 0)
	var contents uint64
	codecCounts := map[byte]int{}
	for i, r := range results {
		if r.blob == nil {
			// shouldn't happen; treat as empty
			continue
		}
		codecCounts[r.tag]++
		if hit, ok := dedup[r.hash]; ok {
			chunkOffsets[i] = hit.off
			chunkLengths[i] = hit.ln
			continue
		}
		off := uint64(len(tileData))
		tileData = append(tileData, r.blob...)
		ln := uint64(len(r.blob))
		chunkOffsets[i] = off
		chunkLengths[i] = ln
		dedup[r.hash] = struct {
			off, ln uint64
		}{off: off, ln: ln}
		contents++
	}

	// dominant chunk codec is a stats hint on the block-table entry; the
	// per-chunk tag inside each payload is still authoritative.
	dominantCodec := uint8(codec.IDBitshuffleZstd)
	bestCount := 0
	for tag, count := range codecCounts {
		if count > bestCount {
			bestCount = count
			dominantCodec = tag
		}
	}

	section := &format.RawGridSection{
		SchemaVersion: format.RawGridSchemaVersion,
		ChunkSizeLog2: chunkLog2,
		Nx:            uint32(spec.Nx),
		Ny:            uint32(spec.Ny),
		Lat0:          spec.Lat0,
		Lon0:          spec.Lon0,
		DY:            spec.DY,
		DX:            spec.DX,
		MissingValue:  spec.MissingValue,
		ChunkCountX:   chunkCountX,
		ChunkCountY:   chunkCountY,
		ChunkOffsets:  chunkOffsets,
		ChunkLengths:  chunkLengths,
	}
	sectionRaw, err := format.MarshalRawGridSection(section)
	if err != nil {
		return nil, fmt.Errorf("raw grid block %q: marshal section: %w", spec.Variable, err)
	}
	sectionComp, err := format.Compress(sectionRaw, comp)
	if err != nil {
		return nil, fmt.Errorf("raw grid block %q: compress section: %w", spec.Variable, err)
	}
	if len(sectionComp) > format.MaxBlockRootBytes {
		return nil, fmt.Errorf("raw grid block %q: section %d > limit %d (chunks=%d); use a smaller chunk size",
			spec.Variable, len(sectionComp), format.MaxBlockRootBytes, totalChunks)
	}

	var nodata uint32
	switch params.DType {
	case quantize.DTypeU8:
		nodata = uint32(quantize.SentinelU8)
	case quantize.DTypeU16:
		nodata = uint32(quantize.SentinelU16)
	case quantize.DTypeF32:
		nodata = quantize.CanonicalQuietNaN
	}

	b := &rawGridBlockBuilder{
		variableID:   variableID,
		timeID:       spec.TimeStep,
		variable:     spec.Variable,
		params:       params,
		codec:        dominantCodec,
		nodata:       nodata,
		vmin:         vmin,
		vmax:         vmax,
		missing:      spec.MissingValue,
		section:      section,
		sectionBytes: sectionComp,
		tileData:     tileData,
		addressed:    uint32(totalChunks),
		contents:     contents,
	}
	return b, nil
}

// encodeChunkBlob picks a codec for a single raw-grid chunk. constant chunks
// collapse to a 5-byte payload; everything else uses bitshuffle+zstd.
func encodeChunkBlob(enc *codec.Encoder, quant []byte, params quantize.Params, nPixels int) ([]byte, byte, error) {
	stride := params.DType.Bytes()
	if isAllSame(quant, stride) {
		blob, err := enc.EncodeWith(codec.IDConstant, quant, params, nPixels)
		return blob, codec.IDConstant, err
	}
	blob, err := enc.EncodeWith(codec.IDBitshuffleZstd, quant, params, nPixels)
	return blob, codec.IDBitshuffleZstd, err
}

func isAllSame(b []byte, stride int) bool {
	if len(b) <= stride {
		return true
	}
	for i := stride; i < len(b); i += stride {
		for j := 0; j < stride; j++ {
			if b[i+j] != b[j] {
				return false
			}
		}
	}
	return true
}

// canonicaliseValues copies values, replacing MissingValue with NaN so the
// quantiser sees a single missing pattern and the dedup hash for all-NoData
// chunks is stable across inputs that use different sentinels.
func canonicaliseValues(in []float32, missing float64) []float32 {
	out := make([]float32, len(in))
	if math.IsNaN(missing) {
		copy(out, in)
		return out
	}
	m32 := float32(missing)
	for i, v := range in {
		if v == m32 {
			out[i] = float32(math.NaN())
		} else {
			out[i] = v
		}
	}
	return out
}

func finiteRange(values []float32) (float64, float64, bool) {
	vmin := math.Inf(+1)
	vmax := math.Inf(-1)
	any := false
	for _, v := range values {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		any = true
		if f < vmin {
			vmin = f
		}
		if f > vmax {
			vmax = f
		}
	}
	if !any {
		return 0, 0, false
	}
	return vmin, vmax, true
}

func copyChunk(src []float32, nx, xStart, yStart, w, h int, dst []float32) {
	for row := 0; row < h; row++ {
		srcOff := (yStart+row)*nx + xStart
		dstOff := row * w
		copy(dst[dstOff:dstOff+w], src[srcOff:srcOff+w])
	}
}

var errRawGridUnknownVariable = errors.New("raw grid block: variable not declared")

func (s *StreamingEncoder) EncodeRawGridBlock(spec RawGridSpec, values []float32) error {
	if err := s.checkErr(); err != nil {
		return err
	}
	s.blockMu.RLock()
	id, ok := s.idByName[spec.Variable]
	s.blockMu.RUnlock()
	if !ok {
		return fmt.Errorf("EncodeRawGridBlock %q: %w", spec.Variable, errRawGridUnknownVariable)
	}

	bb, err := encodeRawGridBlock(spec, id, values, s.opts.InternalCompression, s.opts.ZstdLevel)
	if err != nil {
		return err
	}

	k := blockKey{variableID: id, timeID: spec.TimeStep}
	s.blockMu.Lock()
	defer s.blockMu.Unlock()
	if _, dup := s.rawBlocks[k]; dup {
		return fmt.Errorf("EncodeRawGridBlock %q t=%d: already encoded", spec.Variable, spec.TimeStep)
	}
	if _, dup := s.blocks[k]; dup {
		return fmt.Errorf("EncodeRawGridBlock %q t=%d: already declared as tiled block", spec.Variable, spec.TimeStep)
	}
	if s.rawBlocks == nil {
		s.rawBlocks = make(map[blockKey]*rawGridBlockBuilder)
	}
	s.rawBlocks[k] = bb
	s.rawDeclarations = append(s.rawDeclarations, k)

	if cur, ok := s.globalMin[id]; !ok || bb.vmin < cur {
		s.globalMin[id] = bb.vmin
	}
	if cur, ok := s.globalMax[id]; !ok || bb.vmax > cur {
		s.globalMax[id] = bb.vmax
	}
	if _, ok := s.defaultDType[spec.Variable]; !ok {
		s.defaultDType[spec.Variable] = uint8(bb.params.DType)
	}
	return nil
}

func goMaxParallelEncoders() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		return 1
	}
	if n > 16 {
		// Chunk encode is short and zstd cgo allocates per encoder; oversubscribing
		// past ~16 stops paying off and starts contending on the cgo arena.
		return 16
	}
	return n
}
