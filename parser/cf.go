package parser

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func isCF(f *h5File) bool {
	conv, ok := f.attrStr("", "Conventions")
	if !ok {
		return false
	}
	return strings.HasPrefix(conv, "CF-1.")
}

// CF standard_name → project shortName so precision tables and viewer styling
// reuse the GRIB conventions. Unmapped variables fall back to the dataset name.
var cfStandardNameToShort = map[string]string{
	"air_temperature":                           "t",
	"air_pressure":                              "sp",
	"air_pressure_at_mean_sea_level":            "msl",
	"eastward_wind":                             "u",
	"northward_wind":                            "v",
	"upward_air_velocity":                       "w",
	"specific_humidity":                         "q",
	"relative_humidity":                         "r",
	"dew_point_temperature":                     "td",
	"geopotential_height":                       "gh",
	"surface_geopotential":                      "z",
	"precipitation_amount":                      "tp",
	"precipitation_flux":                        "rprate",
	"cloud_area_fraction":                       "tcc",
	"cloud_area_fraction_in_atmosphere_layer":   "tcc",
	"surface_albedo":                            "al",
	"toa_outgoing_shortwave_flux":               "uswrf",
	"surface_downwelling_shortwave_flux_in_air": "dswrf",
	"radar_equivalent_reflectivity":             "dbzh",
}

// parses a CF "<unit> since <iso8601>" units string. recognises s/min/h/d.
func cfTimeEpoch(units string) (time.Time, float64, bool) {
	idx := strings.Index(units, " since ")
	if idx < 0 {
		return time.Time{}, 0, false
	}
	unit := strings.TrimSpace(units[:idx])
	rest := strings.TrimSpace(units[idx+len(" since "):])
	var perSec float64
	switch unit {
	case "seconds", "second", "s":
		perSec = 1
	case "minutes", "minute":
		perSec = 60
	case "hours", "hour", "h":
		perSec = 3600
	case "days", "day", "d":
		perSec = 86400
	default:
		return time.Time{}, 0, false
	}
	formats := []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, fmtStr := range formats {
		if t, err := time.Parse(fmtStr, rest); err == nil {
			return t.UTC(), perSec, true
		}
	}
	return time.Time{}, 0, false
}

func readCoord1D(f *h5File, name string) ([]float64, error) {
	dims, err := f.datasetShape(name)
	if err != nil {
		return nil, err
	}
	if len(dims) != 1 {
		return nil, fmt.Errorf("CF: coord %s rank %d, want 1", name, len(dims))
	}
	out := make([]float64, dims[0])
	if err := f.readF64(name, out); err != nil {
		return nil, err
	}
	return out, nil
}

func findCoord(f *h5File, candidates []string) string {
	for _, c := range candidates {
		if !f.exists(c) {
			continue
		}
		dims, err := f.datasetShape(c)
		if err == nil && len(dims) == 1 {
			return c
		}
	}
	return ""
}

// parseCF supports 1D lat/lon coordinate vars and (lat, lon) or (time, lat, lon)
// data vars. Rotated/curvilinear/unstructured grids and vertical levels are out
// of scope. withValues=false skips the f32 data read for the encoder's catalog
// scan.
func parseCF(f *h5File, sourceName string, withValues bool) ([]GRIBFile, error) {
	latName := findCoord(f, []string{"lat", "latitude", "Latitude", "LAT"})
	lonName := findCoord(f, []string{"lon", "longitude", "Longitude", "LON"})
	if latName == "" || lonName == "" {
		return nil, fmt.Errorf("CF: lat/lon coordinate variables not found")
	}
	lats, err := readCoord1D(f, latName)
	if err != nil {
		return nil, fmt.Errorf("CF: read %s: %w", latName, err)
	}
	lons, err := readCoord1D(f, lonName)
	if err != nil {
		return nil, fmt.Errorf("CF: read %s: %w", lonName, err)
	}
	if len(lats) < 2 || len(lons) < 2 {
		return nil, fmt.Errorf("CF: lat/lon too short (lats=%d lons=%d)", len(lats), len(lons))
	}

	timeName := findCoord(f, []string{"time", "Time", "valid_time", "forecast_time"})
	var times []time.Time
	if timeName != "" {
		tvals, err := readCoord1D(f, timeName)
		if err == nil {
			units, _ := f.attrStr(timeName, "units")
			epoch, perSec, ok := cfTimeEpoch(units)
			if ok {
				times = make([]time.Time, len(tvals))
				for i, v := range tvals {
					times[i] = epoch.Add(time.Duration(v*perSec*1e9) * time.Nanosecond)
				}
			}
		}
	}

	rootLinks, err := f.listLinks("/")
	if err != nil {
		return nil, fmt.Errorf("CF: list root: %w", err)
	}
	out := make([]GRIBFile, 0)
	dLat := lats[1] - lats[0]
	dLon := lons[1] - lons[0]
	for _, name := range rootLinks {
		if name == latName || name == lonName || name == timeName {
			continue
		}
		dims, err := f.datasetShape(name)
		if err != nil {
			continue
		}
		var nLat, nLon, nT int
		var withTime bool
		switch len(dims) {
		case 2:
			if dims[0] != len(lats) || dims[1] != len(lons) {
				continue
			}
			nLat, nLon = dims[0], dims[1]
			nT = 1
		case 3:
			if dims[1] != len(lats) || dims[2] != len(lons) {
				continue
			}
			nT, nLat, nLon = dims[0], dims[1], dims[2]
			withTime = true
			if len(times) != nT {
				// no usable time axis — fall back to hourly step indices anchored at now
				times = make([]time.Time, nT)
				now := time.Now().UTC().Truncate(time.Hour)
				for i := range times {
					times[i] = now.Add(time.Duration(i) * time.Hour)
				}
			}
		default:
			continue
		}

		units, _ := f.attrStr(name, "units")
		stdName, _ := f.attrStr(name, "standard_name")
		longName, _ := f.attrStr(name, "long_name")
		scale, hasScale := f.attrF64(name, "scale_factor")
		offset, hasOffset := f.attrF64(name, "add_offset")
		fill, hasFill := f.attrF64(name, "_FillValue")
		missing, hasMissing := f.attrF64(name, "missing_value")

		shortName := cfStandardNameToShort[stdName]
		if shortName == "" {
			shortName = stringsToLower(longName)
		}
		if shortName == "" {
			shortName = stringsToLower(name)
		}

		const outMissing = float32(9999)
		var raw []float32
		if withValues {
			nElems := nT * nLat * nLon
			raw = make([]float32, nElems)
			if err := f.readF32(name, raw); err != nil {
				continue
			}
			if hasFill {
				fillF := float32(fill)
				for i := range raw {
					if raw[i] == fillF {
						raw[i] = outMissing
					}
				}
			}
			if hasMissing {
				missF := float32(missing)
				for i := range raw {
					if raw[i] == missF {
						raw[i] = outMissing
					}
				}
			}
			if hasScale || hasOffset {
				s := float32(1)
				o := float32(0)
				if hasScale {
					s = float32(scale)
				}
				if hasOffset {
					o = float32(offset)
				}
				for i, v := range raw {
					if v == outMissing {
						continue
					}
					raw[i] = v*s + o
				}
			}
		}

		// synthetic D/C/P keyed off shortName so varKeyOf splits CF variables
		// without needing a real WMO mapping.
		shortHash := 0
		for _, c := range shortName {
			shortHash = shortHash*31 + int(c)
		}
		discipline := 0
		category := 253
		paramNum := shortHash & 0xFF

		for tIdx := 0; tIdx < nT; tIdx++ {
			refTime := time.Time{}
			if len(times) > tIdx {
				refTime = times[tIdx]
			}
			var values []float32
			if withValues {
				values = make([]float32, nLat*nLon)
				start := tIdx * nLat * nLon
				copy(values, raw[start:start+nLat*nLon])
			}

			distinctLats := make([]float64, nLat)
			copy(distinctLats, lats)
			distinctLons := make([]float64, nLon)
			copy(distinctLons, lons)

			h := GribHeader{
				ShortName:          shortName,
				Units:              units,
				Nx:                 nLon,
				Ny:                 nLat,
				La1:                lats[0],
				La2:                lats[nLat-1],
				Lo1:                lons[0],
				Lo2:                lons[nLon-1],
				DX:                 math.Abs(dLon),
				DY:                 dLat,
				Discipline:         discipline,
				ParameterCategory:  category,
				ParameterNumber:    paramNum,
				ReferenceTime:      refTime,
				ForecastTime:       0,
				EndStep:            0,
				MissingValue:       float64(outMissing),
				TypeOfLevel:        "surface",
				Level:              0,
				BottomLevel:        0,
				DistinctLatitudes:  distinctLats,
				DistinctLongitudes: distinctLons,
			}
			out = append(out, GRIBFile{Header: h, DataValues: values})
		}
		_ = withTime
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("CF: no (lat, lon) data variables found")
	}
	_ = sourceName
	return out, nil
}
