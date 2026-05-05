package hilbert

// standard Hilbert curve XY↔D: quadrant rotation per level keeps neighbouring
// tile IDs spatially adjacent, which makes range coalescing actually work
func XY2D(z uint8, x, y uint32) uint64 {
	var d uint64
	n := uint32(1) << z
	xi, yi := x, y
	for s := n / 2; s > 0; s /= 2 {
		var rx, ry uint32
		if xi&s > 0 {
			rx = 1
		}
		if yi&s > 0 {
			ry = 1
		}
		d += uint64(s) * uint64(s) * uint64((3*rx)^ry)
		if ry == 0 {
			if rx == 1 {
				xi = s - 1 - xi
				yi = s - 1 - yi
			}
			xi, yi = yi, xi
		}
	}
	return d
}

func D2XY(z uint8, d uint64) (uint32, uint32) {
	var x, y uint32
	n := uint32(1) << z
	t := d
	for s := uint32(1); s < n; s *= 2 {
		rx := uint32(1) & uint32(t/2)
		ry := uint32(1) & uint32(t^uint64(rx))
		if ry == 0 {
			if rx == 1 {
				x = s - 1 - x
				y = s - 1 - y
			}
			x, y = y, x
		}
		x += s * rx
		y += s * ry
		t /= 4
	}
	return x, y
}
