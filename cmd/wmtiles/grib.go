package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tiler"
)

type gribEncodeFlags struct {
	output             string
	minZoom            uint
	maxZoom            uint
	tileSizeLog2       uint
	filterShortNames   string
	precisionOverrides map[string]float64
	cpuProfile         string
	memProfile         string
	traceProfile       string
	disableDelta       bool
	zstdLevel          int
}

func parseGribEncodeFlags(command string, args []string) (gribEncodeFlags, string, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	out := fs.String("o", "", "output .wmt path (required)")
	minZoom := fs.Uint("min-zoom", 0, "minimum zoom level")
	maxZoom := fs.Uint("max-zoom", 5, "maximum zoom level")
	tileSizeLog2 := fs.Uint("tile-size-log2", 8, "tile size as log2 of pixel count (7..10 -> 128..1024)")
	filterShortNames := fs.String("filter", "", "comma-separated list of GRIB shortNames to keep (e.g. 't,u,v'); empty = keep all")
	precisionOverride := fs.String("precision", "", "per variable quantization precision overrides, e.g. '2t=0.25,sp=50' (default: lookup table by shortName/unit, then a 12-bit auto-cap from the observed range; explicit 0 forces full u16)")
	cpuProfile := fs.String("cpuprofile", "", "write CPU profile to file")
	memProfile := fs.String("memprofile", "", "write heap profile to file")
	traceProfile := fs.String("trace", "", "write execution trace to file")
	disableDelta := fs.Bool("disable-delta-codec", false, "force bitshuffle-only encoding; faster but produces larger files (smooth GFS vars can grow ~2×)")
	zstdLevel := fs.Int("zstd-level", 0, "libzstd compression level (1=fastest..22=best); 0 uses the default (3)")
	normalizedArgs := normalizeGribEncodeArgs(args)
	if err := fs.Parse(normalizedArgs); err != nil {
		return gribEncodeFlags{}, "", err
	}
	overrides, err := parsePrecisionOverrides(*precisionOverride)
	if err != nil {
		return gribEncodeFlags{}, "", err
	}
	if fs.NArg() != 1 {
		return gribEncodeFlags{}, "", fmt.Errorf("usage: wmtiles %s <input.grib2> -o <output.wmt> [--max-zoom N]", command)
	}
	if *out == "" {
		return gribEncodeFlags{}, "", fmt.Errorf("missing -o output path")
	}
	if *tileSizeLog2 < 7 || *tileSizeLog2 > 10 {
		return gribEncodeFlags{}, "", fmt.Errorf("tile-size-log2 must be 7..10, got %d", *tileSizeLog2)
	}
	if *maxZoom < *minZoom {
		return gribEncodeFlags{}, "", fmt.Errorf("max-zoom %d < min-zoom %d", *maxZoom, *minZoom)
	}
	return gribEncodeFlags{
		output:             *out,
		minZoom:            *minZoom,
		maxZoom:            *maxZoom,
		tileSizeLog2:       *tileSizeLog2,
		filterShortNames:   *filterShortNames,
		precisionOverrides: overrides,
		cpuProfile:         *cpuProfile,
		memProfile:         *memProfile,
		traceProfile:       *traceProfile,
		disableDelta:       *disableDelta,
		zstdLevel:          *zstdLevel,
	}, fs.Arg(0), nil
}

// stdlib flag pkg requires flags before positionals: this re-orders user input so
// `wmtiles encode foo.grib2 -o out.wmt` works the same as `wmtiles encode -o out.wmt foo.grib2`
func normalizeGribEncodeArgs(args []string) []string {
	flagArgs := make([]string, 0, len(args))
	positionalArgs := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionalArgs = append(positionalArgs, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			if strings.Contains(arg, "=") {
				continue
			}
			if i+1 >= len(args) {
				return flagArgs
			}
			i++
			flagArgs = append(flagArgs, args[i])
			continue
		}
		positionalArgs = append(positionalArgs, arg)
	}
	return append(flagArgs, positionalArgs...)
}

// uniquely identifies a GRIB2 variable: (discipline, category, parameter) is the WMO
// triplet, plus level so e.g. temperature@500hPa and temperature@850hPa stay distinct
type varKey struct {
	d, c, p     int
	levelType   string
	level       int
	bottomLevel int
}

func varKeyOf(h *parser.GribHeader) varKey {
	return varKey{
		d:           h.Discipline,
		c:           h.ParameterCategory,
		p:           h.ParameterNumber,
		levelType:   h.TypeOfLevel,
		level:       h.Level,
		bottomLevel: h.BottomLevel,
	}
}

type varInfo struct {
	name         string
	shortName    string
	unit         string
	vmin         float64
	vmax         float64
	hasFinite    bool
	messageCount int
	times        map[time.Time]struct{}
	precSources  map[string]struct{}
}

type variablePlan struct {
	name      string
	unit      string
	messages  int
	vmin      float64
	vmax      float64
	precision float64
	precSrc   string
	dtype     string
	step      float64
}


type parsedMsg struct {
	header  parser.GribHeader
	values  []float32
	vmin    float64
	vmax    float64
	hasFin  bool
	skipMsg bool
}

// Recycle values buffers across parse → tile → release.
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

// scanHeadersOnly extracts per-message metadata in pure Go (with eccodes
// fallback) so the encoder can be built up front while the parallel
// parse + tile + encode pipeline runs.
func scanHeadersOnly(data []byte, ranges []parser.MessageRange, filterShortNames string) (
	headers []parser.GribHeader,
	skip []bool,
	bySig map[varKey]*varInfo,
	allTimes []time.Time,
	bbox [4]float64,
	totalSeen, keptSeen int,
	err error,
) {
	var keep map[string]bool
	if filterShortNames != "" {
		keep = map[string]bool{}
		for n := range strings.SplitSeq(filterShortNames, ",") {
			if n = strings.TrimSpace(n); n != "" {
				keep[n] = true
			}
		}
	}

	headers = make([]parser.GribHeader, len(ranges))
	skip = make([]bool, len(ranges))
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
					// fall back to eccodes for header read
					h2, e2 := parser.ParseHeaderBytes(msg)
					if e2 != nil {
						scanErrs[i] = e2
						continue
					}
					h = h2
				}
				if keep != nil && !keep[h.ShortName] {
					skip[i] = true
				}
				headers[i] = h
			}
		}()
	}
	wg.Wait()

	for _, e := range scanErrs {
		if e != nil {
			err = e
			return
		}
	}

	bySig = map[varKey]*varInfo{}
	timesSeen := map[time.Time]struct{}{}
	bbox = [4]float64{180, 90, -180, -90}
	bboxInit := false
	for i := range headers {
		totalSeen++
		if skip[i] {
			continue
		}
		keptSeen++
		h := &headers[i]
		k := varKeyOf(h)
		v, ok := bySig[k]
		if !ok {
			base := h.ShortName
			if base == "" || base == "unknown" {
				base = fmt.Sprintf("param_%d_%d_%d", k.d, k.c, k.p)
			}
			v = &varInfo{
				name:        base + levelSuffix(k.levelType, k.level, k.bottomLevel),
				shortName:   h.ShortName,
				unit:        h.Units,
				vmin:        math.Inf(+1),
				vmax:        math.Inf(-1),
				times:       map[time.Time]struct{}{},
				precSources: map[string]struct{}{},
			}
			bySig[k] = v
		}
		v.messageCount++
		v.times[h.ReferenceTime] = struct{}{}

		shell := parser.GRIBFile{Header: *h}
		gw, gs, ge, gn := tiler.GridBBox(&shell)
		if !bboxInit {
			bbox = [4]float64{gw, gs, ge, gn}
			bboxInit = true
		} else {
			if gw < bbox[0] {
				bbox[0] = gw
			}
			if gs < bbox[1] {
				bbox[1] = gs
			}
			if ge > bbox[2] {
				bbox[2] = ge
			}
			if gn > bbox[3] {
				bbox[3] = gn
			}
		}
		timesSeen[h.ReferenceTime] = struct{}{}
	}

	disambiguate := func(suffix func(varKey) string) {
		counts := map[string]int{}
		for _, v := range bySig {
			counts[v.name]++
		}
		for k, v := range bySig {
			if counts[v.name] > 1 {
				v.name += suffix(k)
			}
		}
	}
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%d_%d_%d", k.d, k.c, k.p)
	})
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%s_%d_%d", k.levelType, k.level, k.bottomLevel)
	})

	allTimes = make([]time.Time, 0, len(timesSeen))
	for t := range timesSeen {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i].Before(allTimes[j]) })
	return
}

// streamParseAndTile is the pipelined encode worker pool: each worker
// decodes values, declares the block, samples tiles and submits them.
func streamParseAndTile(
	data []byte,
	ranges []parser.MessageRange,
	headers []parser.GribHeader,
	skip []bool,
	bySig map[varKey]*varInfo,
	timeIdxByTime map[time.Time]uint32,
	overrides map[string]float64,
	sink blockSink,
	minZoom, maxZoom uint8,
	pixSize int,
	onDuplicate func(string, time.Time) error,
) (int64, int64, error) {
	numWorkers := max(runtime.GOMAXPROCS(0), 1)

	var submitted, skipped atomic.Int64
	var firstErr atomic.Value
	setErr := func(e error) {
		if e == nil {
			return
		}
		firstErr.CompareAndSwap(nil, e)
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

	ds, _ := sink.(directSinker)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			var dw *encoder.DirectWorker
			if ds != nil {
				w, e := ds.NewDirectWorker()
				if e != nil {
					setErr(e)
					return
				}
				defer w.Close()
				dw = w
			}
			scratch := getValuesBuf()
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
					vmin, vmax, hasFin := finiteRange(values, gg.Header.MissingValue)
					stats = parser.Stats{Min: vmin, Max: vmax, HasFinite: hasFin}
				}
				h := &g.Header
				if !fastOK {
					headers[idx] = g.Header
				}
				k := varKeyOf(h)
				v, ok := bySig[k]
				if !ok {
					_ = values
					continue
				}
				tIdx, ok := timeIdxByTime[h.ReferenceTime]
				if !ok {
					setErr(fmt.Errorf("variable %q time %s: time not in index", v.name, h.ReferenceTime))
					continue
				}
				vt := vtKey{k, tIdx}
				declMu.Lock()
				if _, dup := declared[vt]; dup {
					declMu.Unlock()
					if onDuplicate != nil {
						if e := onDuplicate(v.name, h.ReferenceTime); e != nil {
							setErr(e)
						}
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
				if vmin < v.vmin {
					v.vmin = vmin
				}
				if vmax > v.vmax {
					v.vmax = vmax
				}
				v.hasFinite = true
				precision, src := resolveBlockPrecision(v, vmin, vmax, overrides)
				if v.precSources == nil {
					v.precSources = map[string]struct{}{}
				}
				v.precSources[src] = struct{}{}
				varMu.Unlock()

				if e := sink.DeclareBlock(encoder.BlockSpec{
					Variable:  v.name,
					TimeStep:  tIdx,
					ValueMin:  vmin,
					ValueMax:  vmax,
					Precision: precision,
				}); e != nil {
					setErr(fmt.Errorf("declare block %q t=%d: %w", v.name, tIdx, e))
					continue
				}

				gribFile := parser.GRIBFile{Header: *h, DataValues: values}
				s := tiler.NewSampler(&gribFile)
				if s == nil {
					setErr(fmt.Errorf("variable %q time %s: malformed grid", v.name, h.ReferenceTime))
					continue
				}
				for z := minZoom; z <= maxZoom; z++ {
					for _, c := range tiler.TilesIntersectingGrid(&gribFile, z) {
						px := tiler.Tile(s, z, c.X, c.Y, pixSize)
						if px == nil {
							skipped.Add(1)
							continue
						}
						t := encoder.Tile{
							Variable: v.name, TimeStep: tIdx,
							Z: z, X: c.X, Y: c.Y, Pixels: px,
						}
						var subErr error
						if dw != nil {
							subErr = dw.SubmitDirect(t)
						} else {
							subErr = sink.Submit(t)
						}
						if subErr != nil {
							skipped.Add(1)
							continue
						}
						submitted.Add(1)
					}
				}
			}
			putValuesBuf(scratch)
		}()
	}
	wg.Wait()
	return submitted.Load(), skipped.Load(), getErr()
}

// parseAllMessages decodes every GRIB2 message in parallel, returning the
// per-message cache plus the merged variable/time catalog and bounding box.
// Filtered-out messages skip the values transfer.
func parseAllMessages(data []byte, ranges []parser.MessageRange, filterShortNames string) (
	parsed []parsedMsg,
	bySig map[varKey]*varInfo,
	allTimes []time.Time,
	bbox [4]float64,
	totalSeen, keptSeen int,
	err error,
) {
	var keep map[string]bool
	if filterShortNames != "" {
		keep = map[string]bool{}
		for n := range strings.SplitSeq(filterShortNames, ",") {
			if n = strings.TrimSpace(n); n != "" {
				keep[n] = true
			}
		}
	}

	parsed = make([]parsedMsg, len(ranges))
	parseErrs := make([]error, len(ranges))

	parseOne := func(i int) {
		r := ranges[i]
		msg := data[r.Offset : r.Offset+r.Length]
		if keep != nil {
			h, e := parser.ParseHeaderBytes(msg)
			if e != nil {
				parseErrs[i] = e
				return
			}
			if !keep[h.ShortName] {
				parsed[i] = parsedMsg{header: h, skipMsg: true}
				return
			}
		}
		g, _, e := parser.ParseMessage(msg, getValuesBuf())
		if e != nil {
			parseErrs[i] = e
			return
		}
		vmin, vmax, hasFin := finiteRange(g.DataValues, g.Header.MissingValue)
		parsed[i] = parsedMsg{
			header: g.Header,
			values: g.DataValues,
			vmin:   vmin,
			vmax:   vmax,
			hasFin: hasFin,
		}
	}
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
				parseOne(i)
			}
		}()
	}
	wg.Wait()

	for _, e := range parseErrs {
		if e != nil {
			err = e
			return
		}
	}

	bySig = map[varKey]*varInfo{}
	timesSeen := map[time.Time]struct{}{}
	bbox = [4]float64{180, 90, -180, -90}
	bboxInit := false

	for i := range parsed {
		totalSeen++
		if parsed[i].skipMsg {
			continue
		}
		keptSeen++
		h := &parsed[i].header
		k := varKeyOf(h)
		v, ok := bySig[k]
		if !ok {
			base := h.ShortName
			if base == "" || base == "unknown" {
				base = fmt.Sprintf("param_%d_%d_%d", k.d, k.c, k.p)
			}
			v = &varInfo{
				name:        base + levelSuffix(k.levelType, k.level, k.bottomLevel),
				shortName:   h.ShortName,
				unit:        h.Units,
				vmin:        math.Inf(+1),
				vmax:        math.Inf(-1),
				times:       map[time.Time]struct{}{},
				precSources: map[string]struct{}{},
			}
			bySig[k] = v
		}
		v.messageCount++
		v.times[h.ReferenceTime] = struct{}{}

		shell := parser.GRIBFile{Header: *h}
		gw, gs, ge, gn := tiler.GridBBox(&shell)
		if !bboxInit {
			bbox = [4]float64{gw, gs, ge, gn}
			bboxInit = true
		} else {
			if gw < bbox[0] {
				bbox[0] = gw
			}
			if gs < bbox[1] {
				bbox[1] = gs
			}
			if ge > bbox[2] {
				bbox[2] = ge
			}
			if gn > bbox[3] {
				bbox[3] = gn
			}
		}
		timesSeen[h.ReferenceTime] = struct{}{}
	}

	disambiguate := func(suffix func(varKey) string) {
		counts := map[string]int{}
		for _, v := range bySig {
			counts[v.name]++
		}
		for k, v := range bySig {
			if counts[v.name] > 1 {
				v.name += suffix(k)
			}
		}
	}
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%d_%d_%d", k.d, k.c, k.p)
	})
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%s_%d_%d", k.levelType, k.level, k.bottomLevel)
	})

	allTimes = make([]time.Time, 0, len(timesSeen))
	for t := range timesSeen {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i].Before(allTimes[j]) })
	return
}

// tileFromParsed declares blocks, samples tiles and submits them in parallel
// using the message cache produced by parseAllMessages. Each message is owned
// by one worker, which clears its values slice as soon as its tiles are queued.
func tileFromParsed(
	parsed []parsedMsg,
	bySig map[varKey]*varInfo,
	timeIdxByTime map[time.Time]uint32,
	overrides map[string]float64,
	sink blockSink,
	minZoom, maxZoom uint8,
	pixSize int,
	onDuplicate func(string, time.Time) error,
) (int64, int64, error) {
	numWorkers := max(runtime.GOMAXPROCS(0), 1)

	var submitted, skipped atomic.Int64
	var firstErr atomic.Value
	setErr := func(e error) {
		if e == nil {
			return
		}
		firstErr.CompareAndSwap(nil, e)
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

	keepIdx := make([]int, 0, len(parsed))
	for i := range parsed {
		if !parsed[i].skipMsg && parsed[i].values != nil {
			keepIdx = append(keepIdx, i)
		}
	}
	idxCh := make(chan int, len(keepIdx))
	for _, i := range keepIdx {
		idxCh <- i
	}
	close(idxCh)

	ds, _ := sink.(directSinker)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			var dw *encoder.DirectWorker
			if ds != nil {
				w, e := ds.NewDirectWorker()
				if e != nil {
					setErr(e)
					return
				}
				defer w.Close()
				dw = w
			}
			for idx := range idxCh {
				if getErr() != nil {
					continue
				}
				m := &parsed[idx]
				k := varKeyOf(&m.header)
				v, ok := bySig[k]
				if !ok {
					continue
				}
				tIdx, ok := timeIdxByTime[m.header.ReferenceTime]
				if !ok {
					setErr(fmt.Errorf("variable %q time %s: time not in index", v.name, m.header.ReferenceTime))
					continue
				}
				vt := vtKey{k, tIdx}
				declMu.Lock()
				if _, dup := declared[vt]; dup {
					declMu.Unlock()
					if onDuplicate != nil {
						if e := onDuplicate(v.name, m.header.ReferenceTime); e != nil {
							setErr(e)
						}
					}
					continue
				}
				declared[vt] = struct{}{}
				declMu.Unlock()

				if !m.hasFin {
					putValuesBuf(m.values)
					m.values = nil
					continue
				}
				varMu.Lock()
				if m.vmin < v.vmin {
					v.vmin = m.vmin
				}
				if m.vmax > v.vmax {
					v.vmax = m.vmax
				}
				v.hasFinite = true
				precision, src := resolveBlockPrecision(v, m.vmin, m.vmax, overrides)
				if v.precSources == nil {
					v.precSources = map[string]struct{}{}
				}
				v.precSources[src] = struct{}{}
				varMu.Unlock()

				if e := sink.DeclareBlock(encoder.BlockSpec{
					Variable:  v.name,
					TimeStep:  tIdx,
					ValueMin:  m.vmin,
					ValueMax:  m.vmax,
					Precision: precision,
				}); e != nil {
					setErr(fmt.Errorf("declare block %q t=%d: %w", v.name, tIdx, e))
					putValuesBuf(m.values)
					m.values = nil
					continue
				}

				gribFile := parser.GRIBFile{Header: m.header, DataValues: m.values}
				s := tiler.NewSampler(&gribFile)
				if s == nil {
					setErr(fmt.Errorf("variable %q time %s: malformed grid", v.name, m.header.ReferenceTime))
					putValuesBuf(m.values)
					m.values = nil
					continue
				}
				for z := minZoom; z <= maxZoom; z++ {
					for _, c := range tiler.TilesIntersectingGrid(&gribFile, z) {
						px := tiler.Tile(s, z, c.X, c.Y, pixSize)
						if px == nil {
							skipped.Add(1)
							continue
						}
						t := encoder.Tile{
							Variable: v.name, TimeStep: tIdx,
							Z: z, X: c.X, Y: c.Y, Pixels: px,
						}
						var subErr error
						if dw != nil {
							subErr = dw.SubmitDirect(t)
						} else {
							subErr = sink.Submit(t)
						}
						if subErr != nil {
							skipped.Add(1)
							continue
						}
						submitted.Add(1)
					}
				}
				putValuesBuf(m.values)
				m.values = nil
			}
		}()
	}
	wg.Wait()
	return submitted.Load(), skipped.Load(), getErr()
}

// buildPreliminaryVariableSpecs leaves Precision=0 for variables without a
// known hint; the per-block auto-cap fills it in during tile encoding.
func buildPreliminaryVariableSpecs(bySig map[varKey]*varInfo, overrides map[string]float64) []encoder.VariableSpec {
	specs := make([]encoder.VariableSpec, 0, len(bySig))
	for _, v := range bySig {
		var precision float64
		if p, ok := overrides[v.name]; ok {
			precision = p
		} else if p, ok := overrides[v.shortName]; ok {
			precision = p
		} else if p := defaultPrecisionFor(v.shortName, v.unit); p > 0 {
			precision = p
		}
		specs = append(specs, encoder.VariableSpec{
			Name:      v.name,
			Unit:      v.unit,
			Precision: precision,
		})
	}
	return specs
}

func finalVariablePlans(bySig map[varKey]*varInfo, overrides map[string]float64) []variablePlan {
	plans := make([]variablePlan, 0, len(bySig))
	for _, v := range bySig {
		if !v.hasFinite {
			continue
		}
		precision, src := resolveBlockPrecision(v, v.vmin, v.vmax, overrides)
		if v.precSources != nil {
			src = dominantPrecSource(v.precSources)
		}
		params := quantize.FitParams(v.vmin, v.vmax, precision)
		plans = append(plans, variablePlan{
			name:      v.name,
			unit:      v.unit,
			messages:  v.messageCount,
			vmin:      v.vmin,
			vmax:      v.vmax,
			precision: precision,
			precSrc:   src,
			dtype:     dtypeName(params.DType),
			step:      params.Scale,
		})
	}
	return plans
}

func dominantPrecSource(sources map[string]struct{}) string {
	for _, candidate := range []string{"override", "auto", "cap", "default"} {
		if _, ok := sources[candidate]; ok {
			return candidate
		}
	}
	return "default"
}

// 10-bit cap on the observed range for variables not in the lookup table; well
// above NWP-grade SNR. precision=0 explicitly set by the user still keeps u16
const autoPrecisionSteps = 1024

type vtKey struct {
	k    varKey
	tIdx uint32
}

type blockSink interface {
	DeclareBlock(encoder.BlockSpec) error
	Submit(encoder.Tile) error
}

// directSinker lets sinks expose a fast path that bypasses the encoder's
// channel pipeline.
type directSinker interface {
	NewDirectWorker() (*encoder.DirectWorker, error)
}

func finiteRange(values []float32, missing float64) (vmin, vmax float64, ok bool) {
	missing32 := float32(missing)
	mn := float32(math.Inf(+1))
	mx := float32(math.Inf(-1))
	hasFin := false
	for _, v := range values {
		if v != v || v == missing32 {
			continue
		}
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		hasFin = true
	}
	if hasFin {
		return float64(mn), float64(mx), true
	}
	return 0, 0, false
}

// resolveBlockPrecision uses the per-block observed range for the auto-cap
// fallback, which can drop cooler blocks from u16 to u8.
func resolveBlockPrecision(v *varInfo, vmin, vmax float64, overrides map[string]float64) (float64, string) {
	if p, ok := overrides[v.name]; ok {
		return p, "override"
	}
	if p, ok := overrides[v.shortName]; ok {
		return p, "override"
	}
	if p := defaultPrecisionFor(v.shortName, v.unit); p > 0 {
		return p, "auto"
	}
	if vmax > vmin {
		return (vmax - vmin) / autoPrecisionSteps, "cap"
	}
	return 0, "default"
}

func runEncodeGRIB(command string, args []string) error {
	flags, in, err := parseGribEncodeFlags(command, args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(in); err != nil {
		return err
	}

	if flags.cpuProfile != "" {
		f, err := os.Create(flags.cpuProfile)
		if err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return fmt.Errorf("start cpu profile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}
	if flags.traceProfile != "" {
		f, err := os.Create(flags.traceProfile)
		if err != nil {
			return fmt.Errorf("trace: %w", err)
		}
		defer f.Close()
		if err := trace.Start(f); err != nil {
			return fmt.Errorf("start trace: %w", err)
		}
		defer trace.Stop()
	}
	if flags.memProfile != "" {
		defer func() {
			f, err := os.Create(flags.memProfile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "memprofile: %v\n", err)
				return
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "write heap profile: %v\n", err)
			}
		}()
	}

	cliSection("WMTiles encode")
	cliKV("input", in)
	cliKV("output", flags.output)
	cliKVf("zoom range", "%d..%d", flags.minZoom, flags.maxZoom)
	cliKVf("tile size", "%d px", 1<<flags.tileSizeLog2)
	if flags.filterShortNames == "" {
		cliKV("filter", "all GRIB shortNames")
	} else {
		cliKV("filter", flags.filterShortNames)
	}

	cliSection("Scan GRIB")
	data, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("read grib: %w", err)
	}
	ranges, err := parser.MessageRanges(data)
	if err != nil {
		return fmt.Errorf("scan grib: %w", err)
	}
	tParse := time.Now()
	headers, skipMsg, bySig, allTimes, bbox, totalSeen, keptSeen, err := scanHeadersOnly(data, ranges, flags.filterShortNames)
	if err != nil {
		return fmt.Errorf("scan headers: %w", err)
	}
	parseDuration := time.Since(tParse)
	if keptSeen == 0 {
		if flags.filterShortNames != "" {
			return fmt.Errorf("filter %q matched no messages (scanned %d)", flags.filterShortNames, totalSeen)
		}
		return fmt.Errorf("no GRIB messages found in %s", in)
	}
	if len(allTimes) == 0 {
		return fmt.Errorf("no forecast timestamps?")
	}

	refTime := allTimes[0]
	timeCatalog := buildTimeCatalog(allTimes)
	timeIdxByTime := make(map[time.Time]uint32, len(allTimes))
	for i, t := range allTimes {
		timeIdxByTime[t] = uint32(i)
	}

	specs := buildPreliminaryVariableSpecs(bySig, flags.precisionOverrides)
	cliKVf("messages", "%s kept of %s", commaInt(int64(keptSeen)), commaInt(int64(totalSeen)))
	cliKVf("variables", "%d", len(specs))
	cliKV("time axis", describeTimeCatalog(timeCatalog))
	cliKV("grid bbox", formatBBox(bbox))

	opts := encoder.Options{
		TilePixelSizeLog2:     uint8(flags.tileSizeLog2),
		MinZoom:               uint8(flags.minZoom),
		MaxZoom:               uint8(flags.maxZoom),
		ReferenceForecastTime: refTime,
		TimeCatalog:           timeCatalog,
		BBox:                  bbox,
		Variables:             specs,
		Metadata: map[string]any{
			"sourceGrib":   in,
			"messageCount": keptSeen,
		},
		OnPixelsConsumed:    tiler.PutTileBuf,
		DisableDeltaCodec:   flags.disableDelta,
		ZstdLevel:           flags.zstdLevel,
		SkipInternalWorkers: true,
	}

	enc, err := encoder.NewStreamingEncoder(opts, flags.output)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}

	pixSize := 1 << flags.tileSizeLog2
	t0 := time.Now()
	submitted, skipped, err := streamParseAndTile(data, ranges, headers, skipMsg,
		bySig, timeIdxByTime, flags.precisionOverrides, enc,
		uint8(flags.minZoom), uint8(flags.maxZoom), pixSize, nil)
	if err != nil {
		enc.Close()
		return fmt.Errorf("tile stream: %w", err)
	}
	encodeDuration := time.Since(t0)
	data = nil
	cliSection("Tile encoding")
	cliKV("tiles written", commaInt(submitted))
	cliKV("tiles skipped", commaInt(skipped))
	cliKV("parse", formatDuration(parseDuration))
	cliKV("tile+encode", formatDuration(encodeDuration))
	cliKV("duration", formatDuration(parseDuration+encodeDuration))
	cliKV("throughput", formatThroughput(submitted, parseDuration+encodeDuration, "tiles"))
	cliKVf("workers", "%d", runtime.GOMAXPROCS(0))

	plans := finalVariablePlans(bySig, flags.precisionOverrides)
	printVariablePlans(plans)

	tFinish := time.Now()
	if err := enc.Finish(); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	cliKV("finish", formatDuration(time.Since(tFinish)))
	st, _ := os.Stat(flags.output)
	cliSection("Done")
	if st != nil {
		cliKV("file", flags.output)
		cliKV("size", humanBytes(st.Size()))
	}
	return nil
}

func printVariablePlans(plans []variablePlan) {
	sort.Slice(plans, func(i, j int) bool { return plans[i].name < plans[j].name })
	rows := make([][]string, 0, len(plans))
	for _, p := range plans {
		rows = append(rows, []string{
			p.name,
			emptyAsNA(p.unit),
			fmt.Sprintf("%d", p.messages),
			formatRange(p.vmin, p.vmax),
			formatFloat(p.precision) + " (" + p.precSrc + ")",
			p.dtype,
			formatFloat(p.step),
		})
	}
	cliSection("Variables")
	cliTable([]string{"name", "unit", "msgs", "range", "precision", "dtype", "step"}, rows)
}

func buildTimeCatalog(times []time.Time) format.TimeCatalog {
	if len(times) == 0 {
		return format.TimeCatalog{Regular: true, Count: 0}
	}
	if len(times) == 1 {
		return format.TimeCatalog{
			Regular:    true,
			StartMs:    times[0].UnixMilli(),
			IntervalMs: 0,
			Count:      1,
		}
	}
	regular := true
	step := times[1].Sub(times[0])
	for i := 2; i < len(times); i++ {
		if times[i].Sub(times[i-1]) != step {
			regular = false
			break
		}
	}
	if regular {
		return format.TimeCatalog{
			Regular:    true,
			StartMs:    times[0].UnixMilli(),
			IntervalMs: step.Milliseconds(),
			Count:      int64(len(times)),
		}
	}
	stamps := make([]int64, len(times))
	for i, t := range times {
		stamps[i] = t.UnixMilli()
	}
	return format.TimeCatalog{
		Regular:      false,
		Count:        int64(len(times)),
		TimestampsMs: stamps,
	}
}

func dtypeName(d quantize.DType) string {
	switch d {
	case quantize.DTypeU8:
		return "u8"
	case quantize.DTypeU16:
		return "u16"
	case quantize.DTypeF32:
		return "f32"
	}
	return fmt.Sprintf("dtype(%d)", uint8(d))
}
