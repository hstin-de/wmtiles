package encoder_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

func TestHeaderCRCRecovery(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "crash.wmt")
	const pixSize = 128

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	tiles := makeTiles("temp", pixSize, 1, 2)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 2),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatalf("initial encode: %v", err)
	}

	ctx, err := encoder.OpenForAppend(out, encoder.AppendOptions{})
	if err != nil {
		t.Fatalf("OpenForAppend: %v", err)
	}
	if _, err := ctx.RegisterVariable(encoder.VariableSpec{Name: "wind", Unit: "m/s"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for ti := uint32(0); ti < 2; ti++ {
		if err := ctx.DeclareBlock(encoder.BlockSpec{
			Variable: "wind", TimeStep: ti, ValueMin: -50, ValueMax: 50,
		}); err != nil {
			t.Fatalf("declare: %v", err)
		}
	}
	for _, src := range makeTiles("wind", pixSize, 1, 2) {
		if err := ctx.Submit(src); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	if err := ctx.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r.Header.Flags&format.FlagHasPreviousSnapshot == 0 {
		t.Fatalf("expected hasPrev flag")
	}
	prevOff := r.Header.PreviousSnapshotOffset
	prevLen := r.Header.PreviousSnapshotLength
	r.Close()

	f, err := os.OpenFile(out, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	bodyByte := make([]byte, 1)
	if _, err := f.ReadAt(bodyByte, 100); err != nil {
		t.Fatal(err)
	}
	bodyByte[0] ^= 0xFF
	if _, err := f.WriteAt(bodyByte, 100); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err = reader.Open(out)
	if err != nil {
		t.Fatalf("expected fallback to previous_snapshot, got: %v", err)
	}
	defer r.Close()
	if _, ok := r.VariableID("temp"); !ok {
		t.Errorf("temp should still be readable from previous_snapshot")
	}
	if _, ok := r.VariableID("wind"); ok {
		t.Errorf("wind was added in the corrupted append; previous_snapshot must not see it")
	}
	_ = prevOff
	_ = prevLen
}

func TestSnapshotCRCRecovery(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snapcrash.wmt")
	const pixSize = 128

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	tiles := makeTiles("temp", pixSize, 1, 1)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 1),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatal(err)
	}

	ctx, err := encoder.OpenForAppend(out, encoder.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.RegisterVariable(encoder.VariableSpec{Name: "wind", Unit: "m/s"}); err != nil {
		t.Fatal(err)
	}
	if err := ctx.DeclareBlock(encoder.BlockSpec{
		Variable: "wind", TimeStep: 0, ValueMin: -50, ValueMax: 50,
	}); err != nil {
		t.Fatal(err)
	}
	for _, src := range makeTiles("wind", pixSize, 1, 1) {
		if err := ctx.Submit(src); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctx.Finish(); err != nil {
		t.Fatal(err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	activeOff := int64(r.Header.ActiveSnapshotOffset)
	activeLen := r.Header.ActiveSnapshotLength
	if activeLen == 0 {
		r.Close()
		t.Fatal("active snapshot has zero length")
	}
	r.Close()

	f, err := os.OpenFile(out, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	corruptOff := activeOff + int64(activeLen/2)
	bodyByte := make([]byte, 1)
	if _, err := f.ReadAt(bodyByte, corruptOff); err != nil {
		t.Fatal(err)
	}
	bodyByte[0] ^= 0xFF
	if _, err := f.WriteAt(bodyByte, corruptOff); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err = reader.Open(out)
	if err != nil {
		t.Fatalf("expected fallback to previous snapshot, got error: %v", err)
	}
	defer r.Close()
	if _, ok := r.VariableID("temp"); !ok {
		t.Errorf("expected previous snapshot to expose 'temp'")
	}
	if _, ok := r.VariableID("wind"); ok {
		t.Errorf("previous snapshot should NOT contain 'wind' (that was the corrupted append)")
	}
	if r.Header.SnapshotGeneration != 1 {
		t.Logf("note: header generation is %d (header points to corrupted snapshot)", r.Header.SnapshotGeneration)
	}
}

func TestAppendOrphanedBytesAfterAbort(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "orphan.wmt")
	const pixSize = 128

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	if err := encoder.Encode(makeTiles("temp", pixSize, 1, 1), encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 1),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}, out); err != nil {
		t.Fatal(err)
	}

	statBefore, _ := os.Stat(out)
	logicalEndBefore := uint64(0)
	{
		r, err := reader.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		logicalEndBefore = r.Header.FileLogicalEnd
		r.Close()
	}

	f, err := os.OpenFile(out, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	garbage := []byte("orphaned_bytes_from_aborted_append")
	if _, err := f.WriteAt(garbage, int64(statBefore.Size())); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("reader should ignore orphaned bytes: %v", err)
	}
	if r.Header.FileLogicalEnd != logicalEndBefore {
		t.Errorf("FileLogicalEnd changed: was %d, now %d", logicalEndBefore, r.Header.FileLogicalEnd)
	}
	if _, ok := r.VariableID("temp"); !ok {
		t.Errorf("temp not found")
	}
	r.Close()

	ctx, err := encoder.OpenForAppend(out, encoder.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.RegisterVariable(encoder.VariableSpec{Name: "wind", Unit: "m/s"}); err != nil {
		t.Fatal(err)
	}
	if err := ctx.DeclareBlock(encoder.BlockSpec{
		Variable: "wind", TimeStep: 0, ValueMin: -50, ValueMax: 50,
	}); err != nil {
		t.Fatal(err)
	}
	for _, src := range makeTiles("wind", pixSize, 1, 1) {
		if err := ctx.Submit(src); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctx.Finish(); err != nil {
		t.Fatal(err)
	}

	r, err = reader.Open(out)
	if err != nil {
		t.Fatalf("post-append open: %v", err)
	}
	defer r.Close()
	if _, ok := r.VariableID("wind"); !ok {
		t.Errorf("wind should be present after successful append")
	}
}
