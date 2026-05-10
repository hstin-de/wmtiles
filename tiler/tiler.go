package tiler

import (
	"math"
	"sync"
	"unsafe"

	"github.com/hstin-de/wmtiles/parser"
)

// *[]float32 (not []float32) avoids the per-Put boxing alloc bare slices
// in sync.Pool incur.
var tileBufPool = sync.Pool{
	New: func() any { return (*[]float32)(nil) },
}

type tileScratch struct {
	lats, lons []float64
	colX0      []int32
	colU       []float32
}

var tileScratchPool = sync.Pool{
	New: func() any { return &tileScratch{} },
}

func getTileScratch(pixSize int) *tileScratch {
	s := tileScratchPool.Get().(*tileScratch)
	if cap(s.lats) < pixSize {
		s.lats = make([]float64, pixSize)
	} else {
		s.lats = s.lats[:pixSize]
	}
	if cap(s.lons) < pixSize {
		s.lons = make([]float64, pixSize)
	} else {
		s.lons = s.lons[:pixSize]
	}
	if cap(s.colX0) < pixSize {
		s.colX0 = make([]int32, pixSize)
	} else {
		s.colX0 = s.colX0[:pixSize]
	}
	if cap(s.colU) < pixSize {
		s.colU = make([]float32, pixSize)
	} else {
		s.colU = s.colU[:pixSize]
	}
	return s
}

func putTileScratch(s *tileScratch) {
	tileScratchPool.Put(s)
}

func getTileBuf(n int) []float32 {
	if v := tileBufPool.Get(); v != nil {
		if p, ok := v.(*[]float32); ok && p != nil && cap(*p) >= n {
			return (*p)[:n]
		}
	}
	return make([]float32, n)
}

func PutTileBuf(b []float32) {
	if cap(b) == 0 {
		return
	}
	b = b[:cap(b)]
	tileBufPool.Put(&b)
}

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
	missingF32 float32

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

	// Set at construction; enables the dense-grid fast path in the bilinear
	// filler and fused quantize kernels (skips per-pixel missing checks).
	hasMissing bool
}

func NewSampler(g *parser.GRIBFile) *Sampler {
	if len(g.Header.DistinctLatitudes) < 2 || len(g.Header.DistinctLongitudes) < 2 {
		return nil
	}
	s := &Sampler{
		g:          g,
		lats:       g.Header.DistinctLatitudes,
		lons:       g.Header.DistinctLongitudes,
		missing:    g.Header.MissingValue,
		missingF32: float32(g.Header.MissingValue),
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
	s.hasMissing = scanForMissing(g.DataValues, s.missingF32)
	return s
}

// scanForMissing reports whether any value is the GRIB missing sentinel or NaN.
// Unrolled by 8 for ILP; runs once per message to gate the dense-grid path.
func scanForMissing(values []float32, missing float32) bool {
	n := len(values)
	i := 0
	for ; i+8 <= n; i += 8 {
		v0 := values[i]
		v1 := values[i+1]
		v2 := values[i+2]
		v3 := values[i+3]
		v4 := values[i+4]
		v5 := values[i+5]
		v6 := values[i+6]
		v7 := values[i+7]
		if v0 == missing || v1 == missing || v2 == missing || v3 == missing ||
			v4 == missing || v5 == missing || v6 == missing || v7 == missing {
			return true
		}
		if v0 != v0 || v1 != v1 || v2 != v2 || v3 != v3 ||
			v4 != v4 || v5 != v5 || v6 != v6 || v7 != v7 {
			return true
		}
	}
	for ; i < n; i++ {
		v := values[i]
		if v == missing || v != v {
			return true
		}
	}
	return false
}

// Uniform reports whether the grid has constant lat/lon step. Required by
// the fused tile-and-quantize kernels.
func (s *Sampler) Uniform() bool { return s.uniform }

// HasMissing reports whether the source grid contains any missing/NaN values.
func (s *Sampler) HasMissing() bool { return s.hasMissing }

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
	if v00 == s.missingF32 || v10 == s.missingF32 || v01 == s.missingF32 || v11 == s.missingF32 {
		return math.NaN()
	}
	return (1-u)*(1-v)*float64(v00) + u*(1-v)*float64(v10) + (1-u)*v*float64(v01) + u*v*float64(v11)
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
	ts := getTileScratch(pixSize)
	defer putTileScratch(ts)
	lats := ts.lats
	lons := ts.lons
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

	out := getTileBuf(pixSize * pixSize)
	if s.uniform {
		fillTileUniformScratch(s, lats, lons, out, pixSize, ts.colX0, ts.colU)
	} else {
		fillTileGeneric(s, lats, lons, out, pixSize)
	}
	return out
}

func fillTileGeneric(s *Sampler, lats, lons []float64, out []float32, pixSize int) {
	nan := float32(math.NaN())
	for r := range pixSize {
		lat := lats[r]
		row := r * pixSize
		for c := range pixSize {
			v := s.At(lat, lons[c])
			if math.IsNaN(v) {
				out[row+c] = nan
			} else {
				out[row+c] = float32(v)
			}
		}
	}
}

func fillTileUniformScratch(s *Sampler, lats, lons []float64, out []float32, pixSize int, colX0 []int32, colU []float32) {
	nan := float32(math.NaN())
	lon0 := s.lon0
	lat0 := s.lat0
	invDLat := s.invDLat
	invDLon := s.invDLon
	nLatM1 := s.nLatM1
	nLonM1 := s.nLonM1
	nLatM1F := float64(nLatM1)
	nLonM1F := float64(nLonM1)
	w := s.g.Header.Nx
	missing := s.missingF32
	data := s.g.DataValues
	lonGrid0 := s.lonGrid0
	c0, c1 := pixSize, 0
	for c, lon := range lons {
		if lonGrid0 {
			if lon < 0 {
				lon += 360
			}
		} else if lon > 180 {
			lon -= 360
		}
		xFrac := (lon - lon0) * invDLon
		if xFrac < 0 || xFrac > nLonM1F {
			colX0[c] = -1
			continue
		}
		x0 := int(xFrac)
		if x0 >= nLonM1 {
			x0 = nLonM1 - 1
		}
		colX0[c] = int32(x0)
		colU[c] = float32(xFrac - float64(x0))
		if c < c0 {
			c0 = c
		}
		if c+1 > c1 {
			c1 = c + 1
		}
	}

	noMissing := !s.hasMissing

	for r, lat := range lats {
		rowBase := r * pixSize
		rowOut := out[rowBase : rowBase+pixSize]

		yFrac := (lat - lat0) * invDLat
		if yFrac < 0 || yFrac > nLatM1F {
			for c := range pixSize {
				rowOut[c] = nan
			}
			continue
		}
		y0 := int(yFrac)
		if y0 >= nLatM1 {
			y0 = nLatM1 - 1
		}
		vF := float32(yFrac - float64(y0))
		oneV := 1 - vF
		row0 := y0 * w
		row1 := row0 + w

		for c := 0; c < c0; c++ {
			rowOut[c] = nan
		}
		for c := c1; c < pixSize; c++ {
			rowOut[c] = nan
		}

		// Dense-grid fast path: drop per-pixel missing-value checks.
		if noMissing {
			// Hoist bounds checks on the column scratch slices.
			_ = colX0[c1-1]
			_ = colU[c1-1]
			for c := c0; c < c1; c++ {
				x0 := int(colX0[c])
				if x0 < 0 {
					rowOut[c] = nan
					continue
				}
				base0 := row0 + x0
				base1 := row1 + x0
				v00 := data[base0]
				v10 := data[base0+1]
				v01 := data[base1]
				v11 := data[base1+1]
				u := colU[c]
				oneU := 1 - u
				top := oneU*v00 + u*v10
				bot := oneU*v01 + u*v11
				rowOut[c] = oneV*top + vF*bot
			}
			continue
		}

		for c := c0; c < c1; c++ {
			x0 := int(colX0[c])
			if x0 < 0 {
				rowOut[c] = nan
				continue
			}
			v00 := data[row0+x0]
			v10 := data[row0+x0+1]
			v01 := data[row1+x0]
			v11 := data[row1+x0+1]
			if v00 == missing || v10 == missing || v01 == missing || v11 == missing {
				rowOut[c] = nan
				continue
			}
			u := colU[c]
			oneU := 1 - u
			top := oneU*v00 + u*v10
			bot := oneU*v01 + u*v11
			rowOut[c] = oneV*top + vF*bot
		}
	}
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

// Must match quantize.SentinelU16/U8. Duplicated to avoid importing quantize.
const QuantU16Sentinel uint16 = 0xFFFF
const QuantU8Sentinel uint8 = 0xFF

// TileQuantU16 fuses bilinear interpolation with u16 quantization, writing
// directly into out (LE u16 per pixel). Returns false on no-overlap or
// all-missing tiles. Skips the float32 intermediate the two-pass path needs.
func TileQuantU16(s *Sampler, z uint8, x, y uint32, pixSize int, scale, offset float64, out []byte) bool {
	if !s.uniform {
		return false
	}
	if len(out) < pixSize*pixSize*2 {
		return false
	}
	ts := getTileScratch(pixSize)
	defer putTileScratch(ts)
	lats := ts.lats
	lons := ts.lons
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
	c0Box, c1Box := pixSize, 0
	for c := range pixSize {
		if lons[c] >= gw && lons[c] <= ge {
			if c < c0Box {
				c0Box = c
			}
			c1Box = c + 1
		}
	}
	if r0 >= r1 || c0Box >= c1Box {
		return false
	}
	rArea := (r1 - r0) * (c1Box - c0Box)
	stride := max(isqrt(rArea/256), 1)
	allNaN := true
	for r := r0; r < r1 && allNaN; r += stride {
		lat := lats[r]
		for c := c0Box; c < c1Box; c += stride {
			if !math.IsNaN(s.At(lat, lons[c])) {
				allNaN = false
				break
			}
		}
	}
	if allNaN {
		return false
	}

	colX0 := ts.colX0
	colU := ts.colU

	lon0 := s.lon0
	lat0 := s.lat0
	invDLat := s.invDLat
	invDLon := s.invDLon
	nLatM1 := s.nLatM1
	nLonM1 := s.nLonM1
	nLatM1F := float64(nLatM1)
	nLonM1F := float64(nLonM1)
	w := s.g.Header.Nx
	missing := s.missingF32
	data := s.g.DataValues
	lonGrid0 := s.lonGrid0

	c0, c1 := pixSize, 0
	for c, lon := range lons {
		if lonGrid0 {
			if lon < 0 {
				lon += 360
			}
		} else if lon > 180 {
			lon -= 360
		}
		xFrac := (lon - lon0) * invDLon
		if xFrac < 0 || xFrac > nLonM1F {
			colX0[c] = -1
			continue
		}
		x0 := int(xFrac)
		if x0 >= nLonM1 {
			x0 = nLonM1 - 1
		}
		colX0[c] = int32(x0)
		colU[c] = float32(xFrac - float64(x0))
		if c < c0 {
			c0 = c
		}
		if c+1 > c1 {
			c1 = c + 1
		}
	}

	// Alias out as []uint16: one store per pixel.
	dst := (*[1 << 30]uint16)(unsafe.Pointer(unsafe.SliceData(out)))[: pixSize*pixSize : pixSize*pixSize]

	inv32 := float32(1.0 / scale)
	off32 := float32(offset)
	noMissing := !s.hasMissing

	// Narrow scratch slices to drop per-pixel bounds checks; c0..c1 is
	// contiguous-valid so the inner loop omits the x0<0 guard.
	colU2 := colU[:pixSize]
	colX02 := colX0[:pixSize]

	for r, lat := range lats {
		rowBase := r * pixSize
		rowOut := dst[rowBase : rowBase+pixSize : rowBase+pixSize]
		yFrac := (lat - lat0) * invDLat
		if yFrac < 0 || yFrac > nLatM1F {
			for c := 0; c < pixSize; c++ {
				rowOut[c] = QuantU16Sentinel
			}
			continue
		}
		y0 := int(yFrac)
		if y0 >= nLatM1 {
			y0 = nLatM1 - 1
		}
		vF := float32(yFrac - float64(y0))
		oneV := 1 - vF
		row0 := y0 * w
		row1 := row0 + w

		for c := 0; c < c0; c++ {
			rowOut[c] = QuantU16Sentinel
		}
		for c := c1; c < pixSize; c++ {
			rowOut[c] = QuantU16Sentinel
		}

		if noMissing {
			// Row subslices hoist bounds checks out of the inner loop.
			row0Slice := data[row0 : row0+w]
			row1Slice := data[row1 : row1+w]
			for c := c0; c < c1; c++ {
				x0 := int(colX02[c])
				v00 := row0Slice[x0]
				v10 := row0Slice[x0+1]
				v01 := row1Slice[x0]
				v11 := row1Slice[x0+1]
				u := colU2[c]
				oneU := 1 - u
				top := oneU*v00 + u*v10
				bot := oneU*v01 + u*v11
				v := oneV*top + vF*bot
				rq := (v - off32) * inv32
				var q uint16
				switch {
				case rq <= 0:
					q = 0
				case rq >= 65534:
					q = 65534
				default:
					q = uint16(rq + 0.5)
				}
				rowOut[c] = q
			}
			continue
		}

		row0Slice := data[row0 : row0+w]
		row1Slice := data[row1 : row1+w]
		for c := c0; c < c1; c++ {
			x0 := int(colX02[c])
			if x0 < 0 {
				rowOut[c] = QuantU16Sentinel
				continue
			}
			v00 := row0Slice[x0]
			v10 := row0Slice[x0+1]
			v01 := row1Slice[x0]
			v11 := row1Slice[x0+1]
			if v00 == missing || v10 == missing || v01 == missing || v11 == missing ||
				v00 != v00 || v10 != v10 || v01 != v01 || v11 != v11 {
				rowOut[c] = QuantU16Sentinel
				continue
			}
			u := colU2[c]
			oneU := 1 - u
			top := oneU*v00 + u*v10
			bot := oneU*v01 + u*v11
			v := oneV*top + vF*bot
			rq := (v - off32) * inv32
			var q uint16
			switch {
			case rq <= 0:
				q = 0
			case rq >= 65534:
				q = 65534
			default:
				q = uint16(rq + 0.5)
			}
			rowOut[c] = q
		}
	}
	return true
}

// TileQuantU8 is the u8 counterpart of TileQuantU16. Pixels are written as one
// byte each. Returns false on no-overlap / all-missing tiles.
func TileQuantU8(s *Sampler, z uint8, x, y uint32, pixSize int, scale, offset float64, out []byte) bool {
	if !s.uniform {
		return false
	}
	if len(out) < pixSize*pixSize {
		return false
	}
	ts := getTileScratch(pixSize)
	defer putTileScratch(ts)
	lats := ts.lats
	lons := ts.lons
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
	c0Box, c1Box := pixSize, 0
	for c := range pixSize {
		if lons[c] >= gw && lons[c] <= ge {
			if c < c0Box {
				c0Box = c
			}
			c1Box = c + 1
		}
	}
	if r0 >= r1 || c0Box >= c1Box {
		return false
	}
	rArea := (r1 - r0) * (c1Box - c0Box)
	stride := max(isqrt(rArea/256), 1)
	allNaN := true
	for r := r0; r < r1 && allNaN; r += stride {
		lat := lats[r]
		for c := c0Box; c < c1Box; c += stride {
			if !math.IsNaN(s.At(lat, lons[c])) {
				allNaN = false
				break
			}
		}
	}
	if allNaN {
		return false
	}

	colX0 := ts.colX0
	colU := ts.colU

	lon0 := s.lon0
	lat0 := s.lat0
	invDLat := s.invDLat
	invDLon := s.invDLon
	nLatM1 := s.nLatM1
	nLonM1 := s.nLonM1
	nLatM1F := float64(nLatM1)
	nLonM1F := float64(nLonM1)
	w := s.g.Header.Nx
	missing := s.missingF32
	data := s.g.DataValues
	lonGrid0 := s.lonGrid0

	c0, c1 := pixSize, 0
	for c, lon := range lons {
		if lonGrid0 {
			if lon < 0 {
				lon += 360
			}
		} else if lon > 180 {
			lon -= 360
		}
		xFrac := (lon - lon0) * invDLon
		if xFrac < 0 || xFrac > nLonM1F {
			colX0[c] = -1
			continue
		}
		x0 := int(xFrac)
		if x0 >= nLonM1 {
			x0 = nLonM1 - 1
		}
		colX0[c] = int32(x0)
		colU[c] = float32(xFrac - float64(x0))
		if c < c0 {
			c0 = c
		}
		if c+1 > c1 {
			c1 = c + 1
		}
	}

	inv32 := float32(1.0 / scale)
	off32 := float32(offset)
	noMissing := !s.hasMissing

	for r, lat := range lats {
		rowBase := r * pixSize
		yFrac := (lat - lat0) * invDLat
		if yFrac < 0 || yFrac > nLatM1F {
			for c := 0; c < pixSize; c++ {
				out[rowBase+c] = QuantU8Sentinel
			}
			continue
		}
		y0 := int(yFrac)
		if y0 >= nLatM1 {
			y0 = nLatM1 - 1
		}
		vF := float32(yFrac - float64(y0))
		oneV := 1 - vF
		row0 := y0 * w
		row1 := row0 + w

		for c := 0; c < c0; c++ {
			out[rowBase+c] = QuantU8Sentinel
		}
		for c := c1; c < pixSize; c++ {
			out[rowBase+c] = QuantU8Sentinel
		}

		if noMissing {
			for c := c0; c < c1; c++ {
				x0 := int(colX0[c])
				if x0 < 0 {
					out[rowBase+c] = QuantU8Sentinel
					continue
				}
				base0 := row0 + x0
				base1 := row1 + x0
				v00 := data[base0]
				v10 := data[base0+1]
				v01 := data[base1]
				v11 := data[base1+1]
				u := colU[c]
				oneU := 1 - u
				top := oneU*v00 + u*v10
				bot := oneU*v01 + u*v11
				v := oneV*top + vF*bot
				rq := (v - off32) * inv32
				var q uint8
				switch {
				case rq <= 0:
					q = 0
				case rq >= 254:
					q = 254
				default:
					q = uint8(rq + 0.5)
				}
				out[rowBase+c] = q
			}
			continue
		}

		for c := c0; c < c1; c++ {
			x0 := int(colX0[c])
			if x0 < 0 {
				out[rowBase+c] = QuantU8Sentinel
				continue
			}
			v00 := data[row0+x0]
			v10 := data[row0+x0+1]
			v01 := data[row1+x0]
			v11 := data[row1+x0+1]
			if v00 == missing || v10 == missing || v01 == missing || v11 == missing ||
				v00 != v00 || v10 != v10 || v01 != v01 || v11 != v11 {
				out[rowBase+c] = QuantU8Sentinel
				continue
			}
			u := colU[c]
			oneU := 1 - u
			top := oneU*v00 + u*v10
			bot := oneU*v01 + u*v11
			v := oneV*top + vF*bot
			rq := (v - off32) * inv32
			var q uint8
			switch {
			case rq <= 0:
				q = 0
			case rq >= 254:
				q = 254
			default:
				q = uint8(rq + 0.5)
			}
			out[rowBase+c] = q
		}
	}
	return true
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
