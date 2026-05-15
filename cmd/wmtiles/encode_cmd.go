package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/internal/scan"
)

// Bare files and glob matches bypass the extension filter so users keep
// full control; the filter only narrows directory expansion.
func expandEncodeInputs(args []string, extensions []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(args))
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	matchExt := func(name string) bool {
		low := strings.ToLower(name)
		for _, ext := range extensions {
			if strings.HasSuffix(low, ext) {
				return true
			}
		}
		return false
	}
	for _, raw := range args {
		st, err := os.Stat(raw)
		if err == nil && st.IsDir() {
			entries, err := os.ReadDir(raw)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if !matchExt(e.Name()) {
					continue
				}
				add(filepath.Join(raw, e.Name()))
			}
			continue
		}
		if matches, err := filepath.Glob(raw); err == nil && len(matches) > 0 {
			for _, m := range matches {
				add(m)
			}
			continue
		}
		add(raw)
	}
	sort.Strings(out)
	return out, nil
}

var (
	gribExtensions = []string{".grib2", ".grib", ".grb2", ".grb"}
	hdf5Extensions = []string{".h5", ".hdf5", "-hd5", ".nc", ".nc4"}
)

func runEncode(c *encodeCmd) error {
	if len(c.Inputs) == 0 {
		return fmt.Errorf("no inputs supplied")
	}
	var forced encode.Format
	switch c.Format {
	case "auto":
		forced = ""
	case "grib2":
		forced = encode.FormatGRIB2
	case "hdf5":
		forced = encode.FormatHDF5
	}

	exts := append(append([]string{}, gribExtensions...), hdf5Extensions...)
	if forced == encode.FormatGRIB2 {
		exts = gribExtensions
	} else if forced == encode.FormatHDF5 {
		exts = hdf5Extensions
	}
	paths, err := expandEncodeInputs(c.Inputs, exts)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no inputs found")
	}

	type inputFile struct {
		path   string
		format encode.Format
	}
	inputs := make([]inputFile, 0, len(paths))
	var gribCount, hdf5Count int
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return err
		}
		f := forced
		if f == "" {
			detected, err := detectFormatFromPath(p)
			if err != nil {
				return err
			}
			f = detected
		}
		switch f {
		case encode.FormatGRIB2:
			gribCount++
		case encode.FormatHDF5:
			hdf5Count++
		}
		inputs = append(inputs, inputFile{path: p, format: f})
	}

	var formatLabel string
	switch {
	case gribCount > 0 && hdf5Count > 0:
		formatLabel = fmt.Sprintf("mixed (%d grib2, %d hdf5)", gribCount, hdf5Count)
	case hdf5Count > 0:
		formatLabel = "hdf5"
	default:
		formatLabel = "grib2"
	}

	overrides, err := scan.ParsePrecisionOverrides(c.Precision)
	if err != nil {
		return err
	}

	if c.CPUProfile != "" {
		f, err := os.Create(c.CPUProfile)
		if err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return fmt.Errorf("start cpu profile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}
	if c.Trace != "" {
		f, err := os.Create(c.Trace)
		if err != nil {
			return fmt.Errorf("trace: %w", err)
		}
		defer f.Close()
		if err := trace.Start(f); err != nil {
			return fmt.Errorf("start trace: %w", err)
		}
		defer trace.Stop()
	}
	if c.MemProfile != "" {
		defer func() {
			f, err := os.Create(c.MemProfile)
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

	banner := fmt.Sprintf("%d files -> %s   %d cores", len(inputs), c.Output, runtime.GOMAXPROCS(0))
	if len(inputs) == 1 {
		banner = fmt.Sprintf("%s -> %s   %d cores", inputs[0].path, c.Output, runtime.GOMAXPROCS(0))
	}
	ui.Banner("encode", banner)

	ui.Section("Settings")
	ui.KV("source format", formatLabel)
	if c.NoTiles {
		ui.KV("mode", "raw-grid (no tile pyramid)")
		ui.KVf("raw chunk size", "%d px", 1<<c.RawChunkSizeLog2)
	} else {
		ui.KVf("zoom range", "%d..%d", c.MinZoom, c.MaxZoom)
		ui.KVf("tile size", "%d px", 1<<c.TileSizeLog2)
	}
	if c.Filter == "" {
		ui.KV("filter", "all source variables")
	} else {
		ui.KV("filter", c.Filter)
	}
	ui.KVf("delta codec", "%s", boolWord(!c.DisableDeltaCodec))
	if c.ZstdLevel > 0 {
		ui.KVf("zstd level", "%d", c.ZstdLevel)
	}
	if c.TileDict {
		ui.KV("tile dict", "on")
	}
	if len(inputs) > 1 {
		ui.KVf("inputs", "%d files", len(inputs))
		ui.KV("first", inputs[0].path)
		ui.KV("last", inputs[len(inputs)-1].path)
	}

	ui.Section("Encode")

	var (
		scanPhase, tilePhase, compressPhase, writePhase, snapPhase *Phase
		bytesWritten                                               atomic.Uint64
		scanStart, scanEnd, tileStart, tileEnd                     atomic.Int64
		scannedFiles                                               atomic.Int64
		finalPlans                                                 []encode.VariablePlan
	)
	var enc *encode.Encoder

	endTilePhase := func() {
		if tilePhase == nil {
			return
		}
		sub, _ := enc.Progress()
		tilePhase.SetCurrent(sub)
		start := time.Unix(0, tileStart.Load())
		dur := time.Since(start)
		tileEnd.Store(time.Now().UnixNano())
		tilePhase.Done(fmt.Sprintf("%s tiles  %s", commaInt(sub), formatTileRateString(sub, dur)))
		tilePhase = nil
	}

	opts := encode.Options{
		TileSize:             1 << c.TileSizeLog2,
		MinZoom:              c.MinZoom,
		MaxZoom:              c.MaxZoom,
		NoTiles:              c.NoTiles,
		RawGridChunkSizeLog2: c.RawChunkSizeLog2,
		Precision:            overrides,
		Metadata:             map[string]any{"sourceFormat": formatLabel, "sourceCount": len(inputs)},
		DisableDeltaCodec:    c.DisableDeltaCodec,
		ZstdLevel:            c.ZstdLevel,
		EnableTileDict:       c.TileDict,
		OnInputScanned: func(name string, records int) {
			if scanPhase == nil {
				scanStart.Store(time.Now().UnixNano())
				total := int64(0)
				if len(inputs) > 1 {
					total = int64(len(inputs))
				}
				scanPhase = ui.StartPhase("scan inputs", total)
			}
			n := scannedFiles.Add(1)
			if len(inputs) > 1 {
				scanPhase.SetCurrent(n)
				scanPhase.SetExtra(filepath.Base(name))
			} else {
				scanPhase.SetExtra(fmt.Sprintf("%s messages", commaInt(int64(records))))
			}
		},
		OnScanComplete: func(stats encode.ScanStats) {
			scanEnd.Store(time.Now().UnixNano())
			if scanPhase != nil {
				if len(inputs) > 1 {
					scanPhase.Done(fmt.Sprintf("%d files, %s msgs kept", int(scannedFiles.Load()), commaInt(int64(stats.KeptMessages))))
				} else {
					scanPhase.Done(fmt.Sprintf("%s msgs kept of %s", commaInt(int64(stats.KeptMessages)), commaInt(int64(stats.TotalMessages))))
				}
				scanPhase = nil
			}
			ui.KVf("variables", "%d", stats.VariableCount)
			ui.KV("time axis", describeTimeCatalog(stats.TimeAxis))
			ui.KV("grid bbox", formatBBox(stats.BBox))
			tileStart.Store(time.Now().UnixNano())
			tilePhase = ui.StartPhase("decode + tile", stats.ExpectedTiles)
		},
		OnFinishStats: func(plans []encode.VariablePlan) {
			finalPlans = plans
		},
		OnPhase: func(stage string) {
			switch stage {
			case "compress_blocks":
				endTilePhase()
				compressPhase = ui.StartPhase("compress blocks", 0)
			case "write_blocks":
				if compressPhase != nil {
					compressPhase.Done("")
					compressPhase = nil
				}
				writePhase = ui.StartPhase("write blocks", 0)
			case "write_snapshot":
				if writePhase != nil {
					writePhase.Done(humanBytes(int64(bytesWritten.Load())) + " on disk")
					writePhase = nil
				}
				snapPhase = ui.StartPhase("write snapshot", 0)
			}
		},
		OnBlockCompressed: func(idx, total int, bytes uint64) {
			if compressPhase != nil {
				compressPhase.SetTotal(int64(total))
				compressPhase.AddCurrent(1)
			}
		},
		OnBlockWritten: func(idx, total int, bytes uint64) {
			if writePhase != nil {
				writePhase.SetTotal(int64(total))
				writePhase.AddCurrent(1)
				n := bytesWritten.Add(bytes)
				writePhase.SetExtra(humanBytes(int64(n)))
			}
		},
	}
	if c.Filter != "" {
		for n := range strings.SplitSeq(c.Filter, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				opts.FilterVariables = append(opts.FilterVariables, n)
			}
		}
	}

	created, err := encode.NewEncoder(c.Output, opts)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}
	enc = created
	for _, in := range inputs {
		if err := enc.AddFile(in.path, in.format); err != nil {
			return fmt.Errorf("add %s: %w", in.path, err)
		}
	}

	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		var lastSub, lastSk int64
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopPoll:
				return
			case <-t.C:
				if tilePhase == nil {
					continue
				}
				sub, sk := enc.Progress()
				if sub == lastSub && sk == lastSk {
					continue
				}
				lastSub, lastSk = sub, sk
				tilePhase.SetCurrent(sub + sk)
				if start := tileStart.Load(); start > 0 {
					elapsed := time.Since(time.Unix(0, start))
					if elapsed > 200*time.Millisecond {
						rate := float64(sub) / elapsed.Seconds()
						tilePhase.SetExtra(formatTileRate(rate) + "  " + commaInt(sub) + " written")
					}
				}
			}
		}
	}()

	t0 := time.Now()
	finishErr := enc.Finish()
	close(stopPoll)
	<-pollDone

	if finishErr != nil {
		if tilePhase != nil {
			tilePhase.Done("failed")
		}
		return fmt.Errorf("encode: %w", finishErr)
	}
	if snapPhase != nil {
		snapPhase.Done("")
	}

	if len(finalPlans) > 0 {
		printVariablePlans(finalPlans)
	}

	sub, sk := enc.Progress()
	ui.Section("Done")
	st, _ := os.Stat(c.Output)
	rows := [][2]string{{"output", c.Output}}
	if st != nil {
		rows = append(rows, [2]string{"size", humanBytes(st.Size())})
		var totalInput int64
		for _, in := range inputs {
			if inSt, err := os.Stat(in.path); err == nil {
				totalInput += inSt.Size()
			}
		}
		if totalInput > 0 {
			ratio := float64(totalInput) / float64(st.Size())
			rows = append(rows, [2]string{"input -> output", fmt.Sprintf("%s -> %s  (%.2fx)", humanBytes(totalInput), humanBytes(st.Size()), ratio)})
		}
	}
	scanDur := time.Duration(0)
	if s, e := scanStart.Load(), scanEnd.Load(); s > 0 && e > s {
		scanDur = time.Duration(e - s)
	}
	encodeDur := time.Duration(0)
	if s, e := tileStart.Load(), tileEnd.Load(); s > 0 && e > s {
		encodeDur = time.Duration(e - s)
	}
	rows = append(rows,
		[2]string{"tiles written", commaInt(sub)},
		[2]string{"tiles skipped", commaInt(sk)},
		[2]string{"scan", formatDuration(scanDur)},
		[2]string{"encode", formatDuration(encodeDur)},
		[2]string{"total", formatDuration(time.Since(t0))},
	)
	ui.Summary(rows)
	return nil
}
