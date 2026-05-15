package encode

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/internal/scan"
	"github.com/hstin-de/wmtiles/parser"
)

// rawGridStreamCtx carries the per-encoder bits the raw-grid streamers need
// without bloating function signatures.
type rawGridStreamCtx struct {
	bySig         map[scan.VarKey]*scan.VarInfo
	timeIdxByTime map[time.Time]uint32
	sink          streamSink
	chunkLog2     uint8
	precisionMap  map[string]float64
	allowDup      bool
	submitted     *atomic.Int64
	skipped       *atomic.Int64
}

// streamRawGridInputs dispatches each input to the GRIB fast path or to the
// sequential iterator, then flushes pending blocks per file to keep peak RAM
// at one file's worth of finalised blocks.
func (e *Encoder) streamRawGridInputs(
	bySig map[scan.VarKey]*scan.VarInfo,
	timeIdxByTime map[time.Time]uint32,
	sink streamSink,
	chunkLog2 uint8,
) error {
	ctx := &rawGridStreamCtx{
		bySig:         bySig,
		timeIdxByTime: timeIdxByTime,
		sink:          sink,
		chunkLog2:     chunkLog2,
		precisionMap:  e.opts.Precision,
		allowDup:      e.opts.AllowDuplicateMessages,
		submitted:     &e.submitted,
		skipped:       &e.skipped,
	}

	for i := range e.inputs {
		in := &e.inputs[i]
		var err error
		if in.format == FormatGRIB2 && in.gribData != nil {
			err = ctx.streamGRIBFast(in)
		} else {
			err = ctx.streamSequential(in)
		}
		if err != nil {
			return fmt.Errorf("wmtiles/encode: raw-grid stream %s: %w", in.name, err)
		}
		if err := sink.FlushPendingBlocks(); err != nil {
			return fmt.Errorf("wmtiles/encode: raw-grid flush after %s: %w", in.name, err)
		}
	}
	return nil
}

// streamGRIBFast mirrors the tile-pyramid fast path but emits one raw-grid
// block per kept message. Parallel message decode is preserved; each worker
// runs its own EncodeRawGridBlock pass (which internally parallelises chunk
// encoding).
func (ctx *rawGridStreamCtx) streamGRIBFast(in *input) error {
	data := in.gribData
	ranges := in.gribRanges
	headers := in.gribHeaders
	skip := in.gribSkip
	defer func() {
		in.gribData = nil
		in.gribRanges = nil
		in.gribHeaders = nil
		in.gribSkip = nil
		if in.gribMmap != nil {
			_ = unix.Munmap(in.gribMmap)
			in.gribMmap = nil
		}
	}()

	keepIdx := make([]int, 0, len(ranges))
	for i := range ranges {
		if !skip[i] {
			keepIdx = append(keepIdx, i)
		}
	}
	idxCh := make(chan int, len(keepIdx))
	for _, i := range keepIdx {
		idxCh <- i
	}
	close(idxCh)

	var firstErr atomic.Value
	setErr := func(err error) {
		if err == nil {
			return
		}
		firstErr.CompareAndSwap(nil, err)
	}
	getErr := func() error {
		if v := firstErr.Load(); v != nil {
			return v.(error)
		}
		return nil
	}
	var declMu sync.Mutex
	declared := map[vtKey]struct{}{}
	var varMu sync.Mutex

	// EncodeRawGridBlock spawns its own chunk-encode pool. Running many
	// outer message workers in parallel with that pool oversubscribes the
	// zstd cgo arena, so keep the outer fanout small.
	outerWorkers := max(min(runtime.GOMAXPROCS(0)/4, 4), 1)
	var wg sync.WaitGroup
	wg.Add(outerWorkers)
	for range outerWorkers {
		go func() {
			defer wg.Done()
			scratch := getValuesBuf()
			defer func() { putValuesBuf(scratch) }()

			for idx := range idxCh {
				if getErr() != nil {
					continue
				}
				r := ranges[idx]
				msg := data[r.Offset : r.Offset+r.Length]

				g, sc, st, fastOK, fastErr := parser.FastDecodeRegularLLStats(msg, scratch[:0])
				if fastErr != nil {
					setErr(fastErr)
					continue
				}
				var values []float32
				var stats parser.Stats
				if fastOK {
					values = sc
					scratch = sc[:0]
					stats = st
				} else {
					gg, _, e := parser.ParseMessage(msg, getValuesBuf())
					if e != nil {
						setErr(e)
						continue
					}
					g = gg
					values = gg.DataValues
					vmin, vmax, hasFin := scan.FiniteRange(values, gg.Header.MissingValue)
					stats = parser.Stats{Min: vmin, Max: vmax, HasFinite: hasFin}
				}
				h := &g.Header
				if !fastOK {
					headers[idx] = g.Header
				}
				k := scan.VarKeyOf(h)
				v, ok := ctx.bySig[k]
				if !ok {
					continue
				}
				tIdx, ok := ctx.timeIdxByTime[h.ReferenceTime]
				if !ok {
					setErr(fmt.Errorf("variable %q time %s: time not in index", v.Name, h.ReferenceTime))
					continue
				}
				vt := vtKey{k: k, tIdx: tIdx}
				declMu.Lock()
				if _, dup := declared[vt]; dup {
					declMu.Unlock()
					if !ctx.allowDup {
						setErr(fmt.Errorf("duplicate GRIB message for variable %q time %s in %s",
							v.Name, h.ReferenceTime, in.name))
					}
					continue
				}
				declared[vt] = struct{}{}
				declMu.Unlock()

				if !stats.HasFinite {
					ctx.skipped.Add(1)
					continue
				}
				vmin, vmax := stats.Min, stats.Max

				varMu.Lock()
				if vmin < v.VMin {
					v.VMin = vmin
				}
				if vmax > v.VMax {
					v.VMax = vmax
				}
				v.HasFinite = true
				precision, src := scan.ResolveBlockPrecision(v, vmin, vmax, ctx.precisionMap)
				if v.PrecSources == nil {
					v.PrecSources = map[string]struct{}{}
				}
				v.PrecSources[src] = struct{}{}
				varMu.Unlock()

				spec := rawGridSpecFromHeader(v.Name, tIdx, h, precision, ctx.chunkLog2)
				if err := ctx.sink.EncodeRawGridBlock(spec, values); err != nil {
					setErr(fmt.Errorf("raw-grid encode %q t=%d: %w", v.Name, tIdx, err))
					continue
				}
				ctx.submitted.Add(1)
			}
		}()
	}
	wg.Wait()
	return getErr()
}

// streamSequential handles HDF5, byte-buffer GRIB, and Array inputs by walking
// the parser's per-message callback once and emitting a raw-grid block per
// kept message.
func (ctx *rawGridStreamCtx) streamSequential(in *input) error {
	declared := map[vtKey]string{}
	return in.forEachMessageFiltered(
		func(h *parser.GribHeader) bool {
			_, ok := ctx.bySig[scan.VarKeyOf(h)]
			return ok
		},
		func(msg parser.GRIBFile) error {
			k := scan.VarKeyOf(&msg.Header)
			v, ok := ctx.bySig[k]
			if !ok {
				return nil
			}
			tIdx, ok := ctx.timeIdxByTime[msg.Header.ReferenceTime]
			if !ok {
				return fmt.Errorf("message time %s not in merged time axis", msg.Header.ReferenceTime)
			}
			vt := vtKey{k: k, tIdx: tIdx}
			if prev, dup := declared[vt]; dup {
				if ctx.allowDup {
					return nil
				}
				return fmt.Errorf("duplicate message for variable %q time %s in %s (already seen in %s)",
					v.Name, msg.Header.ReferenceTime.UTC().Format(time.RFC3339), in.name, prev)
			}

			vmin, vmax, hasFinite := scan.FiniteRange(msg.DataValues, msg.Header.MissingValue)
			if !hasFinite {
				declared[vt] = in.name
				ctx.skipped.Add(1)
				return nil
			}
			if vmin < v.VMin {
				v.VMin = vmin
			}
			if vmax > v.VMax {
				v.VMax = vmax
			}
			v.HasFinite = true

			precision, src := scan.ResolveBlockPrecision(v, vmin, vmax, ctx.precisionMap)
			if v.PrecSources == nil {
				v.PrecSources = map[string]struct{}{}
			}
			v.PrecSources[src] = struct{}{}

			spec := rawGridSpecFromHeader(v.Name, tIdx, &msg.Header, precision, ctx.chunkLog2)
			if err := ctx.sink.EncodeRawGridBlock(spec, msg.DataValues); err != nil {
				return fmt.Errorf("raw-grid encode %q t=%d: %w", v.Name, tIdx, err)
			}
			declared[vt] = in.name
			ctx.submitted.Add(1)
			return nil
		})
}

// rawGridSpecFromHeader maps a parser.GribHeader's grid geometry to a
// RawGridSpec. The parser already builds DistinctLatitudes/Longitudes in
// scan order with antimeridian wraparound normalised, so deriving Lat0/Lon0
// and DY/DX from those is more reliable than from La1/Lo1 raw.
func rawGridSpecFromHeader(name string, tIdx uint32, h *parser.GribHeader, precision float64, chunkLog2 uint8) encoder.RawGridSpec {
	lat0, dy := axisOriginStep(h.DistinctLatitudes, h.La1, h.La2, h.DY)
	lon0, dx := axisOriginStep(h.DistinctLongitudes, h.Lo1, h.Lo2, h.DX)
	missing := h.MissingValue
	if missing == 0 {
		missing = math.NaN()
	}
	return encoder.RawGridSpec{
		Variable:      name,
		TimeStep:      tIdx,
		Nx:            h.Nx,
		Ny:            h.Ny,
		Lat0:          lat0,
		Lon0:          lon0,
		DY:            dy,
		DX:            dx,
		MissingValue:  missing,
		Precision:     precision,
		ChunkSizeLog2: chunkLog2,
	}
}

// axisOriginStep derives the origin coordinate and step for one axis. Prefers
// the parser-computed distinct array (which carries antimeridian wraparound
// and scan-direction normalisation); falls back to first/last header fields
// when the distinct array is absent.
func axisOriginStep(distinct []float64, first, last, step float64) (float64, float64) {
	if len(distinct) >= 2 {
		return distinct[0], distinct[1] - distinct[0]
	}
	if len(distinct) == 1 {
		return distinct[0], step
	}
	if last < first {
		return first, -step
	}
	return first, step
}
