package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/internal/scan"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/reader"
	"github.com/hstin-de/wmtiles/tileid"
	"github.com/hstin-de/wmtiles/tiler"
)

type encodeSrcFormat int

const (
	srcFormatGRIB2 encodeSrcFormat = iota
	srcFormatHDF5
)

func (f encodeSrcFormat) String() string {
	switch f {
	case srcFormatHDF5:
		return "hdf5"
	default:
		return "grib2"
	}
}

func runCompare(c *compareCmd) error {
	if _, err := os.Stat(c.Source); err != nil {
		return err
	}

	srcFmtEnc, err := resolveInputFormat(c.Format, c.Source)
	if err != nil {
		return err
	}
	srcFormat := srcFormatGRIB2
	if srcFmtEnc == "hdf5" {
		srcFormat = srcFormatHDF5
	}

	r, err := reader.Open(c.File)
	if err != nil {
		return fmt.Errorf("open wmt: %w", err)
	}
	defer r.Close()

	wantNames := selectVariables(r, c.Variable)
	if len(wantNames) == 0 {
		return fmt.Errorf("no variables match --variable=%q", c.Variable)
	}

	timeToIdx := buildTimeToIdx(r)

	ui.Banner("compare", fmt.Sprintf("%s vs %s", c.Source, c.File))

	ui.Section("Settings")
	ui.KV("source format", srcFormat.String())
	if c.Variable == "" {
		ui.KV("variables", fmt.Sprintf("%d selected", len(wantNames)))
	} else {
		ui.KV("variable", c.Variable)
	}
	if c.Zoom >= 0 {
		ui.KVf("zoom", "%d", c.Zoom)
	} else {
		ui.KV("zoom", "all")
	}
	if c.Tolerance > 0 {
		ui.KV("tolerance", formatFloat(c.Tolerance))
	} else {
		ui.KV("tolerance", "per block scale/2 plus f32 slack")
	}

	ui.Section("Compare")
	scanPhase := ui.StartPhase("scan source", 0)
	samplers, err := buildSamplersForVariables(c.Source, srcFormat, wantNames, timeToIdx)
	if err != nil {
		scanPhase.Done("failed")
		return err
	}
	matched := matchedAny(samplers)
	scanPhase.Done(fmt.Sprintf("%d variables matched", matched))
	reportSamplerCoverage(samplers, wantNames, r)
	if matched == 0 {
		return fmt.Errorf("no .wmt variables found in source; nothing to compare")
	}

	totalAddressed := uint64(0)
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		totalAddressed += e.NumAddressedTiles
		return nil
	}); err != nil {
		return err
	}

	stats := initStats(r, c.Tolerance)

	comparePhase := ui.StartPhase("compare tiles", int64(totalAddressed))
	startTime := time.Now()
	enqueued, err := runCompareWorkers(r, samplers, stats, c.Zoom)
	if err != nil {
		comparePhase.Done("failed")
		return err
	}
	compareDuration := time.Since(startTime)
	comparePhase.SetCurrent(enqueued)
	comparePhase.Done(fmt.Sprintf("%s tiles  %s", commaInt(enqueued), formatTileRateString(enqueued, compareDuration)))

	if anyFail := printCompareResults(stats); anyFail {
		ui.Section("Result")
		ui.Summary([][2]string{
			{"status", ui.styled("FAIL", ansiRed, ansiBold)},
			{"reason", "at least one variable exceeded tolerance or had NaN mismatches"},
		})
		os.Exit(1)
	}
	ui.Section("Result")
	ui.Summary([][2]string{
		{"status", ui.styled("ok", ansiGreen, ansiBold)},
	})
	return nil
}

func selectVariables(r *reader.Reader, filter string) map[string]bool {
	out := map[string]bool{}
	for _, v := range r.Snapshot.Variables {
		if filter == "" || v.Name == filter {
			out[v.Name] = true
		}
	}
	return out
}

func buildTimeToIdx(r *reader.Reader) map[int64]uint32 {
	tc := r.Snapshot.TimeCat
	out := make(map[int64]uint32, tc.Count)
	if tc.Regular {
		for i := int64(0); i < tc.Count; i++ {
			out[tc.StartMs+i*tc.IntervalMs] = uint32(i)
		}
	} else {
		for i, ts := range tc.TimestampsMs {
			out[ts] = uint32(i)
		}
	}
	return out
}

func reportSamplerCoverage(samplers map[string]map[uint32]*tiler.Sampler, wantNames map[string]bool, r *reader.Reader) {
	missing := []string{}
	matched := 0
	for name := range wantNames {
		if len(samplers[name]) > 0 {
			matched++
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	cliKVf("matched vars", "%d", matched)
	cliKVf("unmatched vars", "%d", len(missing))
	if len(missing) > 0 && len(missing) <= 10 {
		cliKV("unmatched", strings.Join(missing, ", "))
	} else if len(missing) > 10 {
		cliKV("unmatched", strings.Join(missing[:10], ", ")+" ...")
	}
	for _, name := range sortedKeys(samplers) {
		if got, want := len(samplers[name]), int(r.Snapshot.TimeCat.Count); got < want {
			fmt.Printf("  warning: variable %q covers %d/%d time steps in GRIB; missing steps will be reported as tilesMissed\n",
				name, got, want)
		}
	}
}

func matchedAny(samplers map[string]map[uint32]*tiler.Sampler) int {
	n := 0
	for _, m := range samplers {
		if len(m) > 0 {
			n++
		}
	}
	return n
}

type accum struct {
	comparedFinite int64
	nanMatch       int64
	nanWmtOnly     int64
	nanGribOnly    int64
	withinTol      int64
	sumDiff        float64
	sumSqDiff      float64
	maxDiff        float64
	maxDiffPos     string
	tilesScanned   int
	tilesMissed    int
	tolerance      float64
}

func (a *accum) merge(o *accum) {
	a.comparedFinite += o.comparedFinite
	a.nanMatch += o.nanMatch
	a.nanWmtOnly += o.nanWmtOnly
	a.nanGribOnly += o.nanGribOnly
	a.withinTol += o.withinTol
	a.sumDiff += o.sumDiff
	a.sumSqDiff += o.sumSqDiff
	a.tilesScanned += o.tilesScanned
	a.tilesMissed += o.tilesMissed
	if o.maxDiff > a.maxDiff {
		a.maxDiff = o.maxDiff
		a.maxDiffPos = o.maxDiffPos
	}
}

func initStats(r *reader.Reader, tolOverride float64) map[string]*accum {
	stats := map[string]*accum{}
	for _, v := range r.Snapshot.Variables {
		stats[v.Name] = &accum{tolerance: tolOverride}
	}
	if tolOverride <= 0 {
		_ = r.EachBlock(func(e format.BlockTableEntry) error {
			name := r.Snapshot.Variables[e.VariableID].Name
			s := stats[name]
			tol := math.Abs(e.Scale) / 2
			tol += f32StorageSlack(e.ValueMin, e.ValueMax)
			if tol > s.tolerance {
				s.tolerance = tol
			}
			return nil
		})
	}
	return stats
}

type compareJob struct {
	tid    uint64
	varID  uint16
	timeID uint32
	z      uint8
	x, y   uint32
}

func runCompareWorkers(r *reader.Reader, samplers map[string]map[uint32]*tiler.Sampler,
	stats map[string]*accum, zoomFilter int) (int64, error) {
	pixSize := 1 << r.Header.TilePixelSizeLog2
	pixCount := r.PixelCount()

	totalAddressed := int64(0)
	_ = r.EachBlock(func(e format.BlockTableEntry) error {
		totalAddressed += int64(e.NumAddressedTiles)
		return nil
	})

	numWorkers := runtime.GOMAXPROCS(0)
	jobs := make(chan compareJob, numWorkers*4)
	var processed, enqueued int64

	progressDone := startProgress(totalAddressed, &processed)

	var statsMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			pixels := make([]float32, pixCount)
			lats := make([]float64, pixSize)
			lons := make([]float64, pixSize)

			for j := range jobs {
				name := r.Snapshot.Variables[j.varID].Name
				tile := compareTile(r, j, samplers[name], stats[name].tolerance, pixels, lats, lons)
				if tile != nil {
					statsMu.Lock()
					stats[name].merge(tile)
					statsMu.Unlock()
				}
				atomic.AddInt64(&processed, 1)
			}
		}()
	}

	err := r.EachBlock(func(e format.BlockTableEntry) error {
		return r.EachTileInBlock(e, func(tid uint64, _ directory.Entry) error {
			z, x, y := tileid.Decode3D(r.Header.MaxZoom, tid)
			if zoomFilter >= 0 && int(z) != zoomFilter {
				return nil
			}
			atomic.AddInt64(&enqueued, 1)
			jobs <- compareJob{tid: tid, varID: e.VariableID, timeID: e.TimeID, z: z, x: x, y: y}
			return nil
		})
	})
	close(jobs)
	wg.Wait()
	close(progressDone)
	if err != nil {
		return 0, err
	}
	return enqueued, nil
}

func compareTile(r *reader.Reader, j compareJob, samplerByT map[uint32]*tiler.Sampler,
	tolerance float64, pixels []float32, lats, lons []float64) *accum {
	if samplerByT == nil {
		return &accum{tilesMissed: 1}
	}
	s := samplerByT[j.timeID]
	if s == nil {
		return &accum{tilesMissed: 1}
	}

	name := r.Snapshot.Variables[j.varID].Name
	if err := r.ReadTile(name, j.timeID, j.z, j.x, j.y, pixels); err != nil {
		fmt.Fprintf(os.Stderr, "\nworker error: decode tile %s/(z=%d,x=%d,y=%d): %v\n",
			name, j.z, j.x, j.y, err)
		return nil
	}

	pixSize := len(lats)
	tiler.TileLats(j.z, j.y, pixSize, lats)
	tiler.TileLons(j.z, j.x, pixSize, lons)

	acc := &accum{tilesScanned: 1, tolerance: tolerance}
	for row := range pixSize {
		lat := lats[row]
		rowOff := row * pixSize
		for col := range pixSize {
			expected := s.At(lat, lons[col])
			actual := float64(pixels[rowOff+col])

			expNaN := math.IsNaN(expected)
			actNaN := math.IsNaN(actual)
			switch {
			case expNaN && actNaN:
				acc.nanMatch++
				continue
			case expNaN && !actNaN:
				acc.nanGribOnly++
				continue
			case !expNaN && actNaN:
				acc.nanWmtOnly++
				continue
			}

			diff := math.Abs(actual - expected)
			acc.comparedFinite++
			acc.sumDiff += diff
			acc.sumSqDiff += diff * diff
			if diff <= tolerance {
				acc.withinTol++
			}
			if diff > acc.maxDiff {
				acc.maxDiff = diff
				acc.maxDiffPos = fmt.Sprintf("%s tile (z=%d,x=%d,y=%d) px (col=%d,row=%d) lat=%.4f lon=%.4f exp=%.6f got=%.6f",
					name, j.z, j.x, j.y, col, row, lat, lons[col], expected, actual)
			}
		}
	}
	return acc
}

func startProgress(total int64, processed *int64) chan struct{} {
	stop := make(chan struct{})
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return stop
	}
	startTime := time.Now()
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				done := atomic.LoadInt64(processed)
				rate := float64(done) / time.Since(startTime).Seconds()
				fmt.Fprintf(os.Stderr, "\r  %d/%d tiles (%.0f tiles/s)", done, total, rate)
			case <-stop:
				fmt.Fprint(os.Stderr, "\r\033[K")
				return
			}
		}
	}()
	return stop
}

func printCompareResults(stats map[string]*accum) bool {
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)

	var (
		totalCompared, totalNaNMatch, totalNaNMismatch, totalWithin int64
		totalSumDiff, totalSumSqDiff, globalMax                     float64
		globalMaxPos                                                string
		anyFail                                                     bool
	)

	cliSection("Per-variable fidelity")
	fmt.Fprintf(cliOut, "  %-32s %12s %10s %10s %10s %10s %10s %s\n",
		"variable", "pixels", "max diff", "mean diff", "rmse", "tol", "within", "status")

	for _, name := range names {
		acc := stats[name]
		var meanDiff, rmse, withinPct float64
		if acc.comparedFinite > 0 {
			meanDiff = acc.sumDiff / float64(acc.comparedFinite)
			rmse = math.Sqrt(acc.sumSqDiff / float64(acc.comparedFinite))
			withinPct = 100 * float64(acc.withinTol) / float64(acc.comparedFinite)
		}
		fail := acc.nanWmtOnly > 0 || acc.nanGribOnly > 0 || acc.maxDiff > acc.tolerance
		failStr := "ok"
		if fail {
			failStr = "FAIL"
			anyFail = true
		}
		fmt.Fprintf(cliOut, "  %-32s %12d %10.4g %10.4g %10.4g %10.4g %9.4f%% %s\n",
			truncName(name, 32), acc.comparedFinite,
			acc.maxDiff, meanDiff, rmse, acc.tolerance, withinPct, failStr)
		if acc.nanMatch > 0 || acc.nanWmtOnly > 0 || acc.nanGribOnly > 0 {
			fmt.Fprintf(cliOut, "    NaN: match=%d wmt only=%d grib only=%d\n",
				acc.nanMatch, acc.nanWmtOnly, acc.nanGribOnly)
		}
		if acc.tilesMissed > 0 {
			fmt.Fprintf(cliOut, "    %d tiles skipped (no matching GRIB message)\n", acc.tilesMissed)
		}
		if acc.maxDiff > acc.tolerance && acc.maxDiffPos != "" {
			fmt.Fprintf(cliOut, "    worst pixel: %s\n", acc.maxDiffPos)
		}

		totalCompared += acc.comparedFinite
		totalNaNMatch += acc.nanMatch
		totalNaNMismatch += acc.nanWmtOnly + acc.nanGribOnly
		totalWithin += acc.withinTol
		totalSumDiff += acc.sumDiff
		totalSumSqDiff += acc.sumSqDiff
		if acc.maxDiff > globalMax {
			globalMax = acc.maxDiff
			globalMaxPos = acc.maxDiffPos
		}
	}

	cliSection("Overall")
	if totalCompared > 0 {
		mean := totalSumDiff / float64(totalCompared)
		rmse := math.Sqrt(totalSumSqDiff / float64(totalCompared))
		within := 100 * float64(totalWithin) / float64(totalCompared)
		cliKV("finite pixels", commaInt(totalCompared))
		cliKV("NaN match", commaInt(totalNaNMatch))
		cliKV("NaN mismatch", commaInt(totalNaNMismatch))
		cliKVf("max abs diff", "%.6g", globalMax)
		cliKVf("mean abs diff", "%.6g", mean)
		cliKVf("RMSE", "%.6g", rmse)
		cliKVf("within tolerance", "%.4f%%", within)
		if globalMaxPos != "" {
			cliKV("worst pixel", globalMaxPos)
		}
	} else {
		cliKV("finite pixels", "0")
	}
	return anyFail
}

func buildSamplersForVariables(path string, format encodeSrcFormat,
	want map[string]bool, timeToIdx map[int64]uint32) (map[string]map[uint32]*tiler.Sampler, error) {
	out := map[string]map[uint32]*tiler.Sampler{}
	headerName := func(h *parser.GribHeader) string {
		base := h.ShortName
		if base == "" || base == "unknown" {
			base = fmt.Sprintf("param_%d_%d_%d", h.Discipline, h.ParameterCategory, h.ParameterNumber)
		}
		return base + scan.LevelSuffix(h.TypeOfLevel, h.Level, h.BottomLevel)
	}
	keep := func(h *parser.GribHeader) bool {
		if !want[headerName(h)] {
			return false
		}
		_, ok := timeToIdx[h.ReferenceTime.UnixMilli()]
		return ok
	}
	consume := func(g parser.GRIBFile) error {
		name := headerName(&g.Header)
		tIdx, ok := timeToIdx[g.Header.ReferenceTime.UnixMilli()]
		if !ok {
			return nil
		}
		if out[name] == nil {
			out[name] = map[uint32]*tiler.Sampler{}
		}
		if _, exists := out[name][tIdx]; exists {
			return nil
		}
		gc := g
		s := tiler.NewSampler(&gc)
		if s == nil {
			return fmt.Errorf("variable %q: malformed grid", name)
		}
		out[name][tIdx] = s
		return nil
	}
	var err error
	switch format {
	case srcFormatHDF5:
		err = parser.ForEachHDF5MessageFiltered(path, keep, consume)
	default:
		err = parser.ForEachMessageFiltered(path, keep, consume)
	}
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", format, err)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// extra tolerance to absorb f32 round-trip rounding: the wmt side stores values
// as float32, and ULP grows with magnitude so we scale the slack by the largest |value|
func f32StorageSlack(vmin, vmax float64) float64 {
	edge := math.Max(math.Abs(vmin), math.Abs(vmax))
	if edge == 0 {
		edge = 1
	}
	const f32Eps = 1.1920929e-7
	return 2 * edge * f32Eps
}

func truncName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}


var _ = quantize.MaxAbsError
