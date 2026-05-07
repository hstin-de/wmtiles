package decode_test

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/decode"
	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
)

func TestOpenReadTile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forecast.wmt")
	start := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)

	pixels := make([]float32, 128*128)
	for i := range pixels {
		pixels[i] = 280 + float32(i%128)*0.01
	}
	if err := encoder.Encode([]encoder.Tile{
		{Variable: "2t", TimeStep: 0, Z: 0, X: 0, Y: 0, Pixels: pixels},
	}, encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: start,
		TimeCatalog: format.TimeCatalog{
			Regular: true,
			StartMs: start.UnixMilli(),
			Count:   1,
		},
		BBox: [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "2t", Unit: "K", Precision: 0.001},
		},
	}, path); err != nil {
		t.Fatalf("encode: %v", err)
	}

	dec, err := decode.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dec.Close()

	if got := dec.TileSize(); got != 128 {
		t.Fatalf("TileSize = %d, want 128", got)
	}
	if vars := dec.Variables(); len(vars) != 1 || vars[0].Name != "2t" {
		t.Fatalf("Variables = %+v", vars)
	}

	out, err := dec.ReadTile("2t", 0, decode.Coord(0, 0, 0))
	if err != nil {
		t.Fatalf("ReadTile: %v", err)
	}
	for i := range out {
		if diff := math.Abs(float64(out[i] - pixels[i])); diff > 0.001 {
			t.Fatalf("pixel %d = %g, want %g", i, out[i], pixels[i])
		}
	}
}
