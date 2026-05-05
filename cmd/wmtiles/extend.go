package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/reader"
	"github.com/hstin-de/wmtiles/tileid"
	"github.com/hstin-de/wmtiles/tiler"
)

func runExtend(args []string) error {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	filterShortNames := fs.String("filter", "", "comma-separated list of GRIB shortNames to keep (default: all)")
	allowReplace := fs.Bool("allow-replace", false, "overwrite existing (variable, time) blocks instead of erroring")
	precisionOverride := fs.String("precision", "", "per variable quantization precision overrides")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: wmtiles extend <file.wmt> <input.grib2>")
	}
	wmtPath := fs.Arg(0)
	gribPath := fs.Arg(1)
	overrides, err := parsePrecisionOverrides(*precisionOverride)
	if err != nil {
		return err
	}

	if _, err := os.Stat(wmtPath); err != nil {
		return err
	}
	if _, err := os.Stat(gribPath); err != nil {
		return err
	}

	cliSection("WMTiles extend")
	cliKV("file", wmtPath)
	cliKV("input", gribPath)
	if *filterShortNames == "" {
		cliKV("filter", "all GRIB shortNames")
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

	cliSection("Scan GRIB")
	bySig, allTimes, _, totalSeen, keptSeen, err := scanGribMetadata(gribPath, *filterShortNames)
	if err != nil {
		return fmt.Errorf("scan GRIB: %w", err)
	}
	if keptSeen == 0 {
		if *filterShortNames != "" {
			return fmt.Errorf("filter %q matched no messages (scanned %d)", *filterShortNames, totalSeen)
		}
		return fmt.Errorf("no GRIB messages found in %s", gribPath)
	}
	if len(allTimes) == 0 {
		return fmt.Errorf("no forecast timestamps?")
	}
	timeCatalog := buildTimeCatalog(allTimes)
	cliKVf("messages", "%s kept of %s", commaInt(int64(keptSeen)), commaInt(int64(totalSeen)))
	cliKVf("variables", "%d", len(bySig))
	cliKV("time axis", describeTimeCatalog(timeCatalog))

	ctx, err := encoder.OpenForAppend(wmtPath, encoder.AppendOptions{AllowReplace: *allowReplace})
	if err != nil {
		return fmt.Errorf("open for append: %w", err)
	}

	for _, v := range bySig {
		precision := pickPrecision(v, overrides)
		if _, err := ctx.RegisterVariable(encoder.VariableSpec{
			Name: v.name, Unit: v.unit, Precision: precision,
		}); err != nil {
			ctx.Close()
			return fmt.Errorf("register variable %q: %w", v.name, err)
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

	plans := make([]variablePlan, 0, len(bySig))
	declared := 0
	skipped := 0
	for _, v := range bySig {
		if !v.hasFinite {
			continue
		}
		if plan, err := variablePlanFor(v, overrides); err == nil {
			plans = append(plans, plan)
		}
		precision := pickPrecision(v, overrides)
		for _, idx := range timeIdxByTime {
			err := ctx.DeclareBlock(encoder.BlockSpec{
				Variable: v.name, TimeStep: idx,
				ValueMin: v.vmin, ValueMax: v.vmax,
				Precision: precision,
			})
			if err != nil {
				// fragile: DeclareBlock doesn't expose a typed sentinel for this case yet
				if strings.Contains(err.Error(), "already exists") {
					skipped++
					continue
				}
				ctx.Close()
				return fmt.Errorf("declare block %q t=%d: %w", v.name, idx, err)
			}
			declared++
		}
	}
	printVariablePlans(plans)
	cliSection("Append plan")
	cliKVf("new blocks", "%d", declared)
	cliKVf("already present", "%d", skipped)

	t0 := time.Now()
	submitted, dropped, err := streamGribTilesIntoAppend(gribPath, bySig, timeIdxByTime, ctx, minZoom, maxZoom, pixelSize)
	if err != nil {
		ctx.Close()
		return fmt.Errorf("stream: %w", err)
	}
	encodeDuration := time.Since(t0)
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

func pickPrecision(v *varInfo, overrides map[string]float64) float64 {
	if p, ok := overrides[v.name]; ok {
		return p
	}
	if p, ok := overrides[v.shortName]; ok {
		return p
	}
	return defaultPrecisionFor(v.shortName, v.unit)
}

func streamGribTilesIntoAppend(
	path string,
	bySig map[varKey]*varInfo,
	timeIdxByTime map[time.Time]uint32,
	ctx *encoder.AppendCtx,
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
				if err := ctx.Submit(encoder.Tile{
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
	err := parser.ForEachMessage(path, func(g parser.GRIBFile) error {
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
