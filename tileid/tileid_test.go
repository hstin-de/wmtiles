package tileid

import (
	"math/rand/v2"
	"testing"
)

func TestZoomOffset(t *testing.T) {
	cases := []struct {
		z    uint8
		want uint64
	}{
		{0, 0},
		{1, 1},
		{2, 5},
		{3, 21},
		{12, 5592405},
	}
	for _, c := range cases {
		if got := ZoomOffset(c.z); got != c.want {
			t.Errorf("ZoomOffset(%d)=%d, want %d", c.z, got, c.want)
		}
	}
}

func TestPMTilesCumulative(t *testing.T) {
	got := Encode3D(12, 3423, 1763)
	if got != 19078479 {
		t.Errorf("Encode3D(12, 3423, 1763) = %d, want 19078479", got)
	}
}

func Test3DRoundtrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	for range 5000 {
		z := uint8(rng.UintN(11))
		n := uint32(1) << z
		x := rng.Uint32N(n)
		y := rng.Uint32N(n)
		id := Encode3D(z, x, y)
		gz, gx, gy := Decode3D(z, id)
		if gz != z || gx != x || gy != y {
			t.Fatalf("(%d,%d,%d) → %d → (%d,%d,%d)", z, x, y, id, gz, gx, gy)
		}
	}
}

func TestSpatialMonotone(t *testing.T) {
	a := Encode3D(5, 16, 16)
	b := Encode3D(5, 17, 16)
	diff := int64(a) - int64(b)
	if diff < 0 {
		diff = -diff
	}
	if diff > 16 {
		t.Errorf("adjacent tiles at z=5 should be within Hilbert distance ~few, got %d", diff)
	}
}

func TestNumXYZ(t *testing.T) {
	if NumXYZ(0) != 1 {
		t.Errorf("NumXYZ(0)=%d, want 1", NumXYZ(0))
	}
	if NumXYZ(6) != 5461 {
		t.Errorf("NumXYZ(6)=%d, want 5461", NumXYZ(6))
	}
}
