package parser

import (
	"math"
	"path/filepath"
	"testing"
)

func TestParseODIMRadarComposite(t *testing.T) {
	path, err := filepath.Abs("../data/composite_wn_20260508_1510_000-hd5")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !IsHDF5File(path) {
		t.Skipf("test data not present: %s", path)
	}
	files, err := ParseHDF5File(path)
	if err != nil {
		t.Fatalf("ParseHDF5File: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d records, want 1 (one quantity per ODIM file)", len(files))
	}
	gf := files[0]
	h := gf.Header
	if h.ShortName != "dbzh" {
		t.Errorf("ShortName = %q, want \"dbzh\"", h.ShortName)
	}
	if h.Units != "dBZ" {
		t.Errorf("Units = %q, want \"dBZ\"", h.Units)
	}
	if h.Nx <= 0 || h.Ny <= 0 {
		t.Fatalf("grid dims invalid: %dx%d", h.Nx, h.Ny)
	}
	// after reprojection the grid covers roughly the German radar footprint
	if !(h.La1 > 44 && h.La1 < 47) {
		t.Errorf("La1 = %v out of expected range (~45°)", h.La1)
	}
	if !(h.La2 > 54 && h.La2 < 57) {
		t.Errorf("La2 = %v out of expected range (~56°)", h.La2)
	}
	if !(h.Lo1 > 0 && h.Lo1 < 4) {
		t.Errorf("Lo1 = %v out of expected range (~1.5°)", h.Lo1)
	}
	if !(h.Lo2 > 17 && h.Lo2 < 20) {
		t.Errorf("Lo2 = %v out of expected range (~19°)", h.Lo2)
	}
	if h.DX <= 0 || h.DY <= 0 {
		t.Errorf("grid spacing not positive: dx=%v dy=%v", h.DX, h.DY)
	}
	if len(gf.DataValues) != h.Nx*h.Ny {
		t.Fatalf("DataValues len = %d, want %d", len(gf.DataValues), h.Nx*h.Ny)
	}
	missing := float32(h.MissingValue)
	finiteCount := 0
	var vmin, vmax float32 = math.MaxFloat32, -math.MaxFloat32
	for _, v := range gf.DataValues {
		if v == missing || v != v {
			continue
		}
		finiteCount++
		if v < vmin {
			vmin = v
		}
		if v > vmax {
			vmax = v
		}
	}
	if finiteCount == 0 {
		t.Fatalf("no finite pixels found (all missing)")
	}
	// physical DBZH range is roughly -32..+95 dBZ
	if vmin < -64 || vmax > 100 {
		t.Errorf("dBZ range [%v, %v] looks wrong", vmin, vmax)
	}
}
