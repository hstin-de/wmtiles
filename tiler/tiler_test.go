package tiler

import (
	"math"
	"testing"
	"time"

	"github.com/hstin-de/wmtiles/parser"
)

func makeGrid() *parser.GRIBFile {
	const nx, ny = 41, 21
	lats := make([]float64, ny)
	lons := make([]float64, nx)
	for i := range ny {
		lats[i] = 35 + float64(i)
	}
	for j := range nx {
		lons[j] = -10 + float64(j)
	}
	data := make([]float64, nx*ny)
	for r := range ny {
		for c := range nx {
			data[r*nx+c] = lats[r] + 0.1*lons[c]
		}
	}
	return &parser.GRIBFile{
		Header: parser.GribHeader{
			Nx: nx, Ny: ny,
			La1: 35, La2: 55, Lo1: -10, Lo2: 30,
			DX: 1, DY: 1,
			MissingValue:       9999,
			ReferenceTime:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			DistinctLatitudes:  lats,
			DistinctLongitudes: lons,
		},
		DataValues: data,
	}
}

func TestPixelToLatLonCenter(t *testing.T) {
	lat, lon := PixelToLatLon(0, 0, 0, 256, 128, 128)
	if math.Abs(lat) > 1.5 || math.Abs(lon) > 1.5 {
		t.Errorf("centre of (0,0,0) approx (0,0) +- 1.5 deg; got (%g, %g)", lat, lon)
	}
}

func TestPixelToLatLonCorners(t *testing.T) {
	lat, lon := PixelToLatLon(0, 0, 0, 256, 0, 0)
	if lat < 80 || lon > -179 {
		t.Errorf("top-left pixel of world tile: got (%g, %g)", lat, lon)
	}
}

func TestSamplerExactGridPoint(t *testing.T) {
	g := makeGrid()
	s := NewSampler(g)
	if s == nil {
		t.Fatal("NewSampler returned nil")
	}
	v := s.At(45, 5)
	want := 45.0 + 0.5
	if math.Abs(v-want) > 1e-9 {
		t.Errorf("At(45, 5) = %g, want %g", v, want)
	}
}

func TestSamplerInterpolation(t *testing.T) {
	g := makeGrid()
	s := NewSampler(g)
	v := s.At(45.5, 5)
	want := 45.5 + 0.5
	if math.Abs(v-want) > 1e-9 {
		t.Errorf("At(45.5, 5) = %g, want %g", v, want)
	}
}

func TestSamplerOutOfRangeNaN(t *testing.T) {
	g := makeGrid()
	s := NewSampler(g)
	if v := s.At(0, 0); !math.IsNaN(v) {
		t.Errorf("expected NaN outside grid, got %g", v)
	}
}

func TestNorthDownGridSampler(t *testing.T) {
	g := makeGrid()
	rev := make([]float64, len(g.Header.DistinctLatitudes))
	for i, v := range g.Header.DistinctLatitudes {
		rev[len(rev)-1-i] = v
	}
	g.Header.DistinctLatitudes = rev
	rowBytes := g.Header.Nx
	revData := make([]float64, len(g.DataValues))
	for r := range g.Header.Ny {
		copy(revData[r*rowBytes:(r+1)*rowBytes],
			g.DataValues[(g.Header.Ny-1-r)*rowBytes:(g.Header.Ny-r)*rowBytes])
	}
	g.DataValues = revData

	s := NewSampler(g)
	v := s.At(45, 5)
	want := 45.0 + 0.5
	if math.Abs(v-want) > 1e-9 {
		t.Errorf("north-down: At(45, 5) = %g, want %g", v, want)
	}
}

func TestTilesIntersectingGrid(t *testing.T) {
	g := makeGrid()
	tiles := TilesIntersectingGrid(g, 3)
	if len(tiles) == 0 {
		t.Errorf("expected tiles intersecting Europe grid at z=3")
	}
	if len(tiles) > 10 {
		t.Errorf("Europe-only grid covers too many tiles at z=3: %d", len(tiles))
	}
}

func TestTileSamplesProduceSomeValid(t *testing.T) {
	g := makeGrid()
	s := NewSampler(g)
	pix := Tile(s, 3, 4, 3, 64)
	hasValid := false
	for _, v := range pix {
		if !math.IsNaN(float64(v)) {
			hasValid = true
			break
		}
	}
	if !hasValid {
		t.Errorf("tile (z=3, x=4, y=3) should overlap Europe grid")
	}
}

func TestGridBBox(t *testing.T) {
	g := makeGrid()
	w, s, e, n := GridBBox(g)
	if w != -10 || s != 35 || e != 30 || n != 55 {
		t.Errorf("GridBBox = (%g, %g, %g, %g); want (-10, 35, 30, 55)", w, s, e, n)
	}
}
