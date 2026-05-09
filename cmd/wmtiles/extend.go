package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/reader"
	"github.com/hstin-de/wmtiles/tileid"
)

func runExtend(args []string) error {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	filterShortNames := fs.String("filter", "", "comma-separated list of shortNames to keep (default: all)")
	allowReplace := fs.Bool("allow-replace", false, "overwrite existing (variable, time) blocks instead of erroring")
	precisionOverride := fs.String("precision", "", "per variable quantization precision overrides")
	formatOverride := fs.String("format", "", "override input format (grib2|hdf5); default = auto-detect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: wmtiles extend <file.wmt> <input.{grib2|h5}> [--format grib2|hdf5]")
	}
	wmtPath := fs.Arg(0)
	srcPath := fs.Arg(1)
	overrides, err := parsePrecisionOverrides(*precisionOverride)
	if err != nil {
		return err
	}

	if _, err := os.Stat(wmtPath); err != nil {
		return err
	}
	if _, err := os.Stat(srcPath); err != nil {
		return err
	}

	srcFormat := srcFormatGRIB2
	if *formatOverride != "" {
		switch strings.ToLower(*formatOverride) {
		case "hdf5":
			srcFormat = srcFormatHDF5
		case "grib2":
			srcFormat = srcFormatGRIB2
		default:
			return fmt.Errorf("--format must be grib2 or hdf5, got %q", *formatOverride)
		}
	} else if parser.IsHDF5File(srcPath) {
		srcFormat = srcFormatHDF5
	}

	cliSection("WMTiles extend")
	cliKV("file", wmtPath)
	cliKV("input", srcPath)
	cliKV("source format", srcFormat.String())
	if *filterShortNames == "" {
		cliKV("filter", "all shortNames")
	} else {
		cliKV("filter", *filterShortNames)
	}
	cliKV("replace blocks", boolWord(*allowReplace))

	r, err := reader.Open(wmtPath)
	if err != nil {
		return fmt.Errorf("open existing file: %w", err)
	}
	pixelSize := 1 << r.Header.TilePixelSizeLog2
	minZoom := r.Header.MinZoom
	maxZoom := r.Header.MaxZoom
	cliKVf("current gen", "%d", r.Header.SnapshotGeneration)
	cliKVf("zoom range", "%d..%d", minZoom, maxZoom)
	cliKVf("tile size", "%d px", pixelSize)
	r.Close()

	cliSection("Scan source")
	var (
		parsedMsgs           []parsedMsg
		bySig                map[varKey]*varInfo
		allTimes             []time.Time
		totalSeen, keptSeen  int
	)
	switch srcFormat {
	case srcFormatHDF5:
		parsedMsgs, bySig, allTimes, _, totalSeen, keptSeen, err = parseAllHDF5Messages(srcPath, *filterShortNames)
		if err != nil {
			return fmt.Errorf("scan HDF5: %w", err)
		}
	default:
		gribData, gerr := os.ReadFile(srcPath)
		if gerr != nil {
			return fmt.Errorf("read GRIB: %w", gerr)
		}
		gribRanges, gerr := parser.MessageRanges(gribData)
		if gerr != nil {
			return fmt.Errorf("scan GRIB: %w", gerr)
		}
		parsedMsgs, bySig, allTimes, _, totalSeen, keptSeen, err = parseAllMessages(gribData, gribRanges, *filterShortNames)
		if err != nil {
			return fmt.Errorf("scan GRIB: %w", err)
		}
	}
	if keptSeen == 0 {
		if *filterShortNames != "" {
			return fmt.Errorf("filter %q matched no messages (scanned %d)", *filterShortNames, totalSeen)
		}
		return fmt.Errorf("no records found in %s", srcPath)
	}
	if len(allTimes) == 0 {
		return fmt.Errorf("no forecast timestamps?")
	}
	timeCatalog := buildTimeCatalog(allTimes)
	cliKVf("messages", "%s kept of %s", commaInt(int64(keptSeen)), commaInt(int64(totalSeen)))
	cliKVf("variables", "%d", len(bySig))
	cliKV("time axis", describeTimeCatalog(timeCatalog))

	ctx, err := encoder.OpenForAppend(wmtPath, encoder.AppendOptions{AllowReplace: *allowReplace, SkipInternalWorkers: true})
	if err != nil {
		return fmt.Errorf("open for append: %w", err)
	}

	specs := buildPreliminaryVariableSpecs(bySig, overrides)
	for _, s := range specs {
		if _, err := ctx.RegisterVariable(s); err != nil {
			ctx.Close()
			return fmt.Errorf("register variable %q: %w", s.Name, err)
		}
	}

	timeIdxByTime := make(map[time.Time]uint32, len(allTimes))
	for _, t := range allTimes {
		idx, err := ctx.RegisterTimeStep(t.UnixMilli())
		if err != nil {
			ctx.Close()
			return fmt.Errorf("register time %s: %w", t, err)
		}
		timeIdxByTime[t] = idx
	}

	var alreadyPresent atomic.Int64
	onDup := func(name string, t time.Time) error {
		alreadyPresent.Add(1)
		return nil
	}

	t0 := time.Now()
	submitted, dropped, err := tileFromParsed(parsedMsgs, bySig, timeIdxByTime,
		overrides, &appendSink{ctx: ctx, alreadyPresent: &alreadyPresent},
		minZoom, maxZoom, pixelSize, onDup)
	if err != nil {
		ctx.Close()
		return fmt.Errorf("stream: %w", err)
	}
	encodeDuration := time.Since(t0)

	plans := finalVariablePlans(bySig, overrides)
	printVariablePlans(plans)
	cliSection("Append plan")
	cliKVf("already present", "%d", alreadyPresent.Load())

	cliSection("Tile encoding")
	cliKV("tiles written", commaInt(submitted))
	cliKV("tiles skipped", commaInt(dropped))
	cliKV("duration", formatDuration(encodeDuration))
	cliKV("throughput", formatThroughput(submitted, encodeDuration, "tiles"))
	cliKVf("workers", "%d", runtime.GOMAXPROCS(0))

	if err := ctx.Finish(); err != nil {
		return fmt.Errorf("commit append: %w", err)
	}
	st, _ := os.Stat(wmtPath)
	cliSection("Done")
	if st != nil {
		cliKV("file", wmtPath)
		cliKV("size", humanBytes(st.Size()))
	}
	return nil
}

// appendSink adapts AppendCtx to blockSink. "already exists" collisions are
// counted, not propagated — extend reports them in the summary.
type appendSink struct {
	ctx            *encoder.AppendCtx
	alreadyPresent *atomic.Int64
}

func (a *appendSink) DeclareBlock(spec encoder.BlockSpec) error {
	err := a.ctx.DeclareBlock(spec)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		a.alreadyPresent.Add(1)
		return nil
	}
	return err
}

func (a *appendSink) Submit(t encoder.Tile) error {
	return a.ctx.Submit(t)
}

// NewDirectWorker satisfies the directSinker fast path so tileFromParsed can
// run quantize+codec inline on parser goroutines.
func (a *appendSink) NewDirectWorker() (*encoder.DirectWorker, error) {
	return a.ctx.NewDirectAppendWorker()
}

func runCompact(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: wmtiles compact <input.wmt> <output.wmt>")
	}
	in, out := args[0], args[1]
	if _, err := os.Stat(in); err != nil {
		return err
	}

	cliSection("WMTiles compact")
	cliKV("input", in)
	cliKV("output", out)

	r, err := reader.Open(in)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer r.Close()
	cliKVf("generation", "%d", r.Header.SnapshotGeneration)
	cliKVf("variables", "%d", len(r.Snapshot.Variables))
	cliKVf("blocks", "%d", r.Snapshot.Header.NumBlocks)

	specs := make([]encoder.VariableSpec, len(r.Snapshot.Variables))
	for i, v := range r.Snapshot.Variables {
		specs[i] = encoder.VariableSpec{
			Name: v.Name, Unit: v.Unit, ColormapHint: v.ColormapHint,
			Precision: v.DefaultPrecisionHint,
		}
	}

	bbox := [4]float64{
		float64(r.Header.BBoxLonMinE7) / 1e7,
		float64(r.Header.BBoxLatMinE7) / 1e7,
		float64(r.Header.BBoxLonMaxE7) / 1e7,
		float64(r.Header.BBoxLatMaxE7) / 1e7,
	}
	opts := encoder.Options{
		TilePixelSizeLog2:     r.Header.TilePixelSizeLog2,
		MinZoom:               r.Header.MinZoom,
		MaxZoom:               r.Header.MaxZoom,
		ReferenceForecastTime: time.UnixMilli(r.Snapshot.Header.ReferenceTimeMs).UTC(),
		TimeCatalog:           r.Snapshot.TimeCat,
		BBox:                  bbox,
		Variables:             specs,
		Metadata:              r.Snapshot.Metadata,
	}
	enc, err := encoder.NewStreamingEncoder(opts, out)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}

	type rowEntry struct {
		varID   uint16
		varName string
		timeID  uint32
		entry   format.BlockTableEntry
	}
	rows := []rowEntry{}
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		name := r.Snapshot.Variables[e.VariableID].Name
		rows = append(rows, rowEntry{
			varID: e.VariableID, varName: name, timeID: e.TimeID, entry: e,
		})
		return nil
	}); err != nil {
		enc.Close()
		return err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].varID != rows[j].varID {
			return rows[i].varID < rows[j].varID
		}
		return rows[i].timeID < rows[j].timeID
	})

	declared := 0
	tilesCopied := 0
	for _, row := range rows {
		e := row.entry
		precision := r.Snapshot.Variables[row.varID].DefaultPrecisionHint
		if err := enc.DeclareBlock(encoder.BlockSpec{
			Variable: row.varName, TimeStep: row.timeID,
			ValueMin: e.ValueMin, ValueMax: e.ValueMax,
			Precision: precision,
		}); err != nil {
			enc.Close()
			return err
		}
		declared++

		buf := make([]float32, r.PixelCount())
		if err := r.EachTileInBlock(e, func(tid uint64, _ directory.Entry) error {
			z, x, y := tileid.Decode3D(r.Header.MaxZoom, tid)
			if err := r.ReadTile(row.varName, row.timeID, z, x, y, buf); err != nil {
				return fmt.Errorf("decode tile %s/(z=%d,x=%d,y=%d): %w", row.varName, z, x, y, err)
			}
			pixCopy := make([]float32, len(buf))
			copy(pixCopy, buf)
			tilesCopied++
			return enc.Submit(encoder.Tile{
				Variable: row.varName, TimeStep: row.timeID,
				Z: z, X: x, Y: y, Pixels: pixCopy,
			})
		}); err != nil {
			enc.Close()
			return err
		}
	}

	if err := enc.Finish(); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	st, _ := os.Stat(out)
	stIn, _ := os.Stat(in)
	cliSection("Done")
	if st != nil && stIn != nil {
		cliKV("input size", humanBytes(stIn.Size()))
		cliKV("output size", humanBytes(st.Size()))
		cliKVf("blocks copied", "%d", declared)
		cliKV("tiles copied", commaInt(int64(tilesCopied)))
		cliKV("file", out)
	}
	return nil
}
