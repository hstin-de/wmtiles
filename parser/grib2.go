package parser

// thin cgo wrapper over ECMWF's eccodes C library: handles GRIB2 message parsing,
// since pure-Go GRIB readers don't cover the WMO templates we need

/*
#cgo CFLAGS: -I/usr/include/x86_64-linux-gnu/
#cgo LDFLAGS: -leccodes
#include "wmt_eccodes.h"
#include <stdio.h>
#include <stdlib.h>
*/
import "C"
import (
	"bytes"
	"encoding/binary"
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
	DataValues []float32
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

	return float64(g.DataValues[y*g.Header.Nx+x])
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

	if float64(v00) == g.Header.MissingValue || float64(v01) == g.Header.MissingValue || float64(v02) == g.Header.MissingValue || float64(v03) == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v10 := data[y0*width+x0m1]
	v11 := data[y0*width+x0]
	v12 := data[y0*width+x0p1]
	v13 := data[y0*width+x0p2]

	if float64(v10) == g.Header.MissingValue || float64(v11) == g.Header.MissingValue || float64(v12) == g.Header.MissingValue || float64(v13) == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v20 := data[y0p1*width+x0m1]
	v21 := data[y0p1*width+x0]
	v22 := data[y0p1*width+x0p1]
	v23 := data[y0p1*width+x0p2]

	if float64(v20) == g.Header.MissingValue || float64(v21) == g.Header.MissingValue || float64(v22) == g.Header.MissingValue || float64(v23) == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	v30 := data[y0p2*width+x0m1]
	v31 := data[y0p2*width+x0]
	v32 := data[y0p2*width+x0p1]
	v33 := data[y0p2*width+x0p2]

	if float64(v30) == g.Header.MissingValue || float64(v31) == g.Header.MissingValue || float64(v32) == g.Header.MissingValue || float64(v33) == g.Header.MissingValue {
		return g.Header.MissingValue
	}

	return float64(v00)*wx0*wy0 + float64(v01)*wx1*wy0 + float64(v02)*wx2*wy0 + float64(v03)*wx3*wy0 +
		float64(v10)*wx0*wy1 + float64(v11)*wx1*wy1 + float64(v12)*wx2*wy1 + float64(v13)*wx3*wy1 +
		float64(v20)*wx0*wy2 + float64(v21)*wx1*wy2 + float64(v22)*wx2*wy2 + float64(v23)*wx3*wy2 +
		float64(v30)*wx0*wy3 + float64(v31)*wx1*wy3 + float64(v32)*wx2*wy3 + float64(v33)*wx3*wy3
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

func getFloatArray(gid *C.codes_handle, key string, out []float32) C.int {
	if len(out) == 0 {
		return C.CODES_SUCCESS
	}
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	n := C.size_t(len(out))
	return C.wmt_get_float_array(gid, ckey, (*C.float)(unsafe.Pointer(&out[0])), &n)
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

// ForEachMessageHeaderFiltered visits header scalars only. Eccodes computes
// minimum/maximum by scanning the data section, so collecting them in a
// pre-pass duplicates exactly what the value pass already does.
func ForEachMessageHeaderFiltered(path string, want func(shortName string) bool, fn func(GribHeader) error) error {
	return forEachHandle(path, func(gid *C.codes_handle) error {
		shortName := getString(gid, "shortName")
		if want != nil && !want(shortName) {
			return nil
		}
		h, err := extractHeaderScalars(gid)
		if err != nil {
			return err
		}
		return fn(h)
	})
}

// ForEachMessageBytes visits every GRIB2 message contained in data.
func ForEachMessageBytes(data []byte, fn func(GRIBFile) error) error {
	return forEachHandleBytes(data, func(gid *C.codes_handle) error {
		g, err := processHandle(gid)
		if err != nil {
			return err
		}
		return fn(g)
	})
}

// ForEachMessageBytesFiltered visits GRIB2 messages in data, decoding values
// only for messages whose header matches want.
func ForEachMessageBytesFiltered(data []byte, want func(*GribHeader) bool, fn func(GRIBFile) error) error {
	return forEachHandleBytes(data, func(gid *C.codes_handle) error {
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

// ForEachMessageMetaBytes visits metadata for every GRIB2 message in data.
func ForEachMessageMetaBytes(data []byte, fn func(HeaderInfo) error) error {
	return forEachHandleBytes(data, func(gid *C.codes_handle) error {
		m, err := processHandleMeta(gid)
		if err != nil {
			return err
		}
		return fn(m)
	})
}

// ForEachMessageMetaBytesFiltered visits metadata for GRIB2 messages in data
// whose shortName matches want.
func ForEachMessageMetaBytesFiltered(data []byte, want func(shortName string) bool, fn func(HeaderInfo) error) error {
	return forEachHandleBytes(data, func(gid *C.codes_handle) error {
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

func ForEachMessageHeaderBytesFiltered(data []byte, want func(shortName string) bool, fn func(GribHeader) error) error {
	return forEachHandleBytes(data, func(gid *C.codes_handle) error {
		shortName := getString(gid, "shortName")
		if want != nil && !want(shortName) {
			return nil
		}
		h, err := extractHeaderScalars(gid)
		if err != nil {
			return err
		}
		return fn(h)
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

func forEachHandleBytes(data []byte, fn func(*C.codes_handle) error) error {
	if len(data) == 0 {
		return errors.New("empty grib data")
	}
	pos := 0
	seen := 0
	for {
		i := bytes.Index(data[pos:], []byte("GRIB"))
		if i < 0 {
			if seen == 0 {
				return errors.New("no GRIB message found")
			}
			return nil
		}
		start := pos + i
		if len(data)-start < 16 {
			return fmt.Errorf("truncated GRIB message header at byte %d", start)
		}
		if edition := data[start+7]; edition != 2 {
			return fmt.Errorf("unsupported GRIB edition %d at byte %d", edition, start)
		}
		msgLen := binary.BigEndian.Uint64(data[start+8 : start+16])
		if msgLen < 16 {
			return fmt.Errorf("invalid GRIB message length %d at byte %d", msgLen, start)
		}
		if msgLen > uint64(len(data)-start) {
			return fmt.Errorf("truncated GRIB message at byte %d: length %d, remaining %d",
				start, msgLen, len(data)-start)
		}

		end := start + int(msgLen)
		msg := data[start:end]
		gid := C.codes_handle_new_from_message(
			C.codes_context_get_default(),
			unsafe.Pointer(&msg[0]),
			C.size_t(len(msg)),
		)
		if gid == nil {
			return fmt.Errorf("codes_handle_new_from_message returned nil at byte %d", start)
		}
		err := fn(gid)
		C.codes_handle_delete(gid)
		if err != nil {
			return err
		}
		seen++
		pos = end
	}
}

func extractHeaderScalars(gid *C.codes_handle) (GribHeader, error) {
	var sc C.wmt_scalars_t
	C.wmt_read_scalars(gid, &sc)
	return scalarsToHeader(&sc)
}

func scalarsToHeader(sc *C.wmt_scalars_t) (GribHeader, error) {
	scale := 1e6
	if sc.basicAngle != 0 {
		scale = float64(sc.basicAngle) / float64(sc.subdivisions)
	}

	gribType := int32((int64(sc.discipline) & 0xFF) |
		((int64(sc.parameterCategory) & 0xFF) << 8) |
		((int64(sc.parameterNumber) & 0xFF) << 16))

	referenceTime := time.Date(int(sc.year), time.Month(sc.month), int(sc.day),
		int(sc.hour), int(sc.minute), int(sc.second), 0, time.UTC)

	// GRIB2 Code Table 4.4
	forecastTime := int64(sc.forecastTime)
	var forecastDuration time.Duration
	switch sc.timeUnit {
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
		return GribHeader{}, fmt.Errorf("unsupported time unit: %d", int(sc.timeUnit))
	}

	return GribHeader{
		Type:              gribType,
		ShortName:         C.GoString(&sc.shortName[0]),
		Units:             C.GoString(&sc.units[0]),
		Nx:                int(sc.ni),
		Ny:                int(sc.nj),
		La1:               float64(sc.la1),
		La2:               float64(sc.la2),
		Lo1:               float64(sc.lo1),
		Lo2:               float64(sc.lo2),
		DX:                float64(sc.dx) / scale,
		DY:                float64(sc.dy) / scale,
		Discipline:        int(sc.discipline),
		ParameterCategory: int(sc.parameterCategory),
		ParameterNumber:   int(sc.parameterNumber),
		ReferenceTime:     referenceTime.Add(forecastDuration),
		ForecastTime:      int(forecastTime),
		MissingValue:      float64(sc.missingValue),
		TypeOfLevel:       C.GoString(&sc.typeOfLevel[0]),
		Level:             int(sc.level),
		BottomLevel:       int(sc.bottomLevel),
	}, nil
}

func attachDistinct(h *GribHeader, msg []byte) error {
	sig := gridSig{nx: h.Nx, ny: h.Ny, la1: h.La1, la2: h.La2, lo1: h.Lo1, lo2: h.Lo2}
	distinctCacheMu.Lock()
	dc, ok := distinctCache[sig]
	if ok {
		distinctCacheMu.Unlock()
		<-dc.ready
		h.DistinctLatitudes = dc.lats
		h.DistinctLongitudes = dc.lons
		return nil
	}
	dc = &distinctCoords{ready: make(chan struct{})}
	distinctCache[sig] = dc
	distinctCacheMu.Unlock()

	gid := C.codes_handle_new_from_message(
		C.codes_context_get_default(),
		unsafe.Pointer(&msg[0]),
		C.size_t(len(msg)),
	)
	if gid == nil {
		distinctCacheMu.Lock()
		delete(distinctCache, sig)
		distinctCacheMu.Unlock()
		close(dc.ready)
		return errors.New("codes_handle_new_from_message returned nil (distinct)")
	}
	lats := make([]float64, h.Ny)
	getDoubleArray(gid, "distinctLatitudes", lats)
	lons := make([]float64, h.Nx)
	getDoubleArray(gid, "distinctLongitudes", lons)
	C.codes_handle_delete(gid)
	if h.Ny > 1 && h.La2 < h.La1 && lats[len(lats)-1] > lats[0] {
		for i, j := 0, len(lats)-1; i < j; i, j = i+1, j-1 {
			lats[i], lats[j] = lats[j], lats[i]
		}
	}
	dc.lats = lats
	dc.lons = lons
	close(dc.ready)
	h.DistinctLatitudes = lats
	h.DistinctLongitudes = lons
	return nil
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
	ready      chan struct{}
}

var (
	distinctCacheMu sync.Mutex
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
	// single-flight on cache miss; otherwise concurrent workers all redo the eccodes lookup.
	distinctCacheMu.Lock()
	dc, ok := distinctCache[sig]
	if !ok {
		dc = &distinctCoords{ready: make(chan struct{})}
		distinctCache[sig] = dc
		distinctCacheMu.Unlock()

		lats := make([]float64, h.Ny)
		getDoubleArray(gid, "distinctLatitudes", lats)
		lons := make([]float64, h.Nx)
		getDoubleArray(gid, "distinctLongitudes", lons)
		if h.Ny > 1 && h.La2 < h.La1 && lats[len(lats)-1] > lats[0] {
			for i, j := 0, len(lats)-1; i < j; i, j = i+1, j-1 {
				lats[i], lats[j] = lats[j], lats[i]
			}
		}
		dc.lats = lats
		dc.lons = lons
		close(dc.ready)
	} else {
		distinctCacheMu.Unlock()
		<-dc.ready
	}
	h.DistinctLatitudes = dc.lats
	h.DistinctLongitudes = dc.lons

	numValues, rc := getSize(gid, "values")
	if rc != C.CODES_SUCCESS {
		return GRIBFile{}, fmt.Errorf("codes_get_size(values) failed: %d", int(rc))
	}

	dataValues := make([]float32, numValues)
	if rc := getFloatArray(gid, "values", dataValues); rc != C.CODES_SUCCESS {
		return GRIBFile{}, fmt.Errorf("codes_get_float_array(values) failed: %d", int(rc))
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
