package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/alecthomas/kong"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

func main() {
	initRenderer()
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("wmtiles"),
		kong.Description("Cloud optimised tiled weather data format."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true, NoAppSummary: false}),
	)
	if err := ctx.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}

func runInspect(path string) error {
	r, err := reader.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()

	h := r.Header
	snap := r.Snapshot

	ui.Banner("inspect", path)

	ui.Section("File")
	if st, _ := os.Stat(path); st != nil {
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

	st, _ := os.Stat(path)
	if st != nil {
		slack := int64(st.Size()) - int64(h.FileLogicalEnd)
		if slack > 0 {
			ui.KV("orphaned bytes", humanBytes(slack))
		}
	}
	return nil
}

func runVerify(path string) error {
	r, err := reader.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()

	ui.Banner("verify", path)

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

func runSnapshotHistory(path string) error {
	r, err := reader.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()

	h := r.Header
	ui.Banner("snapshot-history", path)
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
