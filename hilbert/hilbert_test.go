package hilbert

import (
	"math/rand/v2"
	"testing"
)

func TestPMTilesSpecVector(t *testing.T) {
	got := XY2D(12, 3423, 1763)
	want := uint64(13486074)
	if got != want {
		t.Errorf("XY2D(12, 3423, 1763) = %d, want %d", got, want)
	}
}

func TestRoundtrip(t *testing.T) {
	for z := uint8(0); z <= 10; z++ {
		n := uint32(1) << z
		if z <= 6 {
			for y := range n {
				for x := range n {
					d := XY2D(z, x, y)
					gx, gy := D2XY(z, d)
					if gx != x || gy != y {
						t.Fatalf("z=%d (%d,%d) → d=%d → (%d,%d)", z, x, y, d, gx, gy)
					}
				}
			}
		} else {
			rng := rand.New(rand.NewPCG(uint64(z), 0))
			for range 1000 {
				x := rng.Uint32N(n)
				y := rng.Uint32N(n)
				d := XY2D(z, x, y)
				gx, gy := D2XY(z, d)
				if gx != x || gy != y {
					t.Fatalf("z=%d (%d,%d) → d=%d → (%d,%d)", z, x, y, d, gx, gy)
				}
			}
		}
	}
}

func TestZ0(t *testing.T) {
	if d := XY2D(0, 0, 0); d != 0 {
		t.Errorf("z=0 (0,0) = %d, want 0", d)
	}
}

func TestZ1(t *testing.T) {
	cases := []struct {
		x, y uint32
		want uint64
	}{
		{0, 0, 0},
		{0, 1, 1},
		{1, 1, 2},
		{1, 0, 3},
	}
	for _, c := range cases {
		if got := XY2D(1, c.x, c.y); got != c.want {
			t.Errorf("z=1 (%d,%d) = %d, want %d", c.x, c.y, got, c.want)
		}
	}
}
