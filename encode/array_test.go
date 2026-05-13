package encode_test

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/decode"
	"github.com/hstin-de/wmtiles/encode"
)

func TestEncoderAddArrayRoundTrip(t *testing.T) {
	const nx, ny = 64, 33
	const lon0, lat0 = 0.0, 30.0
	const dx, dy = 0.5, 0.5

	data := make([]float32, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			data[y*nx+x] = float32(lat0+float64(y)*dy) + float32(lon0+float64(x)*dx)/1000
		}
	}

	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	out := filepath.Join(t.TempDir(), "array.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		TileSize:  256,
		MinZoom:   0,
		MaxZoom:   2,
		Precision: map[string]float64{"my_var": 0.01},
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}

	for i, refT := range []time.Time{t0, t1} {
		payload := make([]float32, len(data))
		for j, v := range data {
			payload[j] = v + float32(i)
		}
		if err := enc.AddArray(encode.ArrayInput{
			Variable:      "my_var",
			Unit:          "K",
			ReferenceTime: refT,
			Grid: encode.GridSpec{
				Nx: nx, Ny: ny,
				Lon0: lon0, Lat0: lat0,
				DX: dx, DY: dy,
				MissingValue: math.NaN(),
			},
			Data: payload,
		}); err != nil {
			t.Fatalf("AddArray t%d: %v", i, err)
		}
	}

	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	d, err := decode.Open(out)
	if err != nil {
		t.Fatalf("decode.Open: %v", err)
	}
	defer d.Close()

	vars := d.Variables()
	if len(vars) != 1 || vars[0].Name != "my_var" {
		t.Fatalf("variables = %+v, want one named my_var", vars)
	}
	if vars[0].Unit != "K" {
		t.Errorf("unit = %q, want K", vars[0].Unit)
	}

	times := d.Times()
	if len(times) != 2 || !times[0].Equal(t0) || !times[1].Equal(t1) {
		t.Fatalf("times = %v, want [%s %s]", times, t0, t1)
	}

	b := d.Bounds()
	wantW, wantE := lon0, lon0+float64(nx-1)*dx
	wantS, wantN := lat0, lat0+float64(ny-1)*dy
	if math.Abs(b.West-wantW) > 1e-3 || math.Abs(b.East-wantE) > 1e-3 ||
		math.Abs(b.South-wantS) > 1e-3 || math.Abs(b.North-wantN) > 1e-3 {
		t.Errorf("bounds = %+v, want approx W=%g E=%g S=%g N=%g",
			b, wantW, wantE, wantS, wantN)
	}

	pixels, err := d.ReadTile("my_var", 0, decode.Coord(0, 0, 0))
	if err != nil {
		t.Fatalf("ReadTile: %v", err)
	}
	finite := 0
	for _, v := range pixels {
		if !math.IsNaN(float64(v)) {
			finite++
		}
	}
	if finite == 0 {
		t.Fatal("tile contained no finite samples")
	}
}

func TestEncoderAddArrayValidation(t *testing.T) {
	enc, err := encode.NewEncoder("out.wmt", encode.Options{})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	now := time.Now().UTC()

	cases := []struct {
		name string
		in   encode.ArrayInput
	}{
		{"empty variable", encode.ArrayInput{ReferenceTime: now, Grid: encode.GridSpec{Nx: 4, Ny: 4, DX: 1, DY: 1}, Data: make([]float32, 16)}},
		{"zero time", encode.ArrayInput{Variable: "v", Grid: encode.GridSpec{Nx: 4, Ny: 4, DX: 1, DY: 1}, Data: make([]float32, 16)}},
		{"1x1 grid", encode.ArrayInput{Variable: "v", ReferenceTime: now, Grid: encode.GridSpec{Nx: 1, Ny: 1, DX: 1, DY: 1}, Data: make([]float32, 1)}},
		{"zero DX", encode.ArrayInput{Variable: "v", ReferenceTime: now, Grid: encode.GridSpec{Nx: 4, Ny: 4, DY: 1}, Data: make([]float32, 16)}},
		{"length mismatch", encode.ArrayInput{Variable: "v", ReferenceTime: now, Grid: encode.GridSpec{Nx: 4, Ny: 4, DX: 1, DY: 1}, Data: make([]float32, 15)}},
	}
	for _, c := range cases {
		if err := enc.AddArray(c.in); err == nil {
			t.Errorf("AddArray(%s) succeeded; want error", c.name)
		}
	}
}
