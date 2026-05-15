package encode_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/decode"
	"github.com/hstin-de/wmtiles/encode"
)

// TestEncoderNoTilesRoundTrip encodes a single AddArray input in --no-tiles mode
// and verifies bilinear samples come back near-exact within the requested
// quantisation step.
func TestEncoderNoTilesRoundTrip(t *testing.T) {
	const nx, ny = 320, 161
	const lon0, lat0 = -10.0, 35.0
	const dx, dy = 0.25, 0.25

	data := make([]float32, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			// Smooth field with a clear lat/lon signature.
			data[y*nx+x] = float32(lat0+float64(y)*dy)*2 + float32(lon0+float64(x)*dx)/4
		}
	}

	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	out := filepath.Join(t.TempDir(), "raw.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		NoTiles:   true,
		Precision: map[string]float64{"my_var": 0.01},
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.AddArray(encode.ArrayInput{
		Variable:      "my_var",
		Unit:          "K",
		ReferenceTime: t0,
		Grid: encode.GridSpec{
			Nx: nx, Ny: ny,
			Lon0: lon0, Lat0: lat0,
			DX: dx, DY: dy,
			MissingValue: math.NaN(),
		},
		Data: data,
	}); err != nil {
		t.Fatalf("AddArray: %v", err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	t.Logf("encoded raw-grid file: %d bytes for %dx%d grid", info.Size(), nx, ny)

	d, err := decode.Open(out)
	if err != nil {
		t.Fatalf("decode.Open: %v", err)
	}
	defer d.Close()

	raw, err := d.IsRawGridBlock("my_var", 0)
	if err != nil {
		t.Fatalf("IsRawGridBlock: %v", err)
	}
	if !raw {
		t.Fatalf("IsRawGridBlock = false, want true")
	}

	// Tile reads must reject raw-grid blocks.
	if _, err := d.ReadTile("my_var", 0, decode.Coord(0, 0, 0)); err != decode.ErrRawGridBlock {
		t.Fatalf("ReadTile err = %v, want ErrRawGridBlock", err)
	}

	// Exact grid points roundtrip within precision.
	for _, p := range []struct{ x, y int }{{0, 0}, {nx / 2, ny / 2}, {nx - 1, ny - 1}, {5, 7}} {
		lat := lat0 + float64(p.y)*dy
		lon := lon0 + float64(p.x)*dx
		got, err := d.Sample("my_var", 0, lat, lon)
		if err != nil {
			t.Fatalf("Sample(%d,%d): %v", p.x, p.y, err)
		}
		want := data[p.y*nx+p.x]
		if math.Abs(float64(got-want)) > 0.02 {
			t.Errorf("Sample(%d,%d): got %g, want %g", p.x, p.y, got, want)
		}
	}

	// Interpolated points should land between known cells.
	{
		got, err := d.Sample("my_var", 0, lat0+0.5*dy, lon0+0.5*dx)
		if err != nil {
			t.Fatalf("Sample interp: %v", err)
		}
		want := (data[0*nx+0] + data[0*nx+1] + data[1*nx+0] + data[1*nx+1]) / 4
		if math.Abs(float64(got-want)) > 0.05 {
			t.Errorf("Sample interp: got %g, want ~%g", got, want)
		}
	}

	// Out-of-bounds returns NaN.
	if got, err := d.Sample("my_var", 0, 90, 0); err != nil {
		t.Fatalf("Sample oob: %v", err)
	} else if !math.IsNaN(float64(got)) {
		t.Errorf("Sample oob: got %g, want NaN", got)
	}

	// Samples batch path.
	pts := []decode.SamplePoint{
		{Lat: lat0, Lon: lon0},
		{Lat: lat0 + 2*dy, Lon: lon0 + 3*dx},
		{Lat: lat0 + 4*dy, Lon: lon0 + 4*dx},
	}
	vals, err := d.Samples("my_var", 0, pts)
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(vals) != len(pts) {
		t.Fatalf("Samples: got %d values, want %d", len(vals), len(pts))
	}
	for i, v := range vals {
		single, err := d.Sample("my_var", 0, pts[i].Lat, pts[i].Lon)
		if err != nil {
			t.Fatalf("Sample[%d]: %v", i, err)
		}
		if math.Abs(float64(v-single)) > 1e-5 {
			t.Errorf("Samples[%d] = %g, Sample = %g", i, v, single)
		}
	}
}

func TestEncoderNoTilesWithMissing(t *testing.T) {
	const nx, ny = 32, 32
	const lon0, lat0 = 0.0, 0.0
	const dx, dy = 1.0, 1.0
	const sentinel float32 = -9999

	data := make([]float32, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			if x < 4 || y < 4 {
				data[y*nx+x] = sentinel
			} else {
				data[y*nx+x] = float32(x + y)
			}
		}
	}

	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	out := filepath.Join(t.TempDir(), "missing.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		NoTiles: true,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.AddArray(encode.ArrayInput{
		Variable:      "v",
		Unit:          "",
		ReferenceTime: t0,
		Grid: encode.GridSpec{
			Nx: nx, Ny: ny,
			Lon0: lon0, Lat0: lat0,
			DX: dx, DY: dy,
			MissingValue: -9999,
		},
		Data: data,
	}); err != nil {
		t.Fatalf("AddArray: %v", err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	d, err := decode.Open(out)
	if err != nil {
		t.Fatalf("decode.Open: %v", err)
	}
	defer d.Close()

	// Cell in the masked corner -> NaN propagates.
	got, err := d.Sample("v", 0, lat0+1.5*dy, lon0+1.5*dx)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if !math.IsNaN(float64(got)) {
		t.Errorf("masked sample = %g, want NaN", got)
	}

	// Cell in the unmasked interior -> finite.
	got, err = d.Sample("v", 0, lat0+10*dy, lon0+10*dx)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if math.IsNaN(float64(got)) {
		t.Errorf("interior sample is NaN, want finite")
	}
}
