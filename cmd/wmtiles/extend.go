package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/directory"
	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/internal/scan"
	"github.com/hstin-de/wmtiles/reader"
	"github.com/hstin-de/wmtiles/tileid"
)

func runExtend(c *extendCmd) error {
	wmtPath := c.File
	srcPath := c.Source

	overrides, err := scan.ParsePrecisionOverrides(c.Precision)
	if err != nil {
		return err
	}

	if _, err := os.Stat(wmtPath); err != nil {
		return err
	}
	if _, err := os.Stat(srcPath); err != nil {
		return err
	}

	srcFormatEnc, err := resolveInputFormat(c.Format, srcPath)
	if err != nil {
		return err
	}
	srcFormat := srcFormatGRIB2
	if srcFormatEnc == encode.FormatHDF5 {
		srcFormat = srcFormatHDF5
	}

	ui.Banner("extend", fmt.Sprintf("%s <- %s   %d cores", wmtPath, srcPath, runtime.GOMAXPROCS(0)))

	ui.Section("Settings")
	ui.KV("source format", srcFormat.String())
	if c.Filter == "" {
		ui.KV("filter", "all shortNames")
	} else {
		ui.KV("filter", c.Filter)
	}
	ui.KV("replace blocks", boolWord(c.AllowReplace))

	r, err := reader.Open(wmtPath)
	if err != nil {
		return fmt.Errorf("open existing file: %w", err)
	}
	pixelSize := 1 << r.Header.TilePixelSizeLog2
	minZoom := r.Header.MinZoom
	maxZoom := r.Header.MaxZoom
	ui.KVf("current gen", "%d", r.Header.SnapshotGeneration)
	ui.KVf("zoom range", "%d..%d", minZoom, maxZoom)
	ui.KVf("tile size", "%d px", pixelSize)
	r.Close()

	ui.Section("Extend")

	var (
		scanPhase, tilePhase, compressPhase, writePhase, snapPhase *Phase
		bytesOnDisk                                                atomic.Uint64
		scanStart, scanEnd, tileStart, tileEnd                     atomic.Int64
		scannedFiles                                               atomic.Int64
		finalPlans                                                 []encode.VariablePlan
	)
	var app *encode.Appender

	endTilePhase := func() {
		if tilePhase == nil {
			return
		}
		sub, _ := app.Progress()
		tilePhase.SetCurrent(sub)
		start := time.Unix(0, tileStart.Load())
		dur := time.Since(start)
		tileEnd.Store(time.Now().UnixNano())
		tilePhase.Done(fmt.Sprintf("%s tiles  %s", commaInt(sub), formatTileRateString(sub, dur)))
		tilePhase = nil
	}

	opts := encode.AppenderOptions{
		AllowReplace: c.AllowReplace,
		Precision:    overrides,
		OnInputScanned: func(name string, records int) {
			if scanPhase == nil {
				scanStart.Store(time.Now().UnixNano())
				scanPhase = ui.StartPhase("scan source", 0)
			}
			scannedFiles.Add(1)
			scanPhase.SetExtra(fmt.Sprintf("%s messages", commaInt(int64(records))))
		},
		OnScanComplete: func(stats encode.ScanStats) {
			scanEnd.Store(time.Now().UnixNano())
			if scanPhase != nil {
				scanPhase.Done(fmt.Sprintf("%s msgs kept of %s", commaInt(int64(stats.KeptMessages)), commaInt(int64(stats.TotalMessages))))
				scanPhase = nil
			}
			ui.KVf("variables", "%d", stats.VariableCount)
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
	if c.Filter != "" {
		for n := range strings.SplitSeq(c.Filter, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				opts.FilterVariables = append(opts.FilterVariables, n)
			}
		}
	}

	created, err := encode.NewAppender(wmtPath, opts)
	if err != nil {
		return fmt.Errorf("appender init: %w", err)
	}
	app = created
	if err := app.AddFile(srcPath, srcFormatEnc); err != nil {
		return fmt.Errorf("add %s: %w", srcPath, err)
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
				sub, sk := app.Progress()
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
	finishErr := app.Finish()
	close(stopPoll)
	<-pollDone

	if finishErr != nil {
		if tilePhase != nil {
			tilePhase.Done("failed")
		}
		return fmt.Errorf("extend: %w", finishErr)
	}
	if snapPhase != nil {
		snapPhase.Done("")
	}

	if len(finalPlans) > 0 {
		printVariablePlans(finalPlans)
	}

	stats := app.Stats()
	ui.Section("Done")
	st, _ := os.Stat(wmtPath)
	rows := [][2]string{{"file", wmtPath}}
	if st != nil {
		rows = append(rows, [2]string{"size", humanBytes(st.Size())})
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
		[2]string{"tiles written", commaInt(stats.Submitted)},
		[2]string{"tiles skipped", commaInt(stats.Skipped)},
		[2]string{"already present", fmt.Sprintf("%d", stats.AlreadyPresent)},
		[2]string{"scan", formatDuration(scanDur)},
		[2]string{"encode", formatDuration(encodeDur)},
		[2]string{"total", formatDuration(time.Since(t0))},
	)
	ui.Summary(rows)
	return nil
}

func runCompact(c *compactCmd) error {
	in, out := c.Input, c.Output
	if _, err := os.Stat(in); err != nil {
		return err
	}

	ui.Banner("compact", fmt.Sprintf("%s -> %s", in, out))

	r, err := reader.Open(in)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer r.Close()
	ui.Section("Settings")
	ui.KVf("generation", "%d", r.Header.SnapshotGeneration)
	ui.KVf("variables", "%d", len(r.Snapshot.Variables))
	ui.KVf("blocks", "%d", r.Snapshot.Header.NumBlocks)

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
	var (
		compressPhase, writePhase, snapPhase *Phase
		bytesOnDisk                          atomic.Uint64
	)
	opts := encoder.Options{
		TilePixelSizeLog2:     r.Header.TilePixelSizeLog2,
		MinZoom:               r.Header.MinZoom,
		MaxZoom:               r.Header.MaxZoom,
		ReferenceForecastTime: time.UnixMilli(r.Snapshot.Header.ReferenceTimeMs).UTC(),
		TimeCatalog:           r.Snapshot.TimeCat,
		BBox:                  bbox,
		Variables:             specs,
		Metadata:              r.Snapshot.Metadata,
		OnPhase: func(stage string) {
			switch stage {
			case "compress_blocks":
				compressPhase = ui.StartPhase("compress blocks", 0)
			case "write_blocks":
				if compressPhase != nil {
					compressPhase.Done("")
				}
				writePhase = ui.StartPhase("write blocks", 0)
			case "write_snapshot":
				if writePhase != nil {
					writePhase.Done(humanBytes(int64(bytesOnDisk.Load())) + " on disk")
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
	ui.Section("Compact")
	copyPhase := ui.StartPhase("copy tiles", int64(len(rows)))
	tCopy := time.Now()
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
		copyPhase.AddCurrent(1)
	}
	copyPhase.Done(fmt.Sprintf("%s tiles  %s", commaInt(int64(tilesCopied)), formatTileRateString(int64(tilesCopied), time.Since(tCopy))))

	if err := enc.Finish(); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if snapPhase != nil {
		snapPhase.Done("")
	}

	st, _ := os.Stat(out)
	stIn, _ := os.Stat(in)
	ui.Section("Done")
	rows2 := [][2]string{{"output", out}}
	if st != nil && stIn != nil {
		rows2 = append(rows2,
			[2]string{"input size", humanBytes(stIn.Size())},
			[2]string{"output size", humanBytes(st.Size())},
		)
		if stIn.Size() > 0 {
			ratio := float64(stIn.Size()) / float64(st.Size())
			rows2 = append(rows2, [2]string{"ratio", fmt.Sprintf("%.2fx", ratio)})
		}
	}
	rows2 = append(rows2,
		[2]string{"blocks copied", fmt.Sprintf("%d", declared)},
		[2]string{"tiles copied", commaInt(int64(tilesCopied))},
	)
	ui.Summary(rows2)
	return nil
}
