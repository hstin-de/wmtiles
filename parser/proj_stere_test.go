package parser

import (
	"math"
	"testing"
)

// dwdProj is the projection used by DWD ODIM_H5 radar composites
// (DE1200_WGS84 grid).
const dwdProj = "+proj=stere +lat_ts=60 +lat_0=90 +lon_0=10 +x_0=543196.83521776402 +y_0=3622588.8619310022 +units=m +a=6378137 +b=6356752.3142451802 +no_defs"

func TestParseStereProj(t *testing.T) {
	p, err := ParseStereProj(dwdProj)
	if err != nil {
		t.Fatalf("ParseStereProj: %v", err)
	}
	wantLatTs := 60.0 * math.Pi / 180
	if math.Abs(p.LatTs-wantLatTs) > 1e-9 {
		t.Errorf("LatTs = %v, want %v", p.LatTs, wantLatTs)
	}
	wantLat0 := 90.0 * math.Pi / 180
	if math.Abs(p.Lat0-wantLat0) > 1e-9 {
		t.Errorf("Lat0 = %v, want %v", p.Lat0, wantLat0)
	}
	wantLon0 := 10.0 * math.Pi / 180
	if math.Abs(p.Lon0-wantLon0) > 1e-9 {
		t.Errorf("Lon0 = %v, want %v", p.Lon0, wantLon0)
	}
	if math.Abs(p.X0-543196.83521776402) > 1e-3 {
		t.Errorf("X0 = %v, want 543196.835...", p.X0)
	}
	if math.Abs(p.Y0-3622588.8619310022) > 1e-3 {
		t.Errorf("Y0 = %v, want 3622588.862...", p.Y0)
	}
	if math.Abs(p.A-6378137) > 1e-6 || math.Abs(p.B-6356752.3142451802) > 1e-6 {
		t.Errorf("ellipsoid (a=%v b=%v) does not match WGS84", p.A, p.B)
	}
}

func TestStereForwardInverseRoundtrip(t *testing.T) {
	p, err := ParseStereProj(dwdProj)
	if err != nil {
		t.Fatalf("ParseStereProj: %v", err)
	}
	cases := []struct {
		lat, lon float64 // degrees
	}{
		{50.0, 10.0},       // central Germany, near central meridian
		{55.8621, 1.4633},  // ODIM UL corner (DE1200 NW)
		{45.6964, 3.56699}, // ODIM LL corner (DE1200 SW)
		{55.8454, 18.7316}, // ODIM UR corner (DE1200 NE)
		{45.6846, 16.5809}, // ODIM LR corner (DE1200 SE)
		{47.0, -5.0},
		{60.0, 30.0},
	}
	for _, c := range cases {
		latRad := c.lat * math.Pi / 180
		lonRad := c.lon * math.Pi / 180
		x, y := p.Forward(latRad, lonRad)
		latBack, lonBack := p.Inverse(x, y)
		dLat := math.Abs(latBack-latRad) * 180 / math.Pi
		dLon := math.Abs(lonBack-lonRad) * 180 / math.Pi
		if dLat > 1e-6 || dLon > 1e-6 {
			t.Errorf("roundtrip (lat=%v lon=%v) -> (x=%v y=%v) -> (lat=%v lon=%v); err=(%v°, %v°)",
				c.lat, c.lon, x, y, latBack*180/math.Pi, lonBack*180/math.Pi, dLat, dLon)
		}
	}
}

// projecting the four ODIM corner lat/lons must yield a rectangle close to
// (xsize × xscale) × (ysize × yscale) — the calibration the resampler relies on.
func TestStereCornerCalibration(t *testing.T) {
	p, err := ParseStereProj(dwdProj)
	if err != nil {
		t.Fatalf("ParseStereProj: %v", err)
	}
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	ulX, ulY := p.Forward(rad(55.8621), rad(1.4633))
	urX, urY := p.Forward(rad(55.8454), rad(18.7316))
	llX, llY := p.Forward(rad(45.6964), rad(3.56699))
	lrX, lrY := p.Forward(rad(45.6846), rad(16.5809))
	const xsize = 1100.0
	const ysize = 1200.0
	const xscale = 1000.0
	const yscale = 1000.0
	if math.Abs((urX-ulX)-xsize*xscale) > xscale*2 {
		t.Errorf("top-row width = %v, want ≈%v (within 2 px)", urX-ulX, xsize*xscale)
	}
	if math.Abs((lrX-llX)-xsize*xscale) > xscale*2 {
		t.Errorf("bottom-row width = %v, want ≈%v", lrX-llX, xsize*xscale)
	}
	if math.Abs((ulY-llY)-ysize*yscale) > yscale*2 {
		t.Errorf("left-col height = %v, want ≈%v", ulY-llY, ysize*yscale)
	}
	if math.Abs((urY-lrY)-ysize*yscale) > yscale*2 {
		t.Errorf("right-col height = %v, want ≈%v", urY-lrY, ysize*yscale)
	}
}
