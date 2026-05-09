package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireHDF5Fixture(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "data", name))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("test data not present: %s", abs)
	}
	return abs
}

func buildWMTiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "wmtiles")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runCLI(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\noutput:\n%s", bin, args, err, out)
	}
	return string(out)
}

func TestEncodeCompareExtend_HDF5(t *testing.T) {
	src1 := requireHDF5Fixture(t, "composite_wn_20260508_1510_000-hd5")
	src2 := requireHDF5Fixture(t, "composite_wn_20260508_1510_005-hd5")

	bin := buildWMTiles(t)
	tmp := t.TempDir()
	out := filepath.Join(tmp, "odim.wmt")

	// encode
	o := runCLI(t, bin, "encode", src1, "-o", out)
	if !strings.Contains(o, "Done") {
		t.Fatalf("encode missing Done section:\n%s", o)
	}

	// inspect: 1 variable, 1 block
	o = runCLI(t, bin, "inspect", out)
	if !strings.Contains(o, "blocks:              1") {
		t.Errorf("inspect post-encode missing 'blocks: 1':\n%s", o)
	}
	if !strings.Contains(o, "dbzh") {
		t.Errorf("inspect missing dbzh variable:\n%s", o)
	}

	// compare HDF5 source vs WMT
	o = runCLI(t, bin, "compare", src1, out)
	if !strings.Contains(o, "status:              ok") {
		t.Errorf("compare did not report status ok:\n%s", o)
	}
	if !strings.Contains(o, "100.0000% ok") {
		t.Errorf("compare did not reach 100%% within tolerance:\n%s", o)
	}

	// extend with the next file
	o = runCLI(t, bin, "extend", out, src2)
	if !strings.Contains(o, "Done") {
		t.Fatalf("extend missing Done section:\n%s", o)
	}

	// inspect post-extend: 2 blocks (one per timestep)
	o = runCLI(t, bin, "inspect", out)
	if !strings.Contains(o, "blocks:              2") {
		t.Errorf("inspect post-extend missing 'blocks: 2':\n%s", o)
	}

	// verify
	o = runCLI(t, bin, "verify", out)
	if !strings.Contains(o, "status:              ok") {
		t.Errorf("verify failed:\n%s", o)
	}
}

func TestEncode_FormatOverride_HDF5(t *testing.T) {
	src := requireHDF5Fixture(t, "composite_wn_20260508_1510_000-hd5")
	bin := buildWMTiles(t)
	out := filepath.Join(t.TempDir(), "explicit.wmt")
	o := runCLI(t, bin, "encode", "--format", "hdf5", src, "-o", out)
	if !strings.Contains(o, "Done") {
		t.Fatalf("encode --format hdf5 failed:\n%s", o)
	}

	// Override to grib2 should error (HDF5 file isn't GRIB).
	cmd := exec.Command(bin, "encode", "--format", "grib2", src, "-o", filepath.Join(t.TempDir(), "wrong.wmt"))
	out2, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("encode --format grib2 on HDF5 file unexpectedly succeeded:\n%s", out2)
	}
	if !strings.Contains(string(out2), "GRIB") {
		t.Errorf("expected GRIB-related error message, got:\n%s", out2)
	}
}
