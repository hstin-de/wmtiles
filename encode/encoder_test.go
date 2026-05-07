package encode_test

import (
	"testing"

	"github.com/hstin-de/wmtiles/encode"
)

func TestEncoderValidation(t *testing.T) {
	if _, err := encode.NewEncoder("", encode.Options{}); err == nil {
		t.Fatal("NewEncoder with empty output path succeeded")
	}
	if _, err := encode.NewEncoder("out.wmt", encode.Options{
		Precision: map[string]float64{"2t": -1},
	}); err == nil {
		t.Fatal("NewEncoder with negative precision succeeded")
	}

	enc, err := encode.NewEncoder("out.wmt", encode.Options{})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.AddBytes("", encode.FormatGRIB2, nil); err == nil {
		t.Fatal("AddBytes with empty data succeeded")
	}
	if err := enc.AddBytes("missing-format.grib2", "", []byte("GRIB")); err == nil {
		t.Fatal("AddBytes with empty format succeeded")
	}
	if err := enc.Finish(); err == nil {
		t.Fatal("Finish without inputs succeeded")
	}
	if err := enc.AddBytes("late.grib2", encode.FormatGRIB2, []byte("GRIB")); err == nil {
		t.Fatal("AddBytes after Finish succeeded")
	}
}
