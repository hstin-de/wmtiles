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

func scanGribMetadata(path, filterShortNames string) (
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

	bySig = map[varKey]*varInfo{}
	timesSeen := map[time.Time]struct{}{}
	bbox = [4]float64{180, 90, -180, -90}
	bboxInit := false

	err = parser.ForEachMessageMetaFiltered(path,
		func(sn string) bool {
			totalSeen++
			return keep == nil || keep[sn]
		},
		func(m parser.HeaderInfo) error {
			keptSeen++

			k := varKeyOf(&m.Header)
			v, ok := bySig[k]
			if !ok {
				base := m.Header.ShortName
				if base == "" || base == "unknown" {
					base = fmt.Sprintf("param_%d_%d_%d", k.d, k.c, k.p)
				}
				v = &varInfo{
					name:      base + levelSuffix(k.levelType, k.level, k.bottomLevel),
					shortName: m.Header.ShortName,
					unit:      m.Header.Units,
					vmin:      math.Inf(+1),
					vmax:      math.Inf(-1),
				}
				bySig[k] = v
			}
			v.messageCount++

			if m.HasFinite {
				if m.Min < v.vmin {
					v.vmin = m.Min
				}
				if m.Max > v.vmax {
					v.vmax = m.Max
				}
				v.hasFinite = true
			}

			shell := parser.GRIBFile{Header: m.Header}
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

			timesSeen[m.Header.ReferenceTime] = struct{}{}
			return nil
		})
	if err != nil {
		return
	}

	// disambiguate when shortName + level happen to collide across distinct WMO triplets :
	// rare, but happens with overlapping centre-specific tables
	nameCounts := map[string]int{}
	for _, v := range bySig {
		nameCounts[v.name]++
	}
	for k, v := range bySig {
		if nameCounts[v.name] > 1 {
			v.name = fmt.Sprintf("%s_%d_%d_%d", v.name, k.d, k.c, k.p)
		}
	}

	allTimes = make([]time.Time, 0, len(timesSeen))
	for t := range timesSeen {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i].Before(allTimes[j]) })
	return
}

func buildVariableSpecs(bySig map[varKey]*varInfo, overrides map[string]float64) ([]encoder.VariableSpec, []variablePlan, error) {
	specs := make([]encoder.VariableSpec, 0, len(bySig))
	plans := make([]variablePlan, 0, len(bySig))
	for _, v := range bySig {
		plan, err := variablePlanFor(v, overrides)
		if err != nil {
			return nil, nil, err
		}
		specs = append(specs, encoder.VariableSpec{
			Name:      v.name,
			Unit:      v.unit,
			Precision: plan.precision,
		})
		plans = append(plans, plan)
	}
	return specs, plans, nil
}

// 10-bit cap on the observed range for variables not in the lookup table; well
// above NWP-grade SNR. precision=0 explicitly set by the user still keeps u16
const autoPrecisionSteps = 1024

// resolvePrecision picks the quantisation precision for a variable in one of
// three ways, in priority order: user override, hardcoded shortName/unit
// lookup, or an auto-cap derived from the observed range. The auto-cap fires
// only when nothing else applies; an explicit override of 0 still means "use
// full u16 for whatever range you observe".
func resolvePrecision(v *varInfo, overrides map[string]float64) (float64, string) {
	if p, ok := overrides[v.name]; ok {
		return p, "override"
	}
	if p, ok := overrides[v.shortName]; ok {
		return p, "override"
	}
	if p := defaultPrecisionFor(v.shortName, v.unit); p > 0 {
		return p, "auto"
	}
	if v.vmax > v.vmin {
		return (v.vmax - v.vmin) / autoPrecisionSteps, "cap"
	}
	return 0, "default"
}

func variablePlanFor(v *varInfo, overrides map[string]float64) (variablePlan, error) {
	if !v.hasFinite {
		return variablePlan{}, fmt.Errorf("variable %q has no finite values; refuse to encode", v.name)
	}

	precision, precSrc := resolvePrecision(v, overrides)
	params := quantize.FitParams(v.vmin, v.vmax, precision)
	return variablePlan{
		name:      v.name,
		unit:      v.unit,
		messages:  v.messageCount,
		vmin:      v.vmin,
		vmax:      v.vmax,
		precision: precision,
		precSrc:   precSrc,
		dtype:     dtypeName(params.DType),
		step:      params.Scale,
	}, nil
}

type tileWork struct {
	name string
	tIdx uint32
	z    uint8
	x, y uint32
	s    *tiler.Sampler
}

type vtKey struct {
	k    varKey
	tIdx uint32
}

func streamTilesIntoEncoder(
	path string,
	bySig map[varKey]*varInfo,
	timeIdxByTime map[time.Time]uint32,
	enc *encoder.StreamingEncoder,
	minZoom, maxZoom uint8,
	pixSize int,
) (int64, int64, error) {
	numWorkers := max(runtime.GOMAXPROCS(0), 1)
	workCh := make(chan tileWork, numWorkers*4)
	var submitted, skipped atomic.Int64

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			for w := range workCh {
				px := tiler.Tile(w.s, w.z, w.x, w.y, pixSize)
				if px == nil {
					skipped.Add(1)
					continue
				}
				if err := enc.Submit(encoder.Tile{
					Variable: w.name, TimeStep: w.tIdx,
					Z: w.z, X: w.x, Y: w.y, Pixels: px,
				}); err != nil {
					skipped.Add(1)
					continue
				}
				submitted.Add(1)
			}
		}()
	}

	seen := map[vtKey]struct{}{}
	err := parser.ForEachMessageFiltered(path,
		func(h *parser.GribHeader) bool {
			_, ok := bySig[varKeyOf(h)]
			return ok
		},
		func(g parser.GRIBFile) error {
			k := varKeyOf(&g.Header)
			v, ok := bySig[k]
			if !ok {
				return nil
			}
			tIdx, ok := timeIdxByTime[g.Header.ReferenceTime]
			if !ok {
				return fmt.Errorf("variable %q time %s: time not in index", v.name, g.Header.ReferenceTime)
			}
			vt := vtKey{k, tIdx}
			if _, dup := seen[vt]; dup {
				return nil
			}
			seen[vt] = struct{}{}
			gc := g
			s := tiler.NewSampler(&gc)
			if s == nil {
				return fmt.Errorf("variable %q time %s: malformed grid", v.name, g.Header.ReferenceTime)
			}
			for z := minZoom; z <= maxZoom; z++ {
				for _, c := range tiler.TilesIntersectingGrid(&gc, z) {
					workCh <- tileWork{
						name: v.name, tIdx: tIdx,
						z: z, x: c.X, y: c.Y, s: s,
					}
				}
			}
			return nil
		})

	close(workCh)
	wg.Wait()
	return submitted.Load(), skipped.Load(), err
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
	bySig, allTimes, bbox, totalSeen, keptSeen, err := scanGribMetadata(in, flags.filterShortNames)
	if err != nil {
		return fmt.Errorf("read grib: %w", err)
	}
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

	specs, plans, err := buildVariableSpecs(bySig, flags.precisionOverrides)
	if err != nil {
		return err
	}
	cliKVf("messages", "%s kept of %s", commaInt(int64(keptSeen)), commaInt(int64(totalSeen)))
	cliKVf("variables", "%d", len(specs))
	cliKV("time axis", describeTimeCatalog(timeCatalog))
	cliKV("grid bbox", formatBBox(bbox))
	printVariablePlans(plans)

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
		OnPixelsConsumed:  tiler.PutTileBuf,
		DisableDeltaCodec: flags.disableDelta,
	}

	enc, err := encoder.NewStreamingEncoder(opts, flags.output)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}

	for _, v := range bySig {
		if !v.hasFinite {
			continue
		}
		precision, _ := resolvePrecision(v, flags.precisionOverrides)
		for _, idx := range timeIdxByTime {
			if err := enc.DeclareBlock(encoder.BlockSpec{
				Variable: v.name, TimeStep: idx,
				ValueMin: v.vmin, ValueMax: v.vmax,
				Precision: precision,
			}); err != nil {
				enc.Close()
				return fmt.Errorf("declare block %q t=%d: %w", v.name, idx, err)
			}
		}
	}

	pixSize := 1 << flags.tileSizeLog2
	t0 := time.Now()
	submitted, skipped, err := streamTilesIntoEncoder(in, bySig, timeIdxByTime, enc,
		uint8(flags.minZoom), uint8(flags.maxZoom), pixSize)
	if err != nil {
		enc.Close()
		return fmt.Errorf("tile stream: %w", err)
	}
	encodeDuration := time.Since(t0)
	cliSection("Tile encoding")
	cliKV("tiles written", commaInt(submitted))
	cliKV("tiles skipped", commaInt(skipped))
	cliKV("duration", formatDuration(encodeDuration))
	cliKV("throughput", formatThroughput(submitted, encodeDuration, "tiles"))
	cliKVf("workers", "%d", runtime.GOMAXPROCS(0))

	if err := enc.Finish(); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
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
