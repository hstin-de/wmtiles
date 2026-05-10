package encoder

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/zeebo/xxh3"
)

// hashQuantInto writes xxh3-128 of quant into the first 16 bytes of key;
// trailing bytes stay zero so the [32]byte dedup-map key shape doesn't
// churn when the hash is swapped.
func hashQuantInto(quant []byte, key *[32]byte) {
	h := xxh3.Hash128(quant)
	binary.LittleEndian.PutUint64(key[0:8], h.Lo)
	binary.LittleEndian.PutUint64(key[8:16], h.Hi)
}

type VariableSpec struct {
	Name         string
	Unit         string
	ColormapHint string
	Precision    float64
}

type Tile struct {
	Variable string
	TimeStep uint32
	Z        uint8
	X, Y     uint32
	Pixels   []float32
}

type BlockSpec struct {
	Variable  string
	TimeStep  uint32
	ValueMin  float64
	ValueMax  float64
	Precision float64
}

type Options struct {
	TilePixelSizeLog2 uint8
	MinZoom           uint8
	MaxZoom           uint8

	ReferenceForecastTime time.Time

	TimeCatalog format.TimeCatalog

	BBox [4]float64

	Variables []VariableSpec

	Metadata map[string]any

	ZstdLevel int

	InternalCompression format.InternalCompression

	CreationTime time.Time

	// invoked by encoder workers once a tile's pixel slice has been quantized
	// and is no longer needed. lets callers return slices to a sync.Pool so
	// large encodes don't churn 256 KB allocations per tile through the GC
	OnPixelsConsumed func([]float32)

	// forces every tile through bitshuffle+zstd, skipping delta/lorenzo. set
	// when encode wall time matters more than file size
	DisableDeltaCodec bool

	// EnableTileDict turns on the per-block zstd dictionary pass at finishBlock.
	// The dict is stored at the end of each block and signalled via
	// BlockFlagHasDict so readers route through the dict-aware decode path.
	EnableTileDict bool

	// SkipInternalWorkers disables the channel-based quantize+codec pool;
	// callers must drive the encoder via DirectWorker.SubmitDirect.
	SkipInternalWorkers bool
}

func defaults(o *Options) {
	if o.TilePixelSizeLog2 == 0 {
		o.TilePixelSizeLog2 = 8
	}
	if o.InternalCompression == 0 {
		o.InternalCompression = format.CompZstd
	}
	if o.ZstdLevel == 0 {
		o.ZstdLevel = 3
	}
}

type dedupVal struct {
	offset uint64
	length uint32
}

type recordVal struct {
	tid    uint64
	offset uint64
	length uint32
}

type encodedTile struct {
	block *blockBuilder
	tid   uint64
	key   [32]byte
	// no-dict mode: blob is the post-zstd payload (tag/inner unused).
	// dict mode: inner holds raw quantized bytes with tag = codec ID (blob nil).
	blob  []byte
	tag   byte
	inner []byte
}

func fitParamsFor(vmin, vmax, precision float64) quantize.Params {
	return quantize.FitParams(vmin, vmax, precision)
}

func Encode(tiles []Tile, opts Options, outPath string) error {
	defaults(&opts)
	pixelSize := 1 << opts.TilePixelSizeLog2
	pixPerTile := pixelSize * pixelSize

	type blockKey struct {
		variable string
		timeStep uint32
	}
	type blockRange struct {
		vmin, vmax float64
		count      int
	}
	ranges := map[blockKey]*blockRange{}
	knownVar := map[string]bool{}
	for _, v := range opts.Variables {
		knownVar[v.Name] = true
	}

	for ti := range tiles {
		t := &tiles[ti]
		if !knownVar[t.Variable] {
			return fmt.Errorf("tile references unknown variable %q", t.Variable)
		}
		if len(t.Pixels) != pixPerTile {
			return fmt.Errorf("tile %s/%d/(%d,%d,%d): pixel count %d, want %d",
				t.Variable, t.TimeStep, t.Z, t.X, t.Y, len(t.Pixels), pixPerTile)
		}
		k := blockKey{t.Variable, t.TimeStep}
		r, ok := ranges[k]
		if !ok {
			r = &blockRange{vmin: math.Inf(+1), vmax: math.Inf(-1)}
			ranges[k] = r
		}
		r.count++
		for _, v := range t.Pixels {
			fv := float64(v)
			if fv != fv {
				continue
			}
			if fv < r.vmin {
				r.vmin = fv
			}
			if fv > r.vmax {
				r.vmax = fv
			}
		}
	}

	enc, err := NewStreamingEncoder(opts, outPath)
	if err != nil {
		return err
	}

	keys := make([]blockKey, 0, len(ranges))
	for k := range ranges {
		keys = append(keys, k)
	}
	varOrder := map[string]int{}
	for i, v := range opts.Variables {
		varOrder[v.Name] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		if a, b := varOrder[keys[i].variable], varOrder[keys[j].variable]; a != b {
			return a < b
		}
		return keys[i].timeStep < keys[j].timeStep
	})

	for _, k := range keys {
		r := ranges[k]
		if r.vmin > r.vmax {
			enc.Close()
			return fmt.Errorf("block %s/t=%d has only NaN tiles", k.variable, k.timeStep)
		}
		var precision float64
		for _, v := range opts.Variables {
			if v.Name == k.variable {
				precision = v.Precision
				break
			}
		}
		if err := enc.DeclareBlock(BlockSpec{
			Variable: k.variable, TimeStep: k.timeStep,
			ValueMin: r.vmin, ValueMax: r.vmax,
			Precision: precision,
		}); err != nil {
			enc.Close()
			return err
		}
	}

	type addrKey struct {
		variable string
		t        uint32
		z        uint8
		x, y     uint32
	}
	seen := make(map[addrKey]struct{}, len(tiles))
	for ti := range tiles {
		t := &tiles[ti]
		k := addrKey{t.Variable, t.TimeStep, t.Z, t.X, t.Y}
		if _, dup := seen[k]; dup {
			enc.Close()
			return fmt.Errorf("duplicate tile %s/%d/(%d,%d,%d)",
				t.Variable, t.TimeStep, t.Z, t.X, t.Y)
		}
		seen[k] = struct{}{}
		if err := enc.Submit(*t); err != nil {
			enc.Close()
			return err
		}
	}
	return enc.Finish()
}
