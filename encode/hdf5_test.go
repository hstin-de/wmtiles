package encode_test

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/reader"
)

func TestEncodeODIMRadarComposite(t *testing.T) {
	src, err := filepath.Abs("../data/composite_wn_20260508_1510_000-hd5")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !parser.IsHDF5File(src) {
		t.Skipf("test data not present: %s", src)
	}

	out := filepath.Join(t.TempDir(), "odim.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		TileSize: 256,
		MinZoom:  0,
		MaxZoom:  4,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.AddFile(src, encode.FormatHDF5); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("reader.Open: %v", err)
	}
	defer r.Close()

	if got := len(r.Snapshot.Variables); got != 1 {
		t.Fatalf("got %d variables, want 1", got)
	}
	v := r.Snapshot.Variables[0]
	if v.Name != "dbzh" {
		t.Errorf("Name = %q, want \"dbzh\"", v.Name)
	}
	if v.Unit != "dBZ" {
		t.Errorf("Unit = %q, want \"dBZ\"", v.Unit)
	}
	if math.IsNaN(v.ValueMinObservedGlobal) || math.IsNaN(v.ValueMaxObservedGlobal) {
		t.Fatalf("variable has no observed range; encoder saw no finite pixels")
	}
	if v.ValueMaxObservedGlobal < 0 {
		t.Errorf("max dBZ = %v, expected at least one positive return", v.ValueMaxObservedGlobal)
	}

	blocks := 0
	totalAddressed := uint64(0)
	if err := r.EachBlock(func(e format.BlockTableEntry) error {
		blocks++
		totalAddressed += e.NumAddressedTiles
		return nil
	}); err != nil {
		t.Fatalf("EachBlock: %v", err)
	}
	if blocks == 0 {
		t.Fatalf("no blocks written")
	}
	if totalAddressed == 0 {
		t.Fatalf("blocks written but no tiles addressed")
	}

	// scan the pyramid for any tile with finite pixels — the world-spanning
	// (z=0,0,0) tile is mostly missing for a Germany-only dataset.
	pix := r.PixelCount()
	out0 := make([]float32, pix)
	decoded := false
	for z := r.Header.MinZoom; z <= r.Header.MaxZoom && !decoded; z++ {
		n := uint32(1) << z
		for x := uint32(0); x < n && !decoded; x++ {
			for y := uint32(0); y < n && !decoded; y++ {
				if err := r.ReadTile("dbzh", 0, z, x, y, out0); err != nil {
					continue
				}
				for _, p := range out0 {
					if !math.IsNaN(float64(p)) && p != float32(9999) {
						decoded = true
						break
					}
				}
			}
		}
	}
	if !decoded {
		t.Fatalf("could not decode any tile with finite values across z=%d..%d",
			r.Header.MinZoom, r.Header.MaxZoom)
	}
}

func TestEncodeODIMSeries(t *testing.T) {
	matches, err := filepath.Glob("../data/composite_wn_20260508_1510_0[0-2]*-hd5")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) < 2 {
		t.Skip("need at least 2 ODIM files for series test")
	}

	out := filepath.Join(t.TempDir(), "odim_series.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		TileSize: 256,
		MinZoom:  0,
		MaxZoom:  3,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	for _, m := range matches {
		if err := enc.AddFile(m, encode.FormatHDF5); err != nil {
			t.Fatalf("AddFile %s: %v", m, err)
		}
	}
	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	r, err := reader.Open(out)
	if err != nil {
		t.Fatalf("reader.Open: %v", err)
	}
	defer r.Close()

	if got := int(r.Snapshot.TimeCat.Count); got != len(matches) {
		t.Errorf("time axis count = %d, want %d", got, len(matches))
	}
}
