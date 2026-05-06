package parser

// thin cgo wrapper over ECMWF's eccodes C library: handles GRIB2 message parsing,
// since pure-Go GRIB readers don't cover the WMO templates we need

/*
#cgo CFLAGS: -I/usr/include/x86_64-linux-gnu/
#cgo LDFLAGS: -leccodes
#include "eccodes.h"
#include <stdio.h>
#include <stdlib.h>
*/
import "C"
import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"
)

type GribHeader struct {
	Type               int32     `json:"type"`
	ShortName          string    `json:"shortName"`
	Units              string    `json:"units"`
	Nx                 int       `json:"nx"`
	Ny                 int       `json:"ny"`
	La1                float64   `json:"la1"`
	La2                float64   `json:"la2"`
	Lo1                float64   `json:"lo1"`
	Lo2                float64   `json:"lo2"`
	DX                 float64   `json:"dx"`
	DY                 float64   `json:"dy"`
	ScanMode           int       `json:"scanMode"`
	Discipline         int       `json:"discipline"`
	ParameterCategory  int       `json:"parameterCategory"`
	ParameterNumber    int       `json:"parameterNumber"`
	ReferenceTime      time.Time `json:"referenceTime"`
	ForecastTime       int       `json:"forecastTime"`
	EndStep            int       `json:"endStep"`
	MissingValue       float64   `json:"missingValue"`
	TypeOfLevel        string    `json:"typeOfLevel"`
	Level              int       `json:"level"`
	BottomLevel        int       `json:"bottomLevel"`
	DistinctLatitudes  []float64 `json:"distinctLatitudes"`
	DistinctLongitudes []float64 `json:"distinctLongitudes"`
}

type GRIBFile struct {
	Header     GribHeader
	DataValues []float64
}

func (g *GRIBFile) GetLatLng(x, y int) (float64, float64) {
	if x < 0 || x >= g.Header.Nx || y < 0 || y >= g.Header.Ny {
		return 9999, 9999
	}

	lo1 := g.Header.Lo1
	if lo1 > 180 {
		lo1 -= 360
	}

	lat := g.Header.La1 + float64(y)*g.Header.DY
	lng := lo1 + float64(x)*g.Header.DX

	return lat, lng
}

func (g *GRIBFile) GetData(lat, lng float64) float64 {
	lo1 := g.Header.Lo1
	if lo1 > 180 {
		lo1 -= 360
	}

	x := int((lng - lo1) / g.Header.DX)
	y := int((lat - g.Header.La1) / g.Header.DY)

	if x < 0 || x >= g.Header.Nx || y < 0 || y >= g.Header.Ny {
		return 9999
	}

	return g.DataValues[y*g.Header.Nx+x]
}

// Catmull-Rom bicubic interpolation over the 4×4 stencil around (lat, lng);
// keeps C1 continuity, returns missingValue if any of the 16 neighbours is missing
func (g *GRIBFile) GetInterpolatedData(lat, lng float64) float64 {
	la1 := g.Header.La1
	lo1 := g.Header.Lo1
	la2 := g.Header.La2
	lo2 := g.Header.Lo2
	dx := g.Header.DX
	dy := g.Header.DY
	width := g.Header.Nx
	height := g.Header.Ny

	data := g.DataValues

	if lo1 > 180 {
		lo1 -= 360
	}
	if lng > 180 {
		lng -= 360
	} else if lng < -180 {
		lng += 360
	}
	if dy < 0 {
		dy = -dy
	}

	if lat < la1 || lat > la2 || lng < lo1 || lng > lo2 {
		return g.Header.MissingValue
	}

	x := (lng - lo1) / dx
	y := (lat - la1) / dy

	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))

	if x0 < 1 || x0 > width-3 || y0 < 1 || y0 > height-3 {
		return g.Header.MissingValue
	}

	u := x - float64(x0)
	v := y - float64(y0)

	x0m1 := x0 - 1
	x0p1 := x0 + 1
	x0p2 := x0 + 2
	y0m1 := y0 - 1
	y0p1 := y0 + 1
	y0p2 := y0 + 2

	if x0m1 < 0 || x0p2 >= width || y0m1 < 0 || y0p2 >= height {
		return 0
	}

	u2 := u * u
	u3 := u2 * u
	v2 := v * v
	v3 := v2 * v

	wx0 := (-0.5 * u3) + (u2) - (0.5 * u)
	wx1 := (1.5 * u3) - (2.5 * u2) + 1
	wx2 := (-1.5 * u3) + (2.0 * u2) + (0.5 * u)
	wx3 := (0.5 * u3) - (0.5 * u2)
	wy0 := (-0.5 * v3) + (v2) - (0.5 * v)
	wy1 := (1.5 * v3) - (2.5 * v2) + 1
	wy2 := (-1.5 * v3) + (2.0 * v2) + (0.5 * v)
	wy3 := (0.5 * v3) - (0.5 * v2)

	v00 := data[y0m1*width+x0m1]
	v01 := data[y0m1*width+x0]
	v02 := data[y0m1*width+x0p1]
	v03 := data[y0m1*width+x0p2]

	if v00 == g.Header.MissingValue || v01 == g.Header.MissingValue || v02 == g.Header.MissingValue || v03 == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v10 := data[y0*width+x0m1]
	v11 := data[y0*width+x0]
	v12 := data[y0*width+x0p1]
	v13 := data[y0*width+x0p2]

	if v10 == g.Header.MissingValue || v11 == g.Header.MissingValue || v12 == g.Header.MissingValue || v13 == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v20 := data[y0p1*width+x0m1]
	v21 := data[y0p1*width+x0]
	v22 := data[y0p1*width+x0p1]
	v23 := data[y0p1*width+x0p2]

	if v20 == g.Header.MissingValue || v21 == g.Header.MissingValue || v22 == g.Header.MissingValue || v23 == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v30 := data[y0p2*width+x0m1]
	v31 := data[y0p2*width+x0]
	v32 := data[y0p2*width+x0p1]
	v33 := data[y0p2*width+x0p2]

	if v30 == g.Header.MissingValue || v31 == g.Header.MissingValue || v32 == g.Header.MissingValue || v33 == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	return v00*wx0*wy0 + v01*wx1*wy0 + v02*wx2*wy0 + v03*wx3*wy0 +
		v10*wx0*wy1 + v11*wx1*wy1 + v12*wx2*wy1 + v13*wx3*wy1 +
		v20*wx0*wy2 + v21*wx1*wy2 + v22*wx2*wy2 + v23*wx3*wy2 +
		v30*wx0*wy3 + v31*wx1*wy3 + v32*wx2*wy3 + v33*wx3*wy3
}

func getLong(gid *C.codes_handle, key string) C.long {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	var v C.long
	C.codes_get_long(gid, ckey, &v)
	return v
}

func getDouble(gid *C.codes_handle, key string) C.double {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	var v C.double
	C.codes_get_double(gid, ckey, &v)
	return v
}

func getDoubleChecked(gid *C.codes_handle, key string) (float64, bool) {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	var v C.double
	rc := C.codes_get_double(gid, ckey, &v)
	return float64(v), rc == C.CODES_SUCCESS
}

func getDoubleArray(gid *C.codes_handle, key string, out []float64) C.int {
	if len(out) == 0 {
		return C.CODES_SUCCESS
	}
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	n := C.size_t(len(out))
	return C.codes_get_double_array(gid, ckey, (*C.double)(unsafe.Pointer(&out[0])), &n)
}

func getSize(gid *C.codes_handle, key string) (C.size_t, C.int) {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	var n C.size_t
	rc := C.codes_get_size(gid, ckey, &n)
	return n, rc
}

func getString(gid *C.codes_handle, key string) string {
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	var buf [256]C.char
	n := C.size_t(len(buf))
	if C.codes_get_string(gid, ckey, &buf[0], &n) != C.CODES_SUCCESS {
		return ""
	}
	return C.GoString(&buf[0])
}

func ProcessGRIB(gribData []byte) (GRIBFile, error) {
	if len(gribData) == 0 {
		return GRIBFile{}, errors.New("empty grib data")
	}

	dataPtr := unsafe.Pointer(&gribData[0])
	dataSize := C.size_t(len(gribData))

	gid := C.codes_handle_new_from_message(C.codes_context_get_default(), dataPtr, dataSize)
	if gid == nil {
		return GRIBFile{}, errors.New("codes_handle_new_from_message returned nil")
	}
	defer C.codes_handle_delete(gid)

	return processHandle(gid)
}

func ProcessGRIBFile(path string) ([]GRIBFile, error) {
	var out []GRIBFile
	err := ForEachMessage(path, func(g GRIBFile) error {
		out = append(out, g)
		return nil
	})
	return out, err
}

type HeaderInfo struct {
	Header    GribHeader
	Min, Max  float64
	HasFinite bool
}

func ForEachMessage(path string, fn func(GRIBFile) error) error {
	return forEachHandle(path, func(gid *C.codes_handle) error {
		g, err := processHandle(gid)
		if err != nil {
			return err
		}
		return fn(g)
	})
}

// ForEachMessageFiltered visits GRIB messages but only decodes the value array
// (a multi-megabyte cgo round-trip per message) when want returns true. callers
// that filter by header — every encode/extend/compare call site does — should
// prefer this over ForEachMessage to avoid paying for messages they discard.
func ForEachMessageFiltered(path string, want func(*GribHeader) bool, fn func(GRIBFile) error) error {
	return forEachHandle(path, func(gid *C.codes_handle) error {
		h, err := extractHeaderScalars(gid)
		if err != nil {
			return err
		}
		if want != nil && !want(&h) {
			return nil
		}
		g, err := finishProcessHandle(gid, h)
		if err != nil {
			return err
		}
		return fn(g)
	})
}

func ForEachMessageMeta(path string, fn func(HeaderInfo) error) error {
	return forEachHandle(path, func(gid *C.codes_handle) error {
		m, err := processHandleMeta(gid)
		if err != nil {
			return err
		}
		return fn(m)
	})
}

// ForEachMessageMetaFiltered visits message metadata but only extracts the full
// header scalar set when want(shortName) returns true. lets a --filter pass
// reject messages with a single cgo call (getString shortName) instead of the
// ~25 scalar getters processHandleMeta would otherwise issue per message.
func ForEachMessageMetaFiltered(path string, want func(shortName string) bool, fn func(HeaderInfo) error) error {
	return forEachHandle(path, func(gid *C.codes_handle) error {
		shortName := getString(gid, "shortName")
		if want != nil && !want(shortName) {
			return nil
		}
		m, err := processHandleMeta(gid)
		if err != nil {
			return err
		}
		return fn(m)
	})
}

func forEachHandle(path string, fn func(*C.codes_handle) error) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	cmode := C.CString("rb")
	defer C.free(unsafe.Pointer(cmode))

	fp := C.fopen(cpath, cmode)
	if fp == nil {
		return fmt.Errorf("cannot open %s", path)
	}
	defer C.fclose(fp)

	for {
		var rc C.int
		gid := C.codes_handle_new_from_file(C.codes_context_get_default(), fp, C.PRODUCT_GRIB, &rc)
		if gid == nil {
			if rc != 0 && rc != C.GRIB_END_OF_FILE {
				return fmt.Errorf("codes_handle_new_from_file: rc=%d", int(rc))
			}
			return nil
		}
		err := fn(gid)
		C.codes_handle_delete(gid)
		if err != nil {
			return err
		}
	}
}

func extractHeaderScalars(gid *C.codes_handle) (GribHeader, error) {
	nx := getLong(gid, "Ni")
	ny := getLong(gid, "Nj")
	la1 := getDouble(gid, "latitudeOfFirstGridPointInDegrees")
	la2 := getDouble(gid, "latitudeOfLastGridPointInDegrees")
	lo1 := getDouble(gid, "longitudeOfFirstGridPointInDegrees")
	lo2 := getDouble(gid, "longitudeOfLastGridPointInDegrees")
	dx := getDouble(gid, "iDirectionIncrement")
	dy := getDouble(gid, "jDirectionIncrement")
	basicAngle := getDouble(gid, "basicAngleOfTheInitialProductionDomain")
	subdivisions := getDouble(gid, "subdivisionsOfBasicAngle")
	missingValue := getDouble(gid, "missingValue")

	scale := 1e6
	if basicAngle != 0 {
		scale = float64(basicAngle) / float64(subdivisions)
	}

	year := getLong(gid, "year")
	month := getLong(gid, "month")
	day := getLong(gid, "day")
	hour := getLong(gid, "hour")
	minute := getLong(gid, "minute")
	second := getLong(gid, "second")
	timeUnit := getLong(gid, "indicatorOfUnitOfTimeRange")
	forecastTime := getLong(gid, "forecastTime")
	endStep := getLong(gid, "endStep")
	scanMode := getLong(gid, "scanMode")
	discipline := getLong(gid, "discipline")
	parameterCategory := getLong(gid, "parameterCategory")
	parameterNumber := getLong(gid, "parameterNumber")

	gribType := int32((discipline & 0xFF) | ((parameterCategory & 0xFF) << 8) | ((parameterNumber & 0xFF) << 16))

	shortName := getString(gid, "shortName")
	units := getString(gid, "units")
	typeOfLevel := getString(gid, "typeOfLevel")
	level := getLong(gid, "level")
	bottomLevel := getLong(gid, "bottomLevel")

	referenceTime := time.Date(int(year), time.Month(month), int(day), int(hour), int(minute), int(second), 0, time.UTC)

	// GRIB2 Code Table 4.4: indicatorOfUnitOfTimeRange
	var forecastDuration time.Duration
	switch timeUnit {
	case 0:
		forecastDuration = time.Duration(forecastTime) * time.Minute
	case 1:
		forecastDuration = time.Duration(forecastTime) * time.Hour
	case 2:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour
	case 3:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour * 30
	case 4:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour * 365
	case 5:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour * 365 * 10
	case 6:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour * 365 * 30
	case 7:
		forecastDuration = time.Duration(forecastTime) * 24 * time.Hour * 365 * 100
	case 10:
		forecastDuration = time.Duration(forecastTime) * 3 * time.Hour
	case 11:
		forecastDuration = time.Duration(forecastTime) * 6 * time.Hour
	case 12:
		forecastDuration = time.Duration(forecastTime) * 12 * time.Hour
	case 13:
		forecastDuration = time.Duration(forecastTime) * time.Second
	case 255:
	default:
		return GribHeader{}, fmt.Errorf("unsupported time unit: %d", timeUnit)
	}

	return GribHeader{
		Type:              gribType,
		ShortName:         shortName,
		Units:             units,
		Nx:                int(nx),
		Ny:                int(ny),
		La1:               float64(la1),
		La2:               float64(la2),
		Lo1:               float64(lo1),
		Lo2:               float64(lo2),
		DX:                float64(dx) / scale,
		DY:                float64(dy) / scale,
		ScanMode:          int(scanMode),
		Discipline:        int(discipline),
		ParameterCategory: int(parameterCategory),
		ParameterNumber:   int(parameterNumber),
		ReferenceTime:     referenceTime.Add(forecastDuration),
		ForecastTime:      int(forecastTime),
		EndStep:           int(endStep),
		MissingValue:      float64(missingValue),
		TypeOfLevel:       typeOfLevel,
		Level:             int(level),
		BottomLevel:       int(bottomLevel),
	}, nil
}

// gridSig identifies a regular_ll-style coordinate grid: messages with identical
// (Nx,Ny,La1,La2,Lo1,Lo2) produce identical distinctLatitudes/distinctLongitudes,
// so we read them from eccodes once per grid and reuse across messages
type gridSig struct {
	nx, ny             int
	la1, la2, lo1, lo2 float64
}

type distinctCoords struct {
	lats, lons []float64
}

var (
	distinctCacheMu sync.RWMutex
	distinctCache   = map[gridSig]*distinctCoords{}
)

func processHandle(gid *C.codes_handle) (GRIBFile, error) {
	h, err := extractHeaderScalars(gid)
	if err != nil {
		return GRIBFile{}, err
	}
	return finishProcessHandle(gid, h)
}

// finishProcessHandle does the expensive part of decoding a message: distinct
// lat/lon arrays (cached by grid sig) and the values array (~8 MB per GFS msg).
// split out so ForEachMessageFiltered can skip it for unwanted messages
func finishProcessHandle(gid *C.codes_handle, h GribHeader) (GRIBFile, error) {
	sig := gridSig{nx: h.Nx, ny: h.Ny, la1: h.La1, la2: h.La2, lo1: h.Lo1, lo2: h.Lo2}
	distinctCacheMu.RLock()
	dc, ok := distinctCache[sig]
	distinctCacheMu.RUnlock()
	if ok {
		h.DistinctLatitudes = dc.lats
		h.DistinctLongitudes = dc.lons
	} else {
		lats := make([]float64, h.Ny)
		getDoubleArray(gid, "distinctLatitudes", lats)
		lons := make([]float64, h.Nx)
		getDoubleArray(gid, "distinctLongitudes", lons)

		if h.Ny > 1 && h.La2 < h.La1 && lats[len(lats)-1] > lats[0] {
			for i, j := 0, len(lats)-1; i < j; i, j = i+1, j-1 {
				lats[i], lats[j] = lats[j], lats[i]
			}
		}

		distinctCacheMu.Lock()
		if existing, dup := distinctCache[sig]; dup {
			lats, lons = existing.lats, existing.lons
		} else {
			distinctCache[sig] = &distinctCoords{lats: lats, lons: lons}
		}
		distinctCacheMu.Unlock()
		h.DistinctLatitudes = lats
		h.DistinctLongitudes = lons
	}

	numValues, rc := getSize(gid, "values")
	if rc != C.CODES_SUCCESS {
		return GRIBFile{}, fmt.Errorf("codes_get_size(values) failed: %d", int(rc))
	}

	dataValues := make([]float64, numValues)
	if rc := getDoubleArray(gid, "values", dataValues); rc != C.CODES_SUCCESS {
		return GRIBFile{}, fmt.Errorf("codes_get_double_array(values) failed: %d", int(rc))
	}

	return GRIBFile{Header: h, DataValues: dataValues}, nil
}

func processHandleMeta(gid *C.codes_handle) (HeaderInfo, error) {
	h, err := extractHeaderScalars(gid)
	if err != nil {
		return HeaderInfo{}, err
	}
	minV, minOK := getDoubleChecked(gid, "minimum")
	maxV, maxOK := getDoubleChecked(gid, "maximum")
	hasFinite := minOK && maxOK &&
		!math.IsNaN(minV) && !math.IsNaN(maxV) &&
		minV != h.MissingValue && maxV != h.MissingValue &&
		minV <= maxV
	return HeaderInfo{Header: h, Min: minV, Max: maxV, HasFinite: hasFinite}, nil
}
