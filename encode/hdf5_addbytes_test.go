package encode_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/reader"
)

func TestEncoderAddBytesODIM(t *testing.T) {
	src, _ := filepath.Abs("../data/composite_wn_20260508_1510_000-hd5")
	if !parser.IsHDF5File(src) {
		t.Skipf("test data not present: %s", src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	out := filepath.Join(t.TempDir(), "from_bytes.wmt")
	enc, err := encode.NewEncoder(out, encode.Options{
		TileSize: 256,
		MinZoom:  0,
		MaxZoom:  2,
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.AddBytes("composite_wn-bytes", encode.FormatHDF5, data); err != nil {
		t.Fatalf("AddBytes: %v", err)
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
	if name := r.Snapshot.Variables[0].Name; name != "dbzh" {
		t.Errorf("variable name = %q, want \"dbzh\"", name)
	}
}
