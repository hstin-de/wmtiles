package tileid

import (
	"math/bits"

	"github.com/hstin-de/wmtiles/hilbert"
)

// (4^z - 1)/3: sum of tile counts at zooms 0..z-1, so all zoom levels share one ID space
func ZoomOffset(z uint8) uint64 {
	return ((uint64(1) << (2 * z)) - 1) / 3
}

func NumXYZ(maxZoom uint8) uint64 {
	return ZoomOffset(maxZoom + 1)
}

func Encode3D(z uint8, x, y uint32) uint64 {
	return ZoomOffset(z) + hilbert.XY2D(z, x, y)
}

func Decode3D(maxZoom uint8, tileID uint64) (z uint8, x, y uint32) {
	// invert ZoomOffset: floor(log2(3*tid+1))/2 recovers z
	zi := uint8((bits.Len64(3*tileID+1) - 1) / 2)
	if zi > maxZoom {
		return 0, 0, 0
	}
	gx, gy := hilbert.D2XY(zi, tileID-ZoomOffset(zi))
	return zi, gx, gy
}
