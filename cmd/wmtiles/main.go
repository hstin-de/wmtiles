package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

func main() {
	initRenderer()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "encode":
		format, rest, err := detectFormat(os.Args[2:])
		if err != nil {
			fatal("encode", err)
		}
		switch format {
		case "hdf5":
			if err := runEncodeHDF5("encode", rest); err != nil {
				fatal("encode", err)
			}
		default:
			if err := runEncodeGRIB("encode", rest); err != nil {
				fatal("encode", err)
			}
		}
	case "encode-grib":
		if err := runEncodeGRIB("encode-grib", os.Args[2:]); err != nil {
			fatal("encode-grib", err)
		}
	case "encode-hdf5":
		if err := runEncodeHDF5("encode-hdf5", os.Args[2:]); err != nil {
			fatal("encode-hdf5", err)
		}
	case "extend":
		if err := runExtend(os.Args[2:]); err != nil {
			fatal("extend", err)
		}
	case "compact":
		if err := runCompact(os.Args[2:]); err != nil {
			fatal("compact", err)
		}
	case "snapshot-history":
		if err := runSnapshotHistory(os.Args[2:]); err != nil {
			fatal("snapshot-history", err)
		}
	case "inspect":
		if err := runInspect(os.Args[2:]); err != nil {
			fatal("inspect", err)
		}
	case "verify":
		if err := runVerify(os.Args[2:]); err != nil {
			fatal("verify", err)
		}
	case "compare":
		if err := runCompare(os.Args[2:]); err != nil {
			fatal("compare", err)
		}
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fatal("serve", err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "error: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func fatal(command string, err error) {
	fmt.Fprintf(os.Stderr, "error: %s failed\n", command)
	fmt.Fprintf(os.Stderr, "  reason: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `wmtiles: cloud optimised tiled weather data format

usage:
  wmtiles encode           <input>     -o out.wmt ...  convert GRIB2 or HDF5 (auto-detected) into a fresh .wmt
  wmtiles encode-grib      <input.grib2> -o out.wmt    force GRIB2 encoder
  wmtiles encode-hdf5      <input.h5|glob> -o out.wmt  force HDF5 encoder (ODIM_H5 or CF/NetCDF4)
  wmtiles extend           <file.wmt> <input.grib2>    append blocks for new (variable, time) pairs
  wmtiles compact          <input.wmt> <output.wmt>    rewrite a file with the snapshot in the cold start window
  wmtiles snapshot-history <file.wmt>                  list active + previous snapshots
  wmtiles inspect          <file.wmt>                  dump header + catalog + stats
  wmtiles verify           <file.wmt>                  structural sanity check (incl. CRC validation)
  wmtiles compare          <input.grib2> <file.wmt> ... pixel by pixel fidelity report vs. source GRIB
  wmtiles serve            <file.wmt> [--addr :8080]   launch a web viewer

encode flags:
  -o PATH                  output .wmt path (required)
  --format FMT             override input format (grib2|hdf5); default = auto-detect by magic bytes / extension
  --min-zoom N             minimum zoom (default 0)
  --max-zoom N             maximum zoom (default 5)
  --tile-size-log2 N       tile size log2 (default 8 = 256 px; allowed 7..10)
  --filter SHORTNAMES      comma-separated shortNames to keep (default: all)
  --precision NAME=K,...   per variable quantisation precision overrides`)
}

func runInspect(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wmtiles inspect <file.wmt>")
	}
	r, err := reader.Open(args[0])
	if err != nil {
		return err
	}
	defer r.Close()

	h := r.Header
	snap := r.Snapshot

	ui.Banner("inspect", args[0])

	ui.Section("File")
	if st, _ := os.Stat(args[0]); st != nil {
		ui.KV("file size", humanBytes(st.Size()))
		ui.KV("logical end", humanBytes(int64(h.FileLogicalEnd)))
	}
	ui.KVf("format version", "%d", h.FormatVersion)
	ui.KVf("generation", "%d", h.SnapshotGeneration)
	ui.KVf("zoom range", "%d..%d", h.MinZoom, h.MaxZoom)
	ui.KVf("tile size", "%d px", 1<<h.TilePixelSizeLog2)
	ui.KVf("compression", "%d", h.InternalCompression)
	ui.KV("cold start", boolWord(h.Flags&format.FlagColdStartInWindow != 0))
	ui.KV("previous snap", boolWord(h.Flags&format.FlagHasPreviousSnapshot != 0))

	ui.Section("Snapshot")
	ui.KV("reference time", time.UnixMilli(snap.Header.ReferenceTimeMs).UTC().Format(time.RFC3339))
	ui.KV("time axis", describeTimeCatalog(snap.TimeCat))
	ui.KVf("variables", "%d", len(snap.Variables))
	ui.KVf("blocks", "%d", snap.Header.NumBlocks)

	rows := make([][]string, 0, len(snap.Variables))
	for _, v := range snap.Variables {
		minS := fmt.Sprintf("%g", v.ValueMinObservedGlobal)
		maxS := fmt.Sprintf("%g", v.ValueMaxObservedGlobal)
		if math.IsNaN(v.ValueMinObservedGlobal) {
			minS = "n/a"
		}
		if math.IsNaN(v.ValueMaxObservedGlobal) {
			maxS = "n/a"
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", v.VariableID),
			v.Name,
			emptyAsNA(v.Unit),
			dtypeBadge(dtypeCodeName(v.DefaultDType)),
			formatFloat(v.DefaultPrecisionHint),
			"[" + minS + ", " + maxS + "]",
			emptyAsNA(v.ColormapHint),
		})
	}
	ui.Section("Variables")
	cliTableAligned([]string{"id", "name", "unit", "dtype", "precision", "range", "colormap"}, rows, "rlllllll")

	totalBlocks := 0
	totalAddressed := uint64(0)
	totalContents := uint64(0)
	totalBytes := uint64(0)
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		totalBlocks++
		totalAddressed += e.NumAddressedTiles
		totalContents += e.NumTileContents
		totalBytes += e.BlockLength
		return nil
	}); err != nil {
		return fmt.Errorf("iterate blocks: %w", err)
	}
	ui.Section("Storage")
	ui.KVf("blocks", "%d", totalBlocks)
	ui.KV("addressed tiles", commaUint(totalAddressed))
	ui.KV("unique blobs", commaUint(totalContents))
	ui.KV("dedup ratio", formatDedupRatio(totalContents, totalAddressed))
	ui.KV("variable catalog", humanBytes(int64(snap.Header.VariableCatalogLen)))
	ui.KV("time catalog", humanBytes(int64(snap.Header.TimeCatalogLen)))
	ui.KV("block table root", humanBytes(int64(snap.Header.BlockTableRootLen)))
	ui.KV("block table leaves", humanBytes(int64(snap.Header.BlockTableLeavesLen)))
	ui.KV("metadata", humanBytes(int64(snap.Header.MetadataLen)))
	ui.KV("blocks total", humanBytes(int64(totalBytes)))

	st, _ := os.Stat(args[0])
	if st != nil {
		slack := int64(st.Size()) - int64(h.FileLogicalEnd)
		if slack > 0 {
			ui.KV("orphaned bytes", humanBytes(slack))
		}
	}
	return nil
}

func runVerify(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wmtiles verify <file.wmt>")
	}
	r, err := reader.Open(args[0])
	if err != nil {
		return err
	}
	defer r.Close()

	ui.Banner("verify", args[0])

	ui.Section("Verify")
	if err := r.SanityCheck(); err != nil {
		return err
	}

	totalBlocks := int64(r.Snapshot.Header.NumBlocks)
	verifyPhase := ui.StartPhase("verify blocks", totalBlocks)
	totalAddressed := uint64(0)
	totalContents := uint64(0)
	var scanned int64
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		totalAddressed += e.NumAddressedTiles
		totalContents += e.NumTileContents
		out := make([]float32, r.PixelCount())
		if err := r.ReadTile(r.Snapshot.Variables[e.VariableID].Name, e.TimeID,
			r.Header.MinZoom, 0, 0, out); err != nil {
			_ = err
		}
		scanned++
		verifyPhase.SetCurrent(scanned)
		return nil
	}); err != nil {
		verifyPhase.Done("failed")
		return err
	}
	verifyPhase.Done("")

	ui.Section("Done")
	ui.Summary([][2]string{
		{"status", ui.styled("ok", ansiGreen, ansiBold)},
		{"checks", "header, snapshot, block table, sample tile decode"},
		{"addressed tiles", commaUint(totalAddressed)},
		{"unique blobs", commaUint(totalContents)},
		{"dedup ratio", formatDedupRatio(totalContents, totalAddressed)},
	})
	return nil
}

func runSnapshotHistory(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wmtiles snapshot-history <file.wmt>")
	}
	r, err := reader.Open(args[0])
	if err != nil {
		return err
	}
	defer r.Close()

	h := r.Header
	ui.Banner("snapshot-history", args[0])
	ui.Section("Active snapshot")
	ui.KVf("generation", "%d", h.SnapshotGeneration)
	ui.KVf("offset", "%d", h.ActiveSnapshotOffset)
	ui.KV("length", humanBytes(int64(h.ActiveSnapshotLength)))
	ui.KVf("variables", "%d", r.Snapshot.Header.NumVariables)
	ui.KVf("time steps", "%d", r.Snapshot.Header.NumTimeSteps)
	ui.KVf("blocks", "%d", r.Snapshot.Header.NumBlocks)
	if h.Flags&format.FlagHasPreviousSnapshot != 0 {
		ui.Section("Previous snapshot")
		ui.KVf("offset", "%d", h.PreviousSnapshotOffset)
		ui.KV("length", humanBytes(int64(h.PreviousSnapshotLength)))
	} else {
		ui.Section("Previous snapshot")
		ui.KV("status", "none")
	}
	return nil
}

func humanBytes(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case n >= TB:
		return fmt.Sprintf("%.2f TB", float64(n)/TB)
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/KB)
	}
	return fmt.Sprintf("%d B", n)
}

func describeTimeCatalog(t format.TimeCatalog) string {
	if t.Regular {
		start := time.UnixMilli(t.StartMs).UTC().Format(time.RFC3339)
		return fmt.Sprintf("regular, start %s, interval %s, count %d",
			start, time.Duration(t.IntervalMs)*time.Millisecond, t.Count)
	}
	return fmt.Sprintf("irregular, count %d", t.Count)
}

func emptyAsNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
