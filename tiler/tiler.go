package tiler

import (
	"math"

	"github.com/hstin-de/wmtiles/parser"
)

func PixelToLatLon(z uint8, x, y uint32, pixSize, col, row int) (lat, lon float64) {
	n := float64(uint32(1) << z)
	xmerc := (float64(x) + (float64(col)+0.5)/float64(pixSize)) / n
	ymerc := (float64(y) + (float64(row)+0.5)/float64(pixSize)) / n
	lon = xmerc*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*ymerc))) * 180 / math.Pi
	return
}

func TileBBox(z uint8, x, y uint32) (west, south, east, north float64) {
	n := float64(uint32(1) << z)
	west = float64(x)/n*360 - 180
	east = float64(x+1)/n*360 - 180
	north = math.Atan(math.Sinh(math.Pi*(1-2*float64(y)/n))) * 180 / math.Pi
	south = math.Atan(math.Sinh(math.Pi*(1-2*float64(y+1)/n))) * 180 / math.Pi
	return
}

func GridBBox(g *parser.GRIBFile) (west, south, east, north float64) {
	la1 := g.Header.La1
	la2 := g.Header.La2
	lo1 := g.Header.Lo1
	lo2 := g.Header.Lo2
	dx := g.Header.DX

	north = math.Max(la1, la2)
	south = math.Min(la1, la2)

	if dx > 0 {
		span := math.Abs(lo2-lo1) + dx
		if span >= 360-dx*0.5 {
			west, east = -180, 180
			return
		}
	}

	if lo1 > 180 {
		lo1 -= 360
	}
	if lo2 > 180 {
		lo2 -= 360
	}
	west = math.Min(lo1, lo2)
	east = math.Max(lo1, lo2)
	return
}

func TilesIntersectingGrid(g *parser.GRIBFile, z uint8) []struct{ X, Y uint32 } {
	gw, gs, ge, gn := GridBBox(g)

	n := uint32(1) << z

	xMin := int(math.Floor((gw + 180) / 360 * float64(n)))
	xMax := max(int(math.Floor((ge+180)/360*float64(n)-1e-9)), xMin)
	if xMin < 0 {
		xMin = 0
	}
	if xMax >= int(n) {
		xMax = int(n) - 1
	}

	yMin := latToTileY(gn, z)
	yMax := latToTileY(gs, z)
	if yMin > yMax {
		yMin, yMax = yMax, yMin
	}
	if yMin < 0 {
		yMin = 0
	}
	if yMax >= int(n) {
		yMax = int(n) - 1
	}

	var out []struct{ X, Y uint32 }
	for y := yMin; y <= yMax; y++ {
		for x := xMin; x <= xMax; x++ {
			out = append(out, struct{ X, Y uint32 }{uint32(x), uint32(y)})
		}
	}
	return out
}

// Web Mercator forward: lat→tile-y at zoom z
func latToTileY(lat float64, z uint8) int {
	const cutoff = 85.05112877980659 // atan(sinh(π))·180/π: Web Mercator pole limit
	if lat > cutoff {
		lat = cutoff
	}
	if lat < -cutoff {
		lat = -cutoff
	}
	r := math.Log(math.Tan((90 + lat) * math.Pi / 360))
	n := float64(uint32(1) << z)
	return int(math.Floor((1 - r/math.Pi) / 2 * n))
}

type Sampler struct {
	g          *parser.GRIBFile
	lats, lons []float64
	latDescend bool
	lonDescend bool
	missing    float64

	uniform  bool
	lat0     float64
	lon0     float64
	dlat     float64
	dlon     float64
	invDLat  float64
	invDLon  float64
	nLatM1   int
	nLonM1   int
	lonGrid0 bool
}

func NewSampler(g *parser.GRIBFile) *Sampler {
	if len(g.Header.DistinctLatitudes) < 2 || len(g.Header.DistinctLongitudes) < 2 {
		return nil
	}
	s := &Sampler{
		g:       g,
		lats:    g.Header.DistinctLatitudes,
		lons:    g.Header.DistinctLongitudes,
		missing: g.Header.MissingValue,
	}
	s.latDescend = s.lats[0] > s.lats[len(s.lats)-1]
	s.lonDescend = s.lons[0] > s.lons[len(s.lons)-1]

	// uniform-grid fast path: precompute step/inverse so At() can index in O(1)
	// instead of binary-searching distinctLatitudes/Longitudes for every pixel
	s.uniform = isUniform(s.lats) && isUniform(s.lons)
	if s.uniform {
		s.lat0 = s.lats[0]
		s.lon0 = s.lons[0]
		s.nLatM1 = len(s.lats) - 1
		s.nLonM1 = len(s.lons) - 1
		s.dlat = (s.lats[s.nLatM1] - s.lats[0]) / float64(s.nLatM1)
		s.dlon = (s.lons[s.nLonM1] - s.lons[0]) / float64(s.nLonM1)
		s.invDLat = 1 / s.dlat
		s.invDLon = 1 / s.dlon
		s.lonGrid0 = s.lons[0] >= 180 || s.lons[s.nLonM1] >= 180
	} else {
		ref := s.lons[0]
		if s.lonDescend {
			ref = s.lons[len(s.lons)-1]
		}
		s.lonGrid0 = ref >= 180 || s.lons[len(s.lons)-1] >= 180
	}
	return s
}

func isUniform(arr []float64) bool {
	if len(arr) < 2 {
		return false
	}
	step := (arr[len(arr)-1] - arr[0]) / float64(len(arr)-1)
	if step == 0 {
		return false
	}
	tol := math.Abs(step) * 1e-6
	for i := 1; i < len(arr); i++ {
		expected := arr[0] + step*float64(i)
		if math.Abs(arr[i]-expected) > tol {
			return false
		}
	}
	return true
}

func (s *Sampler) At(lat, lon float64) float64 {
	if s.lonGrid0 {
		if lon < 0 {
			lon += 360
		}
	} else if lon > 180 {
		lon -= 360
	}

	var xFrac, yFrac float64
	var x0, y0 int
	if s.uniform {
		yFrac = (lat - s.lat0) * s.invDLat
		xFrac = (lon - s.lon0) * s.invDLon
		if yFrac < 0 || yFrac > float64(s.nLatM1) ||
			xFrac < 0 || xFrac > float64(s.nLonM1) {
			return math.NaN()
		}
		x0 = int(xFrac)
		y0 = int(yFrac)
		if x0 >= s.nLonM1 {
			x0 = s.nLonM1 - 1
		}
		if y0 >= s.nLatM1 {
			y0 = s.nLatM1 - 1
		}
	} else {
		var ok bool
		yFrac, ok = fracIndex(s.lats, lat, s.latDescend)
		if !ok {
			return math.NaN()
		}
		xFrac, ok = fracIndex(s.lons, lon, s.lonDescend)
		if !ok {
			return math.NaN()
		}
		x0 = int(math.Floor(xFrac))
		y0 = int(math.Floor(yFrac))
		if x0 < 0 || y0 < 0 || x0 >= len(s.lons)-1 || y0 >= len(s.lats)-1 {
			return math.NaN()
		}
	}
	u := xFrac - float64(x0)
	v := yFrac - float64(y0)

	w := s.g.Header.Nx
	row0 := y0 * w
	row1 := row0 + w
	v00 := s.g.DataValues[row0+x0]
	v10 := s.g.DataValues[row0+x0+1]
	v01 := s.g.DataValues[row1+x0]
	v11 := s.g.DataValues[row1+x0+1]
	if v00 == s.missing || v10 == s.missing || v01 == s.missing || v11 == s.missing {
		return math.NaN()
	}
	return (1-u)*(1-v)*v00 + u*(1-v)*v10 + (1-u)*v*v01 + u*v*v11
}

func fracIndex(arr []float64, target float64, descend bool) (float64, bool) {
	n := len(arr)
	if descend {
		if target > arr[0] || target < arr[n-1] {
			return 0, false
		}
		lo, hi := 0, n-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if arr[mid] >= target {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		i := lo
		if i >= n-1 {
			i = n - 2
		}
		denom := arr[i] - arr[i+1]
		if denom == 0 {
			return float64(i), true
		}
		return float64(i) + (arr[i]-target)/denom, true
	}
	if target < arr[0] || target > arr[n-1] {
		return 0, false
	}
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if arr[mid] <= target {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	i := lo
	if i >= n-1 {
		i = n - 2
	}
	denom := arr[i+1] - arr[i]
	if denom == 0 {
		return float64(i), true
	}
	return float64(i) + (target-arr[i])/denom, true
}

func Tile(s *Sampler, z uint8, x, y uint32, pixSize int) []float32 {
	lats := make([]float64, pixSize)
	lons := make([]float64, pixSize)
	TileLats(z, y, pixSize, lats)
	TileLons(z, x, pixSize, lons)

	gw, gs, ge, gn := GridBBox(s.g)
	r0, r1 := pixSize, 0
	for r := range pixSize {
		if lats[r] >= gs && lats[r] <= gn {
			if r < r0 {
				r0 = r
			}
			r1 = r + 1
		}
	}
	c0, c1 := pixSize, 0
	for c := range pixSize {
		if lons[c] >= gw && lons[c] <= ge {
			if c < c0 {
				c0 = c
			}
			c1 = c + 1
		}
	}
	if r0 >= r1 || c0 >= c1 {
		return nil
	}

	// sparse pre-sample on a coarse grid (~256 probes): skips fully-empty tiles
	// without paying for every pixel; tile ends up nil and the encoder drops it
	rArea := (r1 - r0) * (c1 - c0)
	stride := max(isqrt(rArea/256), 1)
	allNaN := true
	for r := r0; r < r1 && allNaN; r += stride {
		lat := lats[r]
		for c := c0; c < c1; c += stride {
			if !math.IsNaN(s.At(lat, lons[c])) {
				allNaN = false
				break
			}
		}
	}
	if allNaN {
		return nil
	}

	out := make([]float32, pixSize*pixSize)
	for r := range pixSize {
		lat := lats[r]
		row := r * pixSize
		for c := range pixSize {
			v := s.At(lat, lons[c])
			if math.IsNaN(v) {
				out[row+c] = float32(math.NaN())
			} else {
				out[row+c] = float32(v)
			}
		}
	}
	return out
}

func isqrt(n int) int {
	if n <= 1 {
		return n
	}
	x := 1
	for x*x <= n {
		x++
	}
	return x - 1
}

func TileLats(z uint8, y uint32, pixSize int, out []float64) {
	n := float64(uint32(1) << z)
	invN := 1 / n
	invPS := 1 / float64(pixSize)
	for r := range pixSize {
		ymerc := (float64(y) + (float64(r)+0.5)*invPS) * invN
		out[r] = math.Atan(math.Sinh(math.Pi*(1-2*ymerc))) * 180 / math.Pi
	}
}

func TileLons(z uint8, x uint32, pixSize int, out []float64) {
	n := float64(uint32(1) << z)
	invN := 1 / n
	invPS := 1 / float64(pixSize)
	for c := range pixSize {
		xmerc := (float64(x) + (float64(c)+0.5)*invPS) * invN
		out[c] = xmerc*360 - 180
	}
}
