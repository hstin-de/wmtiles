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

	cliSection("WMTiles encode")
	cliKV("output", flags.output)
	cliKVf("zoom range", "%d..%d", flags.minZoom, flags.maxZoom)
	cliKVf("tile size", "%d px", 1<<flags.tileSizeLog2)
	if flags.filter == "" {
		cliKV("filter", "all variables")
	} else {
		cliKV("filter", flags.filter)
	}
	cliKVf("inputs", "%d HDF5 files", len(inputs))
	if len(inputs) > 0 {
		cliKV("first", inputs[0])
		if len(inputs) > 1 {
			cliKV("last", inputs[len(inputs)-1])
		}
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
	}
	if flags.filter != "" {
		for n := range strings.SplitSeq(flags.filter, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				opts.FilterVariables = append(opts.FilterVariables, n)
			}
		}
	}

	enc, err := encode.NewEncoder(flags.output, opts)
	if err != nil {
		return fmt.Errorf("encoder init: %w", err)
	}
	for _, in := range inputs {
		if err := enc.AddFile(in, encode.FormatHDF5); err != nil {
			return fmt.Errorf("add %s: %w", in, err)
		}
	}

	t0 := time.Now()
	cliSection("Tile encoding")
	if err := enc.Finish(); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	cliKV("duration", formatDuration(time.Since(t0)))

	st, _ := os.Stat(flags.output)
	cliSection("Done")
	if st != nil {
		cliKV("file", flags.output)
		cliKV("size", humanBytes(st.Size()))
	}
	return nil
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
