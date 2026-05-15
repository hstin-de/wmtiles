package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
)

func main() {
	outDir := "format/testdata"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		die(err)
	}

	const pixSize = 128
	refTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	minimalPath := filepath.Join(outDir, "minimal.wmt")
	makeMinimal(minimalPath, pixSize, refTime, fixedNow)
	fmt.Printf("wrote %s\n", minimalPath)

	extendedPath := filepath.Join(outDir, "extended.wmt")
	makeExtended(extendedPath, pixSize, refTime, fixedNow)
	fmt.Printf("wrote %s\n", extendedPath)

	compactedPath := filepath.Join(outDir, "compacted.wmt")
	makeCompacted(extendedPath, compactedPath, pixSize, refTime, fixedNow)
	fmt.Printf("wrote %s\n", compactedPath)

	crcPath := filepath.Join(outDir, "crc_corrupted.wmt")
	makeCRCCorrupted(extendedPath, crcPath)
	fmt.Printf("wrote %s\n", crcPath)

	multistepPath := filepath.Join(outDir, "multistep.wmt")
	makeMultistep(multistepPath, pixSize, refTime, fixedNow)
	fmt.Printf("wrote %s\n", multistepPath)

	rawGridPath := filepath.Join(outDir, "rawgrid.wmt")
	makeRawGrid(rawGridPath, refTime, fixedNow)
	fmt.Printf("wrote %s\n", rawGridPath)
}

// makeRawGrid writes a small --no-tiles file with a deterministic synthetic
// grid. Used by the JS round-trip tests to lock in raw-grid sample values.
func makeRawGrid(path string, refTime time.Time, now time.Time) {
	const nx, ny = 64, 33
	const lat0, lon0 = 30.0, 0.0
	const dy, dx = 0.5, 0.5
	values := make([]float32, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			// pixel = lat + lon/1000, deterministic and easy to verify.
			values[y*nx+x] = float32(lat0+float64(y)*dy) + float32(lon0+float64(x)*dx)/1000
		}
	}

	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: 0, Count: 1,
		},
		BBox: [4]float64{lon0, lat0, lon0 + float64(nx-1)*dx, lat0 + float64(ny-1)*dy},
		Variables: []encoder.VariableSpec{
			{Name: "temp", Unit: "K", Precision: 0.001},
		},
		CreationTime:        now,
		SkipInternalWorkers: true,
	}
	enc, err := encoder.NewStreamingEncoder(opts, path)
	if err != nil {
		die(err)
	}
	if err := enc.EncodeRawGridBlock(encoder.RawGridSpec{
		Variable: "temp", TimeStep: 0,
		Nx: nx, Ny: ny,
		Lat0: lat0, Lon0: lon0,
		DY: dy, DX: dx,
		Precision: 0.001,
	}, values); err != nil {
		die(err)
	}
	if err := enc.Finish(); err != nil {
		die(err)
	}
}

// 4 hourly steps × 2 variables. Pixel values encode (timeStep, variable) so a
// reader can verify both slicing and per-variable routing.
func makeMultistep(path string, pixSize int, refTime time.Time, now time.Time) {
	const steps = 4
	const hourMs = int64(3600 * 1000)
	vars := []struct {
		Name   string
		Offset float32
	}{
		{"temp", 100},
		{"wind", 200},
	}

	tiles := make([]encoder.Tile, 0, steps*len(vars))
	for _, v := range vars {
		for t := uint32(0); t < steps; t++ {
			px := make([]float32, pixSize*pixSize)
			for i := range px {
				px[i] = v.Offset + float32(t)
			}
			tiles = append(tiles, encoder.Tile{
				Variable: v.Name, TimeStep: t, Z: 0, X: 0, Y: 0, Pixels: px,
			})
		}
	}

	specs := make([]encoder.VariableSpec, 0, len(vars))
	for _, v := range vars {
		specs = append(specs, encoder.VariableSpec{Name: v.Name, Unit: "u"})
	}
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: hourMs, Count: steps,
		},
		BBox:         [4]float64{-180, -85, 180, 85},
		Variables:    specs,
		CreationTime: now,
	}
	if err := encoder.Encode(tiles, opts, path); err != nil {
		die(err)
	}
}

func makeMinimal(path string, pixSize int, refTime time.Time, now time.Time) {
	px := make([]float32, pixSize*pixSize)
	for i := range px {
		px[i] = float32(i % 100)
	}
	tile := encoder.Tile{Variable: "temp", TimeStep: 0, Z: 0, X: 0, Y: 0, Pixels: px}

	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: 0, Count: 1,
		},
		BBox:         [4]float64{-180, -85, 180, 85},
		Variables:    []encoder.VariableSpec{{Name: "temp", Unit: "K"}},
		CreationTime: now,
	}
	if err := encoder.Encode([]encoder.Tile{tile}, opts, path); err != nil {
		die(err)
	}
}

func makeExtended(path string, pixSize int, refTime time.Time, now time.Time) {
	makeMinimal(path, pixSize, refTime, now)

	for _, name := range []string{"wind", "precip"} {
		ctx, err := encoder.OpenForAppend(path, encoder.AppendOptions{
			CreationTime: now,
		})
		if err != nil {
			die(err)
		}
		if _, err := ctx.RegisterVariable(encoder.VariableSpec{Name: name, Unit: "u"}); err != nil {
			die(err)
		}
		if err := ctx.DeclareBlock(encoder.BlockSpec{
			Variable: name, TimeStep: 0, ValueMin: 0, ValueMax: 50,
		}); err != nil {
			die(err)
		}
		px := make([]float32, pixSize*pixSize)
		for i := range px {
			px[i] = float32(i % 50)
		}
		if err := ctx.Submit(encoder.Tile{
			Variable: name, TimeStep: 0, Z: 0, X: 0, Y: 0, Pixels: px,
		}); err != nil {
			die(err)
		}
		if err := ctx.Finish(); err != nil {
			die(err)
		}
	}
}

func makeCompacted(_ string, output string, pixSize int, refTime time.Time, now time.Time) {
	tiles := make([]encoder.Tile, 0, 3)
	for _, name := range []string{"temp", "wind", "precip"} {
		px := make([]float32, pixSize*pixSize)
		mod := 100
		if name != "temp" {
			mod = 50
		}
		for i := range px {
			px[i] = float32(i % mod)
		}
		tiles = append(tiles, encoder.Tile{
			Variable: name, TimeStep: 0, Z: 0, X: 0, Y: 0, Pixels: px,
		})
	}
	opts := encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular: true, StartMs: refTime.UnixMilli(), IntervalMs: 0, Count: 1,
		},
		BBox: [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "temp", Unit: "K"},
			{Name: "wind", Unit: "u"},
			{Name: "precip", Unit: "u"},
		},
		CreationTime: now,
	}
	if err := encoder.Encode(tiles, opts, output); err != nil {
		die(err)
	}
}

func makeCRCCorrupted(input, output string) {
	data, err := os.ReadFile(input)
	if err != nil {
		die(err)
	}
	h, err := format.UnmarshalHeader(data[:format.HeaderSize])
	if err != nil {
		die(err)
	}
	corrupt := h.ActiveSnapshotOffset + h.ActiveSnapshotLength/2
	data[corrupt] ^= 0xFF
	if err := os.WriteFile(output, data, 0o644); err != nil {
		die(err)
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "gen-testdata:", err)
	os.Exit(1)
}
