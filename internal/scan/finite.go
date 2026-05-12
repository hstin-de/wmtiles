package scan

import "math"

// NaNs and the GRIB-reported missing sentinel must not pollute the per-block
// range; that's what would push the auto-cap or quantize scale off the cliff.
func FiniteRange(values []float32, missing float64) (vmin, vmax float64, ok bool) {
	missing32 := float32(missing)
	mn := float32(math.Inf(+1))
	mx := float32(math.Inf(-1))
	hasFin := false
	for _, v := range values {
		if v != v || v == missing32 {
			continue
		}
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		hasFin = true
	}
	if hasFin {
		return float64(mn), float64(mx), true
	}
	return 0, 0, false
}
