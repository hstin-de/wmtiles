package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
)

func TestCLIInspectVerify(t *testing.T) {
	dir := t.TempDir()

	const pixSize = 128
	pixCount := pixSize * pixSize

	tiles := make([]encoder.Tile, 2)
	for i := range 2 {
		px := make([]float32, pixCount)
		for j := range px {
			px[j] = float32(250 + i*10 + (j % 37))
		}
		tiles[i] = encoder.Tile{
			Variable: "air_temperature",
			TimeStep: uint32(i),
			Z:        0,
			X:        0,
			Y:        0,
			Pixels:   px,
		}
	}

	outPath := filepath.Join(dir, "out.wmt")
	refTime := time.Unix(0, 0).UTC()
	if err := encoder.Encode(tiles, encoder.Options{
		TilePixelSizeLog2:     7,
		MinZoom:               0,
		MaxZoom:               0,
		ReferenceForecastTime: refTime,
		TimeCatalog: format.TimeCatalog{
			Regular:    true,
			StartMs:    refTime.UnixMilli(),
			IntervalMs: 3600_000,
			Count:      2,
		},
		BBox: [4]float64{-180, -85, 180, 85},
		Variables: []encoder.VariableSpec{
			{Name: "air_temperature", Unit: "K", ColormapHint: "viridis"},
		},
		Metadata: map[string]any{"source": "synthetic"},
	}, outPath); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "wmtiles")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("output missing: %v", err)
	}

	if out, err := exec.Command(bin, "inspect", outPath).CombinedOutput(); err != nil {
		t.Fatalf("inspect: %v\n%s", err, out)
	} else if !bytes.Contains(out, []byte("air_temperature")) {
		t.Errorf("inspect output missing variable name:\n%s", out)
	}

	if out, err := exec.Command(bin, "verify", outPath).CombinedOutput(); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	} else if !bytes.Contains(out, []byte("ok")) {
		t.Errorf("verify did not report ok:\n%s", out)
	}
}

func TestCLIEncodeUsageIsGRIBOnly(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wmtiles")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "encode").CombinedOutput()
	if err == nil {
		t.Fatalf("encode without args succeeded unexpectedly:\n%s", out)
	}
	if !bytes.Contains(out, []byte("usage: wmtiles encode <input.grib2> -o <output.wmt>")) {
		t.Fatalf("encode usage should point at GRIB input:\n%s", out)
	}
	if bytes.Contains(bytes.ToLower(out), []byte("manifest")) {
		t.Fatalf("encode usage should not mention manifests:\n%s", out)
	}

	help, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v\n%s", err, help)
	}
	if bytes.Contains(bytes.ToLower(help), []byte("manifest")) {
		t.Fatalf("help should not mention manifests:\n%s", help)
	}
}

func TestParseEncodeAllowsInputBeforeFlags(t *testing.T) {
	flags, in, err := parseGribEncodeFlags("encode", []string{
		"input.grib2",
		"-o", "out.wmt",
		"--min-zoom", "1",
		"--max-zoom=2",
		"--tile-size-log2", "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in != "input.grib2" {
		t.Fatalf("input = %q, want input.grib2", in)
	}
	if flags.output != "out.wmt" || flags.minZoom != 1 || flags.maxZoom != 2 || flags.tileSizeLog2 != 7 {
		t.Fatalf("flags parsed incorrectly: %+v", flags)
	}
}
