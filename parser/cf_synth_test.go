package parser

import (
	"path/filepath"
	"testing"

	"github.com/hstin-de/wmtiles/parser/internal/cfsynth"
)

func TestParseCFSynthetic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synth.h5")
	lats := []float64{50, 51, 52, 53, 54}
	lons := []float64{10, 11, 12, 13}
	times := []float64{0, 3600}
	const fill = float32(-9999)
	const scale = float32(0.1)
	const offset = float32(273.15)
	vals := make([]float32, len(times)*len(lats)*len(lons))
	for i := range vals {
		vals[i] = float32(i % 200)
	}
	vals[1*len(lats)*len(lons)+0] = fill

	if err := cfsynth.WriteCFFile(path, lats, lons, times, vals, fill, scale, offset); err != nil {
		t.Fatalf("write CF synthetic: %v", err)
	}
	if !IsHDF5File(path) {
		t.Fatalf("synthetic file not recognised as HDF5")
	}

	files, err := ParseHDF5File(path)
	if err != nil {
		t.Fatalf("ParseHDF5File: %v", err)
	}
	if len(files) != len(times) {
		t.Fatalf("got %d records, want %d (one per timestep)", len(files), len(times))
	}
	for tIdx, gf := range files {
		h := gf.Header
		if h.ShortName != "t" {
			t.Errorf("t=%d ShortName = %q, want \"t\" (mapped from air_temperature)", tIdx, h.ShortName)
		}
		if h.Units != "K" {
			t.Errorf("t=%d Units = %q, want \"K\"", tIdx, h.Units)
		}
		if h.Nx != len(lons) || h.Ny != len(lats) {
			t.Errorf("t=%d grid %dx%d, want %dx%d", tIdx, h.Nx, h.Ny, len(lons), len(lats))
		}
		if h.La1 != lats[0] || h.La2 != lats[len(lats)-1] {
			t.Errorf("t=%d lat range [%v..%v], want [%v..%v]", tIdx, h.La1, h.La2, lats[0], lats[len(lats)-1])
		}
		if h.Lo1 != lons[0] || h.Lo2 != lons[len(lons)-1] {
			t.Errorf("t=%d lon range [%v..%v], want [%v..%v]", tIdx, h.Lo1, h.Lo2, lons[0], lons[len(lons)-1])
		}
		if len(gf.DataValues) != h.Nx*h.Ny {
			t.Errorf("t=%d DataValues len %d, want %d", tIdx, len(gf.DataValues), h.Nx*h.Ny)
		}
		if tIdx == 0 {
			got := gf.DataValues[0]
			want := float32(0)*scale + offset
			if abs32(got-want) > 1e-3 {
				t.Errorf("t=0 (0,0): got %v, want %v after scale/offset", got, want)
			}
		}
		if tIdx == 1 {
			got := gf.DataValues[0]
			missing := float32(h.MissingValue)
			if got != missing {
				t.Errorf("t=1 (0,0): got %v, want %v (missing sentinel)", got, missing)
			}
		}
	}

	t0 := files[0].Header.ReferenceTime
	t1 := files[1].Header.ReferenceTime
	if t0.IsZero() || t1.IsZero() {
		t.Fatalf("time axis missing: t0=%v t1=%v", t0, t1)
	}
	if gap := t1.Sub(t0); gap.Seconds() != 3600 {
		t.Errorf("time gap = %v, want 1h", gap)
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
