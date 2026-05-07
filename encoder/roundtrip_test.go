package encoder_test

import (
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

func makeTiles(varName string, pixSize int, maxZoom uint8, numTimes uint32) []encoder.Tile {
	var tiles []encoder.Tile
	for t := uint32(0); t < numTimes; t++ {
		for z := uint8(0); z <= maxZoom; z++ {
			n := uint32(1) << z
			for y := uint32(0); y < n; y++ {
				for x := uint32(0); x < n; x++ {
					px := make([]float32, pixSize*pixSize)
					for i := range px {
						r := i / pixSize
						c := i % pixSize
						v := 250.0 +
							30.0*math.Sin(float64(c)/float64(pixSize)*math.Pi*2) +
							20.0*math.Cos(float64(r)/float64(pixSize)*math.Pi*2) +
							0.5*float64(int(t)+int(x)+int(y))
						px[i] = float32(v)
					}
					tiles = append(tiles, encoder.Tile{
						Variable: varName,
						TimeStep: t,
						Z:        z,
						X:        x,
						Y:        y,
						Pixels:   px,
					})
				}
			}
		}
	}
	return tiles
}

func regularTimeCatalog(start time.Time, intervalMs int64, count int64) format.TimeCatalog {
	return format.TimeCatalog{
		Regular:    true,
		StartMs:    start.UnixMilli(),
		IntervalMs: intervalMs,
		Count:      count,
	}
}

func TestSingleVariableRoundtrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.wmt")

	const maxZoom = 2
	const pixSize = 128
	const numTimes = 2

	tiles := makeTiles("air_temperature", pixSize, maxZoom, numTimes)

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               maxZoom,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, numTimes),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "air_temperature", Unit: "K", ColormapHint: "viridis"},
		},
	}

	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatalf("encode: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if err := r.SanityCheck(); err != nil {
		t.Errorf("sanity: %v", err)
	}

	id, _ := r.VariableID("air_temperature")
	blk, err := r.LookupBlock(id, 0)
	if err != nil {
		t.Fatalf("lookup block: %v", err)
	}
	allowedErr := math.Abs(blk.Scale)/2 + math.Abs(blk.ValueMax)*1e-6

	out32 := make([]float32, pixSize*pixSize)
	for _, src := range tiles {
		if err := r.ReadTile(src.Variable, src.TimeStep, src.Z, src.X, src.Y, out32); err != nil {
			t.Fatalf("read tile: %v", err)
		}
		var maxErr float64
		for i, want := range src.Pixels {
			e := math.Abs(float64(want - out32[i]))
			if e > maxErr {
				maxErr = e
			}
		}
		if maxErr > allowedErr {
			t.Errorf("tile t=%d z=%d x=%d y=%d: max err %g > allowed %g",
				src.TimeStep, src.Z, src.X, src.Y, maxErr, allowedErr)
			break
		}
	}
}

func TestLosslessF32Roundtrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "lossless.wmt")

	const pixSize = 128
	rng := rand.New(rand.NewPCG(7, 0))

	tiles := []encoder.Tile{}
	for t := uint32(0); t < 2; t++ {
		for z := uint8(0); z <= 1; z++ {
			n := uint32(1) << z
			for y := uint32(0); y < n; y++ {
				for x := uint32(0); x < n; x++ {
					px := make([]float32, pixSize*pixSize)
					for i := range px {
						px[i] = float32(rng.Float64()*1e-3 + 1e-9)
					}
					px[0] = float32(math.NaN())
					tiles = append(tiles, encoder.Tile{
						Variable: "spec_humidity", TimeStep: t, Z: z, X: x, Y: y, Pixels: px,
					})
				}
			}
		}
	}

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 2),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "spec_humidity", Unit: "kg/kg", Precision: 1e-12},
		},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatalf("encode: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, _ := r.VariableID("spec_humidity")
	blk, err := r.LookupBlock(id, 0)
	if err != nil {
		t.Fatalf("lookup block: %v", err)
	}
	if blk.DType != 3 {
		t.Fatalf("expected f32 (3), got %d", blk.DType)
	}

	out32 := make([]float32, pixSize*pixSize)
	for _, src := range tiles {
		if err := r.ReadTile(src.Variable, src.TimeStep, src.Z, src.X, src.Y, out32); err != nil {
			t.Fatalf("read: %v", err)
		}
		if !math.IsNaN(float64(out32[0])) {
			t.Errorf("NaN didn't round-trip")
		}
		for i := 1; i < len(src.Pixels); i++ {
			if math.Float32bits(src.Pixels[i]) != math.Float32bits(out32[i]) {
				t.Errorf("byte-exact mismatch at i=%d: %g vs %g", i, src.Pixels[i], out32[i])
				return
			}
		}
	}
}

func TestConstantTilesDeduplicate(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "constants.wmt")

	const pixSize = 128
	const maxZoom = 1

	tiles := []encoder.Tile{}
	for t := uint32(0); t < 4; t++ {
		for z := uint8(0); z <= maxZoom; z++ {
			n := uint32(1) << z
			for y := uint32(0); y < n; y++ {
				for x := uint32(0); x < n; x++ {
					px := make([]float32, pixSize*pixSize)
					for i := range px {
						px[i] = 0
					}
					tiles = append(tiles, encoder.Tile{
						Variable: "precip", TimeStep: t, Z: z, X: x, Y: y, Pixels: px,
					})
				}
			}
		}
	}

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               maxZoom,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 4),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "precip", Unit: "mm/h"}},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatalf("encode: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	totalContents := uint64(0)
	totalBlocks := 0
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		totalContents += e.NumTileContents
		totalBlocks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if totalBlocks != 4 {
		t.Errorf("expected 4 blocks, got %d", totalBlocks)
	}
	if totalContents != 4 {
		t.Errorf("expected 4 unique blobs (one per block), got %d", totalContents)
	}

	out32 := make([]float32, pixSize*pixSize)
	if err := r.ReadTile("precip", 0, 0, 0, 0, out32); err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, v := range out32 {
		if math.Abs(float64(v)) > 1e-3 {
			t.Errorf("expected ~0, got %g", v)
		}
	}
}

func TestColdStartBudget(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "leaves.wmt")

	const maxZoom = 3
	const pixSize = 128
	const numTimes = 4

	tiles := makeTiles("temp", pixSize, maxZoom, numTimes)

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               maxZoom,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, numTimes),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(tiles, opts, out); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.SanityCheck(); err != nil {
		t.Errorf("cold-start violated: %v", err)
	}
	out32 := make([]float32, pixSize*pixSize)
	if err := r.ReadTile("temp", 0, maxZoom, 5, 5, out32); err != nil {
		t.Fatalf("read sample: %v", err)
	}
}

func TestMissingTileNotFound(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sparse.wmt")
	const pixSize = 128

	tile := encoder.Tile{
		Variable: "x", TimeStep: 0, Z: 0, X: 0, Y: 0,
		Pixels: make([]float32, pixSize*pixSize),
	}

	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               2,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 1),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "x", Unit: ""}},
	}
	if err := encoder.Encode([]encoder.Tile{tile}, opts, out); err != nil {
		t.Fatal(err)
	}
	r, err := reader.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out32 := make([]float32, pixSize*pixSize)
	if err := r.ReadTile("x", 0, 2, 1, 1, out32); err != reader.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestAppendRoundtrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "append.wmt")

	const pixSize = 128
	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	initial := makeTiles("temp", pixSize, 1, 2)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               1,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 2),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(initial, opts, out); err != nil {
		t.Fatalf("initial encode: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if got := r.Header.SnapshotGeneration; got != 0 {
		t.Errorf("initial generation %d, want 0", got)
	}
	r.Close()

	ctx, err := encoder.OpenForAppend(out, encoder.AppendOptions{})
	if err != nil {
		t.Fatalf("OpenForAppend: %v", err)
	}
	if _, err := ctx.RegisterVariable(encoder.VariableSpec{Name: "precip", Unit: "mm/h"}); err != nil {
		t.Fatalf("register variable: %v", err)
	}
	for ti := uint32(0); ti < 2; ti++ {
		if err := ctx.DeclareBlock(encoder.BlockSpec{
			Variable: "precip", TimeStep: ti, ValueMin: 0, ValueMax: 100,
		}); err != nil {
			t.Fatalf("declare: %v", err)
		}
	}
	for _, src := range makeTiles("precip", pixSize, 1, 2) {
		if err := ctx.Submit(src); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	if err := ctx.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	r, err = reader.Open(out)
	if err != nil {
		t.Fatalf("post-append open: %v", err)
	}
	defer r.Close()
	if got := r.Header.SnapshotGeneration; got != 1 {
		t.Errorf("post-append generation %d, want 1", got)
	}
	if r.Header.Flags&format.FlagHasPreviousSnapshot == 0 {
		t.Errorf("FlagHasPreviousSnapshot should be set")
	}
	if _, ok := r.VariableID("temp"); !ok {
		t.Errorf("temp lost after append")
	}
	if _, ok := r.VariableID("precip"); !ok {
		t.Errorf("precip not found after append")
	}
	out32 := make([]float32, pixSize*pixSize)
	if err := r.ReadTile("temp", 1, 1, 1, 0, out32); err != nil {
		t.Errorf("read pre-append tile: %v", err)
	}
	if err := r.ReadTile("precip", 1, 1, 0, 0, out32); err != nil {
		t.Errorf("read appended tile: %v", err)
	}
}

func TestAppendRejectsTimeOutsideCatalog(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "append-time-range.wmt")

	const pixSize = 128
	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog:           regularTimeCatalog(refTime, 3600_000, 1),
		BBox:                  [4]float64{-180, -85, 180, 85},
		Variables:             []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
	}
	if err := encoder.Encode(makeTiles("temp", pixSize, 0, 1), opts, out); err != nil {
		t.Fatalf("initial encode: %v", err)
	}

	ctx, err := encoder.OpenForAppend(out, encoder.AppendOptions{})
	if err != nil {
		t.Fatalf("OpenForAppend: %v", err)
	}
	defer ctx.Close()

	if err := ctx.DeclareBlock(encoder.BlockSpec{
		Variable: "temp", TimeStep: 1, ValueMin: 250, ValueMax: 300,
	}); err == nil {
		t.Fatal("DeclareBlock outside time catalog succeeded")
	}
	if err := ctx.Submit(encoder.Tile{
		Variable: "temp",
		TimeStep: 1,
		Z:        0,
		X:        0,
		Y:        0,
		Pixels:   make([]float32, pixSize*pixSize),
	}); err == nil {
		t.Fatal("Submit outside time catalog succeeded")
	}
}
