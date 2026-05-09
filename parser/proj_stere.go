package parser

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ellipsoidal polar-stereographic projection parameters (Snyder, "Map
// Projections — A Working Manual", §21). angles in radians, lengths in metres.
type StereParams struct {
	LatTs float64
	Lat0  float64
	Lon0  float64
	X0    float64 // false easting
	Y0    float64 // false northing
	A     float64 // semi-major axis
	B     float64 // semi-minor axis
}

// parses the +proj=stere subset of PROJ.4 strings used by ODIM_H5 radar
// composites. unknown tokens are ignored so we tolerate harmless extras.
func ParseStereProj(s string) (StereParams, error) {
	out := StereParams{
		Lat0: math.Pi / 2, // default north polar
		A:    6378137.0,   // WGS84
		B:    6356752.3142451802,
	}
	tokens := strings.Fields(s)
	gotProj := false
	for _, tok := range tokens {
		tok = strings.TrimPrefix(tok, "+")
		if tok == "no_defs" || tok == "" {
			continue
		}
		eq := strings.IndexByte(tok, '=')
		var k, v string
		if eq < 0 {
			k = tok
		} else {
			k = tok[:eq]
			v = tok[eq+1:]
		}
		switch k {
		case "proj":
			if v != "stere" {
				return StereParams{}, fmt.Errorf("proj: expected stere, got %q", v)
			}
			gotProj = true
		case "lat_ts":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("lat_ts: %w", err)
			}
			out.LatTs = f * math.Pi / 180
		case "lat_0":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("lat_0: %w", err)
			}
			out.Lat0 = f * math.Pi / 180
		case "lon_0":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("lon_0: %w", err)
			}
			out.Lon0 = f * math.Pi / 180
		case "x_0":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("x_0: %w", err)
			}
			out.X0 = f
		case "y_0":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("y_0: %w", err)
			}
			out.Y0 = f
		case "a":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("a: %w", err)
			}
			out.A = f
		case "b":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return StereParams{}, fmt.Errorf("b: %w", err)
			}
			out.B = f
		case "ellps":
			switch v {
			case "WGS84":
				out.A = 6378137.0
				out.B = 6356752.3142451802
			case "GRS80":
				out.A = 6378137.0
				out.B = 6356752.314140356
			case "sphere":
				out.A = 6371008.7714
				out.B = out.A
			}
		case "units", "type", "datum", "axis", "k_0", "k", "to_meter", "geoidgrids", "vunits":
			// defaults are correct for ODIM products
		}
	}
	if !gotProj {
		return StereParams{}, fmt.Errorf("proj: +proj=stere missing in %q", s)
	}
	if out.A <= 0 || out.B <= 0 {
		return StereParams{}, fmt.Errorf("proj: invalid ellipsoid a=%g b=%g", out.A, out.B)
	}
	return out, nil
}

// e returns the first eccentricity of the ellipsoid.
func (p *StereParams) ecc() float64 {
	r := p.B / p.A
	return math.Sqrt(1 - r*r)
}

// Forward returns projected (x, y) in metres, including the X0/Y0 offsets.
func (p *StereParams) Forward(latRad, lonRad float64) (float64, float64) {
	e := p.ecc()
	north := p.Lat0 > 0
	phi := latRad
	if !north {
		phi = -latRad
	}
	dlam := lonRad - p.Lon0

	t := tFromPhi(phi, e)
	var rho float64
	if math.Abs(math.Abs(p.LatTs)-math.Pi/2) < 1e-12 {
		rho = 2.0 * p.A * t / math.Sqrt(math.Pow(1+e, 1+e)*math.Pow(1-e, 1-e))
	} else {
		latTs := p.LatTs
		if !north {
			latTs = -latTs
		}
		mc := math.Cos(latTs) / math.Sqrt(1-e*e*math.Sin(latTs)*math.Sin(latTs))
		tc := tFromPhi(latTs, e)
		rho = p.A * mc * t / tc
	}
	var x, y float64
	if north {
		x = rho * math.Sin(dlam)
		y = -rho * math.Cos(dlam)
	} else {
		x = rho * math.Sin(dlam)
		y = rho * math.Cos(dlam)
	}
	return x + p.X0, y + p.Y0
}

// Inverse maps projected (x, y) in metres back to (lat, lon) in radians.
func (p *StereParams) Inverse(x, y float64) (float64, float64) {
	e := p.ecc()
	xr := x - p.X0
	yr := y - p.Y0
	north := p.Lat0 > 0

	rho := math.Hypot(xr, yr)
	if rho < 1e-12 {
		// pole singularity
		return p.Lat0, p.Lon0
	}
	var t float64
	if math.Abs(math.Abs(p.LatTs)-math.Pi/2) < 1e-12 {
		t = rho * math.Sqrt(math.Pow(1+e, 1+e)*math.Pow(1-e, 1-e)) / (2 * p.A)
	} else {
		latTs := p.LatTs
		if !north {
			latTs = -latTs
		}
		mc := math.Cos(latTs) / math.Sqrt(1-e*e*math.Sin(latTs)*math.Sin(latTs))
		tc := tFromPhi(latTs, e)
		t = rho * tc / (p.A * mc)
	}
	// Snyder eq. 7-9: iterate latitude on the conformal sphere; converges in
	// a handful of steps even at high eccentricity.
	phi := math.Pi/2 - 2*math.Atan(t)
	for i := 0; i < 16; i++ {
		esinphi := e * math.Sin(phi)
		newPhi := math.Pi/2 - 2*math.Atan(t*math.Pow((1-esinphi)/(1+esinphi), e/2))
		if math.Abs(newPhi-phi) < 1e-13 {
			phi = newPhi
			break
		}
		phi = newPhi
	}
	if !north {
		phi = -phi
	}
	var lon float64
	if north {
		lon = p.Lon0 + math.Atan2(xr, -yr)
	} else {
		lon = p.Lon0 + math.Atan2(xr, yr)
	}
	for lon > math.Pi {
		lon -= 2 * math.Pi
	}
	for lon < -math.Pi {
		lon += 2 * math.Pi
	}
	return phi, lon
}

// Snyder eq. 15-9: t = tan(π/4 − φ/2) · ((1+e·sinφ)/(1−e·sinφ))^(e/2).
func tFromPhi(phi, e float64) float64 {
	esinphi := e * math.Sin(phi)
	return math.Tan(math.Pi/4-phi/2) *
		math.Pow((1+esinphi)/(1-esinphi), e/2)
}

// projected raster geometry; X0/Y0 are the projected coordinates of the upper-
// left pixel centre (ODIM convention).
type stereGrid struct {
	Cols, Rows     int
	XScale, YScale float64
	X0, Y0         float64
	Params         StereParams
}

func (g *stereGrid) pixelAtProjXY(x, y float64) (float64, float64) {
	col := (x-g.X0)/g.XScale - 0.5
	row := (g.Y0-y)/g.YScale - 0.5
	return col, row
}
