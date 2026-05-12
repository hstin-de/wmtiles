package encode

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/internal/scan"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/tiler"
)

// File bytes, message ranges and headers are cached on the input so the
// streaming pass doesn't re-read the file or re-parse headers.
func (e *Encoder) scanGRIBFast(in *input, filter map[string]bool, st *scanState) error {
	data, err := os.ReadFile(in.path)
	if err != nil {
		return fmt.Errorf("read grib: %w", err)
	}
	return e.scanGRIBBytes(in, data, filter, st)
}

func (e *Encoder) scanGRIBBytesFast(in *input, filter map[string]bool, st *scanState) error {
	return e.scanGRIBBytes(in, in.data, filter, st)
}

// Falls back to eccodes for jpeg2000 / complex packing / other variants the
// pure-Go simple-packing reader can't see through.
func (e *Encoder) scanGRIBBytes(in *input, data []byte, filter map[string]bool, st *scanState) error {
	ranges, err := parser.MessageRanges(data)
	if err != nil {
		return fmt.Errorf("scan grib ranges: %w", err)
	}
	headers := make([]parser.GribHeader, len(ranges))
	skip := make([]bool, len(ranges))
	scanErrs := make([]error, len(ranges))

	numWorkers := max(runtime.GOMAXPROCS(0), 1)
	idxCh := make(chan int, len(ranges))
	for i := range ranges {
		idxCh <- i
	}
	close(idxCh)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			for i := range idxCh {
				r := ranges[i]
				msg := data[r.Offset : r.Offset+r.Length]
				h, ok, e := parser.FastScanMetadata(msg)
				if e != nil {
					scanErrs[i] = e
					continue
				}
				if !ok {
					h2, e2 := parser.ParseHeaderBytes(msg)
					if e2 != nil {
						scanErrs[i] = e2
						continue
					}
					h = h2
				}
				if filter != nil && !filter[h.ShortName] {
					skip[i] = true
				}
				headers[i] = h
			}
		}()
	}
	wg.Wait()

	for _, e := range scanErrs {
		if e != nil {
			return e
		}
	}

	for i := range headers {
		st.totalSeen++
		if skip[i] {
			continue
		}
		st.keptSeen++
		st.accumulate(&headers[i])
	}

	in.gribRanges = ranges
	in.gribHeaders = headers
	in.gribSkip = skip
	in.gribData = data
	return nil
}

// Per-message worker pool: parallelism wins on FastDecodeRegularLLStats
// since it's pure Go. Tries the fused tile path on uniform grids so we can
// skip the float32 intermediate; falls back per-tile when fused declines.
func (e *Encoder) streamGRIBFast(in *input, bySig map[scan.VarKey]*scan.VarInfo,
	timeIdxByTime map[time.Time]uint32, sink streamSink, pixSize int,
	minZoom, maxZoom uint8) error {

	data := in.gribData
	ranges := in.gribRanges
	headers := in.gribHeaders
	skip := in.gribSkip
	defer func() {
		// Multi-file encodes would otherwise pin every file's raw bytes.
		in.gribData = nil
		in.gribRanges = nil
		in.gribHeaders = nil
		in.gribSkip = nil
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

	numWorkers := max(runtime.GOMAXPROCS(0), 1)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			dw, dwErr := sink.NewDirectWorker()
			if dwErr != nil {
				setErr(dwErr)
				return
			}
			defer dw.Close()
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
				v, ok := bySig[k]
				if !ok {
					continue
				}
				tIdx, ok := timeIdxByTime[h.ReferenceTime]
				if !ok {
					setErr(fmt.Errorf("variable %q time %s: time not in index", v.Name, h.ReferenceTime))
					continue
				}
				vt := vtKey{k: k, tIdx: tIdx}
				declMu.Lock()
				if _, dup := declared[vt]; dup {
					declMu.Unlock()
					if !e.opts.AllowDuplicateMessages {
						setErr(fmt.Errorf("duplicate GRIB message for variable %q time %s in %s",
							v.Name, h.ReferenceTime, in.name))
					}
					continue
				}
				declared[vt] = struct{}{}
				declMu.Unlock()

				if !stats.HasFinite {
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
				precision, src := scan.ResolveBlockPrecision(v, vmin, vmax, e.opts.Precision)
				if v.PrecSources == nil {
					v.PrecSources = map[string]struct{}{}
				}
				v.PrecSources[src] = struct{}{}
				varMu.Unlock()

				if err := sink.DeclareBlock(encoder.BlockSpec{
					Variable:  v.Name,
					TimeStep:  tIdx,
					ValueMin:  vmin,
					ValueMax:  vmax,
					Precision: precision,
				}); err != nil {
					setErr(fmt.Errorf("declare block %q t=%d: %w", v.Name, tIdx, err))
					continue
				}

				gribFile := parser.GRIBFile{Header: *h, DataValues: values}
				s := tiler.NewSampler(&gribFile)
				if s == nil {
					setErr(fmt.Errorf("variable %q time %s: malformed grid", v.Name, h.ReferenceTime))
					continue
				}
				fusedOK := s.Uniform()
				for z := minZoom; z <= maxZoom; z++ {
					for _, c := range tiler.TilesIntersectingGrid(&gribFile, z) {
						if fusedOK {
							ok, fusedErr := dw.SubmitTileFused(v.Name, tIdx, z, c.X, c.Y, pixSize, s)
							if fusedErr != nil {
								if encoder.IsFusedNotSupported(fusedErr) {
									fusedOK = false
								} else {
									setErr(fusedErr)
									continue
								}
							} else {
								if ok {
									e.submitted.Add(1)
								} else {
									e.skipped.Add(1)
								}
								continue
							}
						}
						px := tiler.Tile(s, z, c.X, c.Y, pixSize)
						if px == nil {
							e.skipped.Add(1)
							continue
						}
						t := encoder.Tile{
							Variable: v.Name, TimeStep: tIdx,
							Z: z, X: c.X, Y: c.Y, Pixels: px,
						}
						if subErr := dw.SubmitDirect(t); subErr != nil {
							e.skipped.Add(1)
							continue
						}
						e.submitted.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	return getErr()
}

// Per-message decode buffers are large; recycle them so the heap doesn't
// thrash on multi-thousand-message files.
var valuesPool = sync.Pool{
	New: func() any { b := make([]float32, 0, 1<<20); return &b },
}

func getValuesBuf() []float32 {
	p := valuesPool.Get().(*[]float32)
	return (*p)[:0]
}

func putValuesBuf(b []float32) {
	if cap(b) == 0 {
		return
	}
	b = b[:0]
	valuesPool.Put(&b)
}
