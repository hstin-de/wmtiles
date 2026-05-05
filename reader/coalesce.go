package reader

import (
	"fmt"
	"math"
	"sort"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tileid"
)

type TileCoord struct {
	Z    uint8
	X, Y uint32
}

type CoalesceOptions struct {
	MaxGapBytes     uint64
	MaxRequestBytes uint64
}

func (o CoalesceOptions) withDefaults() CoalesceOptions {
	if o.MaxGapBytes == 0 {
		o.MaxGapBytes = 64 * 1024 // ~one TCP window: paying for unused bytes is cheaper than a second RTT
	}
	if o.MaxRequestBytes == 0 {
		o.MaxRequestBytes = 4 * 1024 * 1024 // CDN-friendly cap
	}
	return o
}

func (r *Reader) ReadTilesInBlock(variable string, timeID uint32, coords []TileCoord, outs [][]float32, opts CoalesceOptions) error {
	if len(coords) != len(outs) {
		return fmt.Errorf("ReadTilesInBlock: coords/outs length mismatch (%d vs %d)", len(coords), len(outs))
	}
	if len(coords) == 0 {
		return nil
	}
	opts = opts.withDefaults()

	id, ok := r.VariableID(variable)
	if !ok {
		return ErrUnknownVariable
	}
	blk, err := r.LookupBlock(id, timeID)
	if err != nil {
		for i := range outs {
			fillNaN(outs[i])
		}
		return nil
	}
	hdr, root, err := r.loadBlockHeader(blk.BlockOffset, blk.BlockLength)
	if err != nil {
		return err
	}

	type rangeFetch struct {
		origIdx int
		fileOff uint64
		length  uint32
	}
	tuples := make([]rangeFetch, 0, len(coords))
	missing := make([]bool, len(coords))

	for i, c := range coords {
		n := uint32(1) << c.Z
		if c.Z < r.Header.MinZoom || c.Z > r.Header.MaxZoom || c.X >= n || c.Y >= n {
			missing[i] = true
			continue
		}
		tid := tileid.Encode3D(c.Z, c.X, c.Y)
		entry, found := directory.FindTile(root, tid)
		if !found {
			missing[i] = true
			continue
		}
		if entry.IsLeafPointer() {
			leaf, err := r.loadBlockLeaf(blk.BlockOffset, hdr, entry.Offset, entry.Length)
			if err != nil {
				return err
			}
			entry, found = directory.FindTile(leaf, tid)
			if !found || entry.IsLeafPointer() {
				missing[i] = true
				continue
			}
		}
		tuples = append(tuples, rangeFetch{
			origIdx: i,
			fileOff: blk.BlockOffset + hdr.TileDataOffset + entry.Offset,
			length:  entry.Length,
		})
	}
	for i, miss := range missing {
		if miss {
			fillNaN(outs[i])
		}
	}
	if len(tuples) == 0 {
		return nil
	}

	// sort by file offset, then greedy-merge into groups bounded by gap and total size :
	// each group becomes one HTTP Range request
	sort.Slice(tuples, func(i, j int) bool { return tuples[i].fileOff < tuples[j].fileOff })

	type group struct {
		start, end uint64
		members    []*rangeFetch
	}
	groups := []group{}
	for i := range tuples {
		t := &tuples[i]
		end := t.fileOff + uint64(t.length)
		if len(groups) > 0 {
			last := &groups[len(groups)-1]
			gap := uint64(0)
			if t.fileOff > last.end {
				gap = t.fileOff - last.end
			}
			newEnd := end
			if last.end > newEnd {
				newEnd = last.end
			}
			if gap <= opts.MaxGapBytes && newEnd-last.start <= opts.MaxRequestBytes {
				if newEnd > last.end {
					last.end = newEnd
				}
				last.members = append(last.members, t)
				continue
			}
		}
		groups = append(groups, group{start: t.fileOff, end: end, members: []*rangeFetch{t}})
	}

	dec := r.decoderPool.Get()
	if err, ok := dec.(error); ok {
		return err
	}
	d := dec.(*codec.Decoder)
	defer r.decoderPool.Put(d)

	stride := quantize.DType(blk.DType).Bytes()
	tileBytes := make([]byte, r.PixelCount()*stride)
	p := quantize.Params{DType: quantize.DType(blk.DType), Scale: blk.Scale, Offset: blk.Offset}

	for _, g := range groups {
		buf := make([]byte, g.end-g.start)
		if _, err := r.src.ReadAt(buf, int64(g.start)); err != nil {
			return fmt.Errorf("coalesced range %d-%d: %w", g.start, g.end, err)
		}
		for _, m := range g.members {
			localOff := m.fileOff - g.start
			blob := buf[localOff : localOff+uint64(m.length)]
			if err := d.Decode(blob, p, r.PixelCount(), tileBytes); err != nil {
				return fmt.Errorf("decode tile (z=%d,x=%d,y=%d): %w",
					coords[m.origIdx].Z, coords[m.origIdx].X, coords[m.origIdx].Y, err)
			}
			if len(outs[m.origIdx]) < r.PixelCount() {
				return fmt.Errorf("outs[%d] too small (%d < %d)", m.origIdx, len(outs[m.origIdx]), r.PixelCount())
			}
			quantize.Decode(tileBytes, p, outs[m.origIdx])
		}
	}
	return nil
}

func fillNaN(out []float32) {
	nan := float32(math.NaN())
	for i := range out {
		out[i] = nan
	}
}
