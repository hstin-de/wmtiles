package main

import (
	"flag"
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
)

// runEncodeHDF5 handles `wmtiles encode` for ODIM_H5 and CF/NetCDF4 inputs.
// glob/dir args are expanded; the union of variables and times is folded into
// one fresh .wmt.
func runEncodeHDF5(command string, args []string) error {
	flags, inputs, err := parseHDF5EncodeFlags(command, args)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("no HDF5 inputs found")
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

	subtitle := fmt.Sprintf("%d files -> %s   %d cores", len(inputs), flags.output, runtime.GOMAXPROCS(0))
	ui.Banner(command, subtitle)

	ui.Section("Settings")
	ui.KVf("zoom range", "%d..%d", flags.minZoom, flags.maxZoom)
	ui.KVf("tile size", "%d px", 1<<flags.tileSizeLog2)
	if flags.filter == "" {
		ui.KV("filter", "all variables")
	} else {
		ui.KV("filter", flags.filter)
	}
	ui.KVf("inputs", "%d HDF5 files", len(inputs))
	if len(inputs) > 0 {
		ui.KV("first", inputs[0])
		if len(inputs) > 1 {
			ui.KV("last", inputs[len(inputs)-1])
		}
	}

	ui.Section("Encode")

	var (
		scanPhase, tilePhase, compressPhase, writePhase, snapPhase *Phase
		bytesOnDisk                                                atomic.Uint64
		scannedFiles                                               atomic.Int64
		scanStart                                                  atomic.Int64
		scanEnd                                                    atomic.Int64
		tileStart                                                  atomic.Int64
		tileEnd                                                    atomic.Int64
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
		TileSize:  1 << flags.tileSizeLog2,
		MinZoom:   uint8(flags.minZoom),
		MaxZoom:   uint8(flags.maxZoom),
		Precision: flags.precisionOverrides,
		Metadata: map[string]any{
			"sourceFormat": "hdf5",
			"sourceCount":  len(inputs),
		},
		DisableDeltaCodec: flags.disableDelta,
		ZstdLevel:         flags.zstdLevel,
		OnInputScanned: func(name string, records int) {
			if scanPhase == nil {
				scanStart.Store(time.Now().UnixNano())
				scanPhase = ui.StartPhase("scan inputs", int64(len(inputs)))
			}
			n := scannedFiles.Add(1)
			scanPhase.SetCurrent(n)
			scanPhase.SetExtra(filepath.Base(name))
		},
		OnScanComplete: func(stats encode.ScanStats) {
			scanEnd.Store(time.Now().UnixNano())
			if scanPhase != nil {
				scanPhase.Done(fmt.Sprintf("%d files, %s msgs kept", int(scannedFiles.Load()), commaInt(int64(stats.KeptMessages))))
				scanPhase = nil
			}
			ui.KVf("variables", "%d", stats.VariableCount)
			ui.KV("time axis", describeTimeCatalog(stats.TimeAxis))
			ui.KV("grid bbox", formatBBox(stats.BBox))
			tileStart.Store(time.Now().UnixNano())
			tilePhase = ui.StartPhase("decode + tile", 0)
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
					writePhase.Done(humanBytes(int64(bytesOnDisk.Load())) + " on disk")
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
				bytesOnDisk.Add(bytes)
				writePhase.SetExtra(humanBytes(int64(bytesOnDisk.Load())))
			}
		},
	}
	if flags.filter != "" {
		for n := range strings.SplitSeq(flags.filter, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				opts.FilterVariables = append(opts.FilterVariables, n)
			}
		}
	}

	encCreated, err := encode.NewEncoder(flags.output, opts)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}
	enc = encCreated
	for _, in := range inputs {
		if err := enc.AddFile(in, encode.FormatHDF5); err != nil {
			return fmt.Errorf("add %s: %w", in, err)
		}
	}

	// Tile work runs inside Finish; we poll enc.Progress to keep tilePhase live.
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		var lastSub, lastSk int64
		t := time.NewTicker(150 * time.Millisecond)
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
					if elapsed > 250*time.Millisecond {
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
		printEncodePlans(finalPlans)
	}

	sub, sk := enc.Progress()
	ui.Section("Done")
	st, _ := os.Stat(flags.output)
	rows := [][2]string{{"output", flags.output}}
	if st != nil {
		rows = append(rows, [2]string{"size", humanBytes(st.Size())})
		var totalInput int64
		for _, in := range inputs {
			if inSt, err := os.Stat(in); err == nil {
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

// printEncodePlans mirrors printVariablePlans for the encode.Encoder pipeline.
func printEncodePlans(plans []encode.VariablePlan) {
	if len(plans) == 0 {
		return
	}
	const maxRows = 20
	visible := plans
	hidden := 0
	if len(plans) > maxRows {
		sort.SliceStable(plans, func(i, j int) bool { return plans[i].Messages > plans[j].Messages })
		visible = plans[:maxRows]
		hidden = len(plans) - maxRows
		sort.Slice(visible, func(i, j int) bool { return visible[i].Name < visible[j].Name })
	}
	rows := make([][]string, 0, len(visible))
	for _, p := range visible {
		rows = append(rows, []string{
			p.Name,
			emptyAsNA(p.Unit),
			fmt.Sprintf("%d", p.Messages),
			formatRange(p.Min, p.Max),
			formatFloat(p.Precision) + " (" + p.PrecSrc + ")",
			dtypeBadge(p.DType),
			formatFloat(p.Step),
		})
	}
	ui.Section("Variables")
	cliTableAligned([]string{"name", "unit", "msgs", "range", "precision", "dtype", "step"}, rows, "llrllll")
	if hidden > 0 {
		ui.KVf("more", "%d variables omitted", hidden)
	}
}

type hdf5EncodeFlags struct {
	output             string
	minZoom            uint
	maxZoom            uint
	tileSizeLog2       uint
	filter             string
	precisionOverrides map[string]float64
	disableDelta       bool
	zstdLevel          int
	cpuProfile         string
	memProfile         string
	traceProfile       string
}

func parseHDF5EncodeFlags(command string, args []string) (hdf5EncodeFlags, []string, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	out := fs.String("o", "", "output .wmt path (required)")
	minZoom := fs.Uint("min-zoom", 0, "minimum zoom level")
	maxZoom := fs.Uint("max-zoom", 5, "maximum zoom level")
	tileSizeLog2 := fs.Uint("tile-size-log2", 8, "tile size as log2 of pixel count (7..10 -> 128..1024)")
	filter := fs.String("filter", "", "comma-separated variable shortNames to keep (e.g. 'dbzh,rate'); empty = keep all")
	precisionOverride := fs.String("precision", "", "per variable quantization precision overrides, e.g. 'dbzh=0.5,rate=0.01'")
	disableDelta := fs.Bool("disable-delta-codec", false, "force bitshuffle-only encoding")
	zstdLevel := fs.Int("zstd-level", 0, "libzstd compression level (1=fastest..22=best); 0 uses the default (3)")
	cpuProfile := fs.String("cpuprofile", "", "write CPU profile to file")
	memProfile := fs.String("memprofile", "", "write heap profile to file")
	traceProfile := fs.String("trace", "", "write execution trace to file")
	normalizedArgs := normalizeGribEncodeArgs(args)
	if err := fs.Parse(normalizedArgs); err != nil {
		return hdf5EncodeFlags{}, nil, err
	}
	overrides, err := parsePrecisionOverrides(*precisionOverride)
	if err != nil {
		return hdf5EncodeFlags{}, nil, err
	}
	if fs.NArg() < 1 {
		return hdf5EncodeFlags{}, nil, fmt.Errorf("usage: wmtiles %s <input.h5|glob> -o <output.wmt>", command)
	}
	if *out == "" {
		return hdf5EncodeFlags{}, nil, fmt.Errorf("missing -o output path")
	}
	if *tileSizeLog2 < 7 || *tileSizeLog2 > 10 {
		return hdf5EncodeFlags{}, nil, fmt.Errorf("tile-size-log2 must be 7..10, got %d", *tileSizeLog2)
	}
	if *maxZoom < *minZoom {
		return hdf5EncodeFlags{}, nil, fmt.Errorf("max-zoom %d < min-zoom %d", *maxZoom, *minZoom)
	}

	inputs, err := expandHDF5Inputs(fs.Args())
	if err != nil {
		return hdf5EncodeFlags{}, nil, err
	}

	return hdf5EncodeFlags{
		output:             *out,
		minZoom:            *minZoom,
		maxZoom:            *maxZoom,
		tileSizeLog2:       *tileSizeLog2,
		filter:             *filter,
		precisionOverrides: overrides,
		disableDelta:       *disableDelta,
		zstdLevel:          *zstdLevel,
		cpuProfile:         *cpuProfile,
		memProfile:         *memProfile,
		traceProfile:       *traceProfile,
	}, inputs, nil
}

// resolves each positional arg as a directory (HDF5-extension entries),
// a glob, or a plain file. result is deduplicated and sorted.
func expandHDF5Inputs(args []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(args))
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
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
				low := strings.ToLower(e.Name())
				if !(strings.HasSuffix(low, ".h5") ||
					strings.HasSuffix(low, ".hdf5") ||
					strings.HasSuffix(low, "-hd5") ||
					strings.HasSuffix(low, ".nc") ||
					strings.HasSuffix(low, ".nc4")) {
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
