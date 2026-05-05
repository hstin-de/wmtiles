package reader_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

type rangeRecorder struct {
	src            readerAt
	totalReadBytes int64
	numReads       int64
}

type readerAt interface {
	ReadAt(p []byte, off int64) (int, error)
}

func (r *rangeRecorder) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt64(&r.numReads, 1)
	atomic.AddInt64(&r.totalReadBytes, int64(len(p)))
	return r.src.ReadAt(p, off)
}

func makeOneBlock(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "one.wmt")
	const pixSize = 128
	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	tiles := []encoder.Tile{}
	for z := uint8(0); z <= 1; z++ {
		n := uint32(1) << z
		for y := uint32(0); y < n; y++ {
			for x := uint32(0); x < n; x++ {
				px := make([]float32, pixSize*pixSize)
				for i := range px {
					px[i] = float32(i % 100)
				}
				tiles = append(tiles, encoder.Tile{
					Variable: "temp", TimeStep: 0, Z: z, X: x, Y: y, Pixels: px,
				})
			}
		}
	}
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: 0, Count: 1,
		},
		BBox:      [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestColdStart1RT(t *testing.T) {
	dir := t.TempDir()
	path := makeOneBlock(t, dir)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rec := &rangeRecorder{src: f}
	r, err := reader.NewReader(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if reads := atomic.LoadInt64(&rec.numReads); reads != 1 {
		t.Errorf("expected 1 cold-start read, got %d", reads)
	}
	if r.Header.Flags&format.FlagColdStartInWindow == 0 {
		t.Errorf("expected FlagColdStartInWindow on a small fresh file")
	}
}

func TestPerBlockQuantization(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "twoblocks.wmt")
	const pixSize = 128
	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	tiles := []encoder.Tile{}
	for z := uint8(0); z <= 0; z++ {
		px := make([]float32, pixSize*pixSize)
		for i := range px {
			px[i] = float32(i % 100)
		}
		tiles = append(tiles, encoder.Tile{
			Variable: "temp", TimeStep: 0, Z: z, X: 0, Y: 0, Pixels: px,
		})
	}
	{
		px := make([]float32, pixSize*pixSize)
		for i := range px {
			px[i] = float32(1000 + i%100)
		}
		tiles = append(tiles, encoder.Tile{
			Variable: "temp", TimeStep: 1, Z: 0, X: 0, Y: 0, Pixels: px,
		})
	}

	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: 3600_000, Count: 2,
		},
		BBox: [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "temp", Unit: "K", Precision: 0.1},
		},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatal(err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	id, _ := r.VariableID("temp")
	b0, err := r.LookupBlock(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := r.LookupBlock(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b0.Offset == b1.Offset {
		t.Errorf("expected per-block offsets to differ: both %g", b0.Offset)
	}
	if b0.ValueMax >= 500 {
		t.Errorf("block 0 vmax should be ~100, got %g", b0.ValueMax)
	}
	if b1.ValueMin <= 500 {
		t.Errorf("block 1 vmin should be ~1000, got %g", b1.ValueMin)
	}

	out32 := make([]float32, pixSize*pixSize)
	if err := r.ReadTile("temp", 0, 0, 0, 0, out32); err != nil {
		t.Fatal(err)
	}
	for i, v := range out32 {
		want := float32(i % 100)
		if diff := abs32(v - want); diff > 0.5 {
			t.Errorf("t=0 block: pixel %d got %g want ~%g (diff %g)", i, v, want, diff)
			break
		}
	}
	if err := r.ReadTile("temp", 1, 0, 0, 0, out32); err != nil {
		t.Fatal(err)
	}
	for i, v := range out32 {
		want := float32(1000 + i%100)
		if diff := abs32(v - want); diff > 0.5 {
			t.Errorf("t=1 block: pixel %d got %g want ~%g (diff %g)", i, v, want, diff)
			break
		}
	}
}

func TestCorruptHeaderRejected(t *testing.T) {
	dir := t.TempDir()
	path := makeOneBlock(t, dir)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("BADMAGIC"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := reader.Open(path); err == nil {
		t.Error("expected error opening file with bad magic")
	} else if !errors.Is(err, format.ErrBadMagic) {
		t.Logf("got error (file is rejected): %v", err)
	}
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func TestRangeCoalescing(t *testing.T) {
	dir := t.TempDir()
	path := makeOneBlock(t, dir)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rec := &rangeRecorder{src: f}
	r, err := reader.NewReader(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	coords := []reader.TileCoord{
		{Z: 1, X: 0, Y: 0},
		{Z: 1, X: 1, Y: 0},
		{Z: 1, X: 0, Y: 1},
		{Z: 1, X: 1, Y: 1},
	}
	outs := make([][]float32, 4)
	for i := range outs {
		outs[i] = make([]float32, r.PixelCount())
	}

	readsBefore := atomic.LoadInt64(&rec.numReads)
	if err := r.ReadTilesInBlock("temp", 0, coords, outs, reader.CoalesceOptions{}); err != nil {
		t.Fatalf("ReadTilesInBlock: %v", err)
	}
	readsAfter := atomic.LoadInt64(&rec.numReads)
	addedReads := readsAfter - readsBefore

	t.Logf("4 tiles in 1 block -> %d additional ReadAts", addedReads)
	if addedReads > 2 {
		t.Errorf("expected <= 2 fresh ReadAts (ideally 0 for cold-start-cached files), got %d", addedReads)
	}

	for i := range outs {
		for j, v := range outs[i] {
			want := float32(j % 100)
			if abs32(v-want) > 0.5 {
				t.Errorf("tile %d pixel %d: got %g want ~%g", i, j, v, want)
				break
			}
		}
	}
}

func TestRangeCoalescingMissingTile(t *testing.T) {
	dir := t.TempDir()
	path := makeOneBlock(t, dir)

	r, err := reader.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	coords := []reader.TileCoord{
		{Z: 1, X: 0, Y: 0},
		{Z: 7, X: 0, Y: 0},
	}
	outs := [][]float32{
		make([]float32, r.PixelCount()),
		make([]float32, r.PixelCount()),
	}
	if err := r.ReadTilesInBlock("temp", 0, coords, outs, reader.CoalesceOptions{}); err != nil {
		t.Fatal(err)
	}
	if outs[0][0] != outs[0][0] {
		t.Errorf("first tile should have non-NaN values")
	}
	allNaN := true
	for _, v := range outs[1] {
		if !math.IsNaN(float64(v)) {
			allNaN = false
			break
		}
	}
	if !allNaN {
		t.Errorf("missing tile should be NaN-filled")
	}
}
