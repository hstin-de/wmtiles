package reader

import (
	"container/list"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
)

type SamplePoint struct {
	Lat, Lon float64
}

// Zero values pick sensible defaults.
type SampleCoalesceOptions struct {
	MaxGapBytes     uint64
	MaxRequestBytes uint64
}

const (
	defaultSampleMaxGapBytes     uint64 = 32 * 1024
	defaultSampleMaxRequestBytes uint64 = 1 << 20
)

// NaN out-of-grid; ErrNotRawGrid on a tile pyramid; ErrNotFound on a missing block.
func (r *Reader) ReadSample(variable string, t uint32, lat, lon float64) (float32, error) {
	id, ok := r.VariableID(variable)
	if !ok {
		return 0, ErrUnknownVariable
	}
	entry, err := r.LookupBlock(id, t)
	if err != nil {
		return 0, err
	}
	hdr, _, ce, err := r.loadBlockHeaderEntry(entry.BlockOffset, entry.BlockLength)
	if err != nil {
		return 0, err
	}
	if hdr.BlockFlags&format.BlockFlagRawGrid == 0 {
		return 0, ErrNotRawGrid
	}
	value, err := r.sampleFromRawBlock(entry, hdr, ce, lat, lon)
	if err != nil {
		return 0, err
	}
	return value, nil
}

// Chunk fetches are coalesced into shared range requests.
func (r *Reader) ReadSamples(variable string, t uint32, points []SamplePoint, opts ...SampleCoalesceOptions) ([]float32, error) {
	id, ok := r.VariableID(variable)
	if !ok {
		return nil, ErrUnknownVariable
	}
	entry, err := r.LookupBlock(id, t)
	if err != nil {
		return nil, err
	}
	hdr, _, ce, err := r.loadBlockHeaderEntry(entry.BlockOffset, entry.BlockLength)
	if err != nil {
		return nil, err
	}
	if hdr.BlockFlags&format.BlockFlagRawGrid == 0 {
		return nil, ErrNotRawGrid
	}
	opt := SampleCoalesceOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.MaxGapBytes == 0 {
		opt.MaxGapBytes = defaultSampleMaxGapBytes
	}
	if opt.MaxRequestBytes == 0 {
		opt.MaxRequestBytes = defaultSampleMaxRequestBytes
	}

	out := make([]float32, len(points))
	if err := r.prefetchChunksFor(entry, hdr, ce, points, opt); err != nil {
		return nil, err
	}
	for i, p := range points {
		v, err := r.sampleFromRawBlock(entry, hdr, ce, p.Lat, p.Lon)
		if err != nil {
			return nil, fmt.Errorf("sample %d (%g, %g): %w", i, p.Lat, p.Lon, err)
		}
		out[i] = v
	}
	return out, nil
}

// any-NaN neighbour → NaN; out-of-grid → NaN.
func (r *Reader) sampleFromRawBlock(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	lat, lon float64,
) (float32, error) {
	g := ce.rawGrid
	if g.DX == 0 || g.DY == 0 {
		return 0, errors.New("raw-grid: zero dx/dy")
	}
	gx := (lon - g.Lon0) / g.DX
	gy := (lat - g.Lat0) / g.DY
	if math.IsNaN(gx) || math.IsNaN(gy) {
		return float32(math.NaN()), nil
	}
	if gx < 0 || gy < 0 {
		return float32(math.NaN()), nil
	}
	if gx > float64(g.Nx-1) || gy > float64(g.Ny-1) {
		return float32(math.NaN()), nil
	}

	x0 := int(math.Floor(gx))
	y0 := int(math.Floor(gy))
	x1 := x0 + 1
	y1 := y0 + 1
	if x1 > int(g.Nx)-1 {
		x1 = x0
	}
	if y1 > int(g.Ny)-1 {
		y1 = y0
	}
	wx := float32(gx - float64(x0))
	wy := float32(gy - float64(y0))

	v00, err := r.pixelAt(entry, hdr, ce, x0, y0)
	if err != nil {
		return 0, err
	}
	v10, err := r.pixelAt(entry, hdr, ce, x1, y0)
	if err != nil {
		return 0, err
	}
	v01, err := r.pixelAt(entry, hdr, ce, x0, y1)
	if err != nil {
		return 0, err
	}
	v11, err := r.pixelAt(entry, hdr, ce, x1, y1)
	if err != nil {
		return 0, err
	}
	// NaN propagation: any missing neighbour invalidates the interpolation.
	if isNaN32(v00) || isNaN32(v10) || isNaN32(v01) || isNaN32(v11) {
		return float32(math.NaN()), nil
	}
	a := v00*(1-wx) + v10*wx
	b := v01*(1-wx) + v11*wx
	return a*(1-wy) + b*wy, nil
}

func (r *Reader) pixelAt(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	x, y int,
) (float32, error) {
	g := ce.rawGrid
	cs := g.ChunkSize()
	cx := uint32(x / cs)
	cy := uint32(y / cs)
	idx := uint32(g.ChunkIndex(cx, cy))
	pixels, err := r.loadChunk(entry, hdr, ce, idx, cx, cy)
	if err != nil {
		return 0, err
	}
	w := g.ChunkWidth(cx)
	row := y - int(cy)*cs
	col := x - int(cx)*cs
	return pixels[row*w+col], nil
}

// chunk (offset, length) in cell-row-major order.
type fineIndex struct {
	offsets []uint64
	lengths []uint64
	cellW   uint32
}

// cached unbounded per block; fine-indices are ~KB.
func (r *Reader) loadFineIndex(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	coarseIdx int,
) (*fineIndex, error) {
	ce.fineMu.Lock()
	if fi, ok := ce.fineCache[coarseIdx]; ok {
		ce.fineMu.Unlock()
		return fi, nil
	}
	ce.fineMu.Unlock()

	g := ce.rawGrid
	if coarseIdx < 0 || coarseIdx >= len(g.CoarseTable) {
		return nil, fmt.Errorf("fine index: coarse idx %d out of range [0,%d)", coarseIdx, len(g.CoarseTable))
	}
	cce := g.CoarseTable[coarseIdx]
	coarseCountX := int(g.CoarseCountX())
	coarseCx := uint32(coarseIdx % coarseCountX)
	coarseCy := uint32(coarseIdx / coarseCountX)
	cellW, cellH := g.CoarseCellChunkExtent(coarseCx, coarseCy)
	expected := int(cellW) * int(cellH)

	var offsets, lengths []uint64
	if cce.Length == 0 {
		// Degenerate cell with no chunks (only possible for malformed inputs).
		offsets = make([]uint64, expected)
		lengths = make([]uint64, expected)
	} else {
		buf := make([]byte, cce.Length)
		abs := entry.BlockOffset + hdr.LeafDirectoriesOffset + uint64(cce.Offset)
		if _, err := r.src.ReadAt(buf, int64(abs)); err != nil {
			return nil, fmt.Errorf("read fine index for coarse %d: %w", coarseIdx, err)
		}
		var err error
		offsets, lengths, err = format.UnmarshalFineIndex(buf, expected)
		if err != nil {
			return nil, fmt.Errorf("parse fine index for coarse %d: %w", coarseIdx, err)
		}
	}
	fi := &fineIndex{offsets: offsets, lengths: lengths, cellW: cellW}

	ce.fineMu.Lock()
	defer ce.fineMu.Unlock()
	if existing, ok := ce.fineCache[coarseIdx]; ok {
		return existing, nil
	}
	if ce.fineCache == nil {
		ce.fineCache = map[int]*fineIndex{}
	}
	ce.fineCache[coarseIdx] = fi
	return fi, nil
}

func (r *Reader) chunkLocation(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	cx, cy uint32,
) (offset, length uint64, err error) {
	g := ce.rawGrid
	coarseIdx, localIdx := g.CoarseIndexOf(cx, cy)
	fi, err := r.loadFineIndex(entry, hdr, ce, coarseIdx)
	if err != nil {
		return 0, 0, err
	}
	if localIdx < 0 || localIdx >= len(fi.offsets) {
		return 0, 0, fmt.Errorf("fine index: local idx %d out of range [0,%d)", localIdx, len(fi.offsets))
	}
	return fi.offsets[localIdx], fi.lengths[localIdx], nil
}

// payload at (blockOffset + tile_data_offset + chunk_offset).
func (r *Reader) loadChunk(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	idx, cx, cy uint32,
) ([]float32, error) {
	ce.chunkMu.Lock()
	if px, ok := ce.chunkCache[idx]; ok {
		// LRU touch.
		if ce.chunkOrder != nil {
			for e := ce.chunkOrder.Back(); e != nil; e = e.Prev() {
				if e.Value.(uint32) == idx {
					ce.chunkOrder.MoveToBack(e)
					break
				}
			}
		}
		ce.chunkMu.Unlock()
		return px, nil
	}
	ce.chunkMu.Unlock()

	g := ce.rawGrid
	offset, length, err := r.chunkLocation(entry, hdr, ce, cx, cy)
	if err != nil {
		return nil, err
	}
	w := g.ChunkWidth(cx)
	h := g.ChunkHeight(cy)
	n := w * h
	pixels := make([]float32, n)
	if length == 0 {
		// absent chunk: all NoData.
		for i := range pixels {
			pixels[i] = float32(math.NaN())
		}
	} else {
		blob := make([]byte, length)
		abs := entry.BlockOffset + hdr.TileDataOffset + offset
		if _, err := r.src.ReadAt(blob, int64(abs)); err != nil {
			return nil, fmt.Errorf("read raw-grid chunk %d: %w", idx, err)
		}
		params := quantize.Params{
			DType:  quantize.DType(entry.DType),
			Scale:  entry.Scale,
			Offset: entry.Offset,
		}
		dec := r.borrowDecoder()
		if dec == nil {
			return nil, errors.New("raw-grid: codec decoder unavailable")
		}
		err := dec.DecodeToFloat32(blob, params, n, pixels)
		r.returnDecoder(dec)
		if err != nil {
			return nil, fmt.Errorf("decode raw-grid chunk %d: %w", idx, err)
		}
	}

	ce.chunkMu.Lock()
	defer ce.chunkMu.Unlock()
	if existing, ok := ce.chunkCache[idx]; ok {
		return existing, nil
	}
	if ce.chunkCache == nil {
		ce.chunkCache = map[uint32][]float32{}
		ce.chunkOrder = list.New()
	}
	ce.chunkCache[idx] = pixels
	ce.chunkOrder.PushBack(idx)
	ce.chunkTotal++
	for ce.chunkTotal > chunkCacheCap {
		oldest := ce.chunkOrder.Front()
		if oldest == nil {
			break
		}
		ce.chunkOrder.Remove(oldest)
		delete(ce.chunkCache, oldest.Value.(uint32))
		ce.chunkTotal--
	}
	return pixels, nil
}

func (r *Reader) borrowDecoder() *codec.Decoder {
	v := r.decoderPool.Get()
	if err, ok := v.(error); ok {
		_ = err
		return nil
	}
	return v.(*codec.Decoder)
}

func (r *Reader) returnDecoder(d *codec.Decoder) {
	r.decoderPool.Put(d)
}

// decodes every chunk under the bilinear 2x2 neighbourhood of any point.
func (r *Reader) prefetchChunksFor(
	entry format.BlockTableEntry,
	hdr *format.BlockHeader,
	ce *blockCacheEntry,
	points []SamplePoint,
	opt SampleCoalesceOptions,
) error {
	g := ce.rawGrid
	if len(points) == 0 {
		return nil
	}
	cs := g.ChunkSize()

	need := map[uint32]struct{}{}
	for _, p := range points {
		gx := (p.Lon - g.Lon0) / g.DX
		gy := (p.Lat - g.Lat0) / g.DY
		if math.IsNaN(gx) || math.IsNaN(gy) {
			continue
		}
		if gx < 0 || gy < 0 || gx > float64(g.Nx-1) || gy > float64(g.Ny-1) {
			continue
		}
		x0 := int(math.Floor(gx))
		y0 := int(math.Floor(gy))
		x1 := x0 + 1
		y1 := y0 + 1
		if x1 > int(g.Nx)-1 {
			x1 = x0
		}
		if y1 > int(g.Ny)-1 {
			y1 = y0
		}
		for _, x := range [2]int{x0, x1} {
			for _, y := range [2]int{y0, y1} {
				cx := uint32(x / cs)
				cy := uint32(y / cs)
				need[uint32(g.ChunkIndex(cx, cy))] = struct{}{}
			}
		}
	}
	if len(need) == 0 {
		return nil
	}

	ce.chunkMu.Lock()
	for idx := range need {
		if _, ok := ce.chunkCache[idx]; ok {
			delete(need, idx)
		}
	}
	ce.chunkMu.Unlock()
	if len(need) == 0 {
		return nil
	}

	// dedup by coarse cell so each fine-index is fetched at most once.
	coarseSet := map[int]struct{}{}
	for idx := range need {
		cy := idx / g.ChunkCountX
		cx := idx % g.ChunkCountX
		coarseIdx, _ := g.CoarseIndexOf(cx, cy)
		coarseSet[coarseIdx] = struct{}{}
	}
	for coarseIdx := range coarseSet {
		if _, err := r.loadFineIndex(entry, hdr, ce, coarseIdx); err != nil {
			return err
		}
	}

	type chunkRange struct {
		idx     uint32
		off, ln uint64
		cx, cy  uint32
	}
	ranges := make([]chunkRange, 0, len(need))
	for idx := range need {
		cy := idx / g.ChunkCountX
		cx := idx % g.ChunkCountX
		off, ln, err := r.chunkLocation(entry, hdr, ce, cx, cy)
		if err != nil {
			return err
		}
		ranges = append(ranges, chunkRange{
			idx: idx, off: off, ln: ln, cx: cx, cy: cy,
		})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].off < ranges[j].off })

	i := 0
	for i < len(ranges) {
		j := i
		runStart := ranges[i].off
		runEnd := ranges[i].off + ranges[i].ln
		for j+1 < len(ranges) {
			next := ranges[j+1]
			if next.ln == 0 {
				j++
				continue
			}
			gap := next.off - runEnd
			if gap > opt.MaxGapBytes {
				break
			}
			if next.off+next.ln-runStart > opt.MaxRequestBytes {
				break
			}
			runEnd = next.off + next.ln
			j++
		}
		if runEnd > runStart {
			buf := make([]byte, runEnd-runStart)
			abs := entry.BlockOffset + hdr.TileDataOffset + runStart
			if _, err := r.src.ReadAt(buf, int64(abs)); err != nil {
				return fmt.Errorf("coalesced raw-grid read: %w", err)
			}
			for k := i; k <= j; k++ {
				rg := ranges[k]
				if rg.ln == 0 {
					if err := r.cacheChunkPixels(ce, rg.idx, allNaN(g.ChunkWidth(rg.cx)*g.ChunkHeight(rg.cy))); err != nil {
						return err
					}
					continue
				}
				start := rg.off - runStart
				blob := buf[start : start+rg.ln]
				w := g.ChunkWidth(rg.cx)
				h := g.ChunkHeight(rg.cy)
				pixels := make([]float32, w*h)
				params := quantize.Params{
					DType:  quantize.DType(entry.DType),
					Scale:  entry.Scale,
					Offset: entry.Offset,
				}
				dec := r.borrowDecoder()
				if dec == nil {
					return errors.New("raw-grid: codec decoder unavailable")
				}
				err := dec.DecodeToFloat32(blob, params, w*h, pixels)
				r.returnDecoder(dec)
				if err != nil {
					return fmt.Errorf("decode coalesced raw-grid chunk %d: %w", rg.idx, err)
				}
				if err := r.cacheChunkPixels(ce, rg.idx, pixels); err != nil {
					return err
				}
			}
		}
		i = j + 1
	}
	return nil
}

func (r *Reader) cacheChunkPixels(ce *blockCacheEntry, idx uint32, pixels []float32) error {
	ce.chunkMu.Lock()
	defer ce.chunkMu.Unlock()
	if _, ok := ce.chunkCache[idx]; ok {
		return nil
	}
	if ce.chunkCache == nil {
		ce.chunkCache = map[uint32][]float32{}
		ce.chunkOrder = list.New()
	}
	ce.chunkCache[idx] = pixels
	ce.chunkOrder.PushBack(idx)
	ce.chunkTotal++
	for ce.chunkTotal > chunkCacheCap {
		oldest := ce.chunkOrder.Front()
		if oldest == nil {
			break
		}
		ce.chunkOrder.Remove(oldest)
		delete(ce.chunkCache, oldest.Value.(uint32))
		ce.chunkTotal--
	}
	return nil
}

// lets callers dispatch tile-pyramid vs sample APIs without catching ErrRawGridBlock.
func (r *Reader) IsRawGridBlock(variable string, t uint32) (bool, error) {
	id, ok := r.VariableID(variable)
	if !ok {
		return false, ErrUnknownVariable
	}
	entry, err := r.LookupBlock(id, t)
	if err != nil {
		return false, err
	}
	hdr, _, _, err := r.loadBlockHeaderEntry(entry.BlockOffset, entry.BlockLength)
	if err != nil {
		return false, err
	}
	return hdr.BlockFlags&format.BlockFlagRawGrid != 0, nil
}

// ErrNotRawGrid on a tile pyramid.
func (r *Reader) RawGridSection(variable string, t uint32) (*format.RawGridSection, error) {
	id, ok := r.VariableID(variable)
	if !ok {
		return nil, ErrUnknownVariable
	}
	entry, err := r.LookupBlock(id, t)
	if err != nil {
		return nil, err
	}
	hdr, _, ce, err := r.loadBlockHeaderEntry(entry.BlockOffset, entry.BlockLength)
	if err != nil {
		return nil, err
	}
	if hdr.BlockFlags&format.BlockFlagRawGrid == 0 {
		return nil, ErrNotRawGrid
	}
	return ce.rawGrid, nil
}

func isNaN32(f float32) bool { return f != f }

func allNaN(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(math.NaN())
	}
	return out
}
