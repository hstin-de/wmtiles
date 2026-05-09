package parser

// Pure-Go fast path for grid template 3.0 + simple packing 5.0; eccodes'
// global-context lock caps parallel parses at ~2x otherwise.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

func FastScanMetadata(msg []byte) (h GribHeader, ok bool, err error) {
	g, _, dok, err := decodeFastPath(msg, nil, true)
	if err != nil || !dok {
		return GribHeader{}, dok, err
	}
	return g.Header, true, nil
}

func FastDecodeRegularLL(msg []byte, scratch []float32) (g GRIBFile, values []float32, ok bool, err error) {
	g, values, _, ok, err = decodeFastPathStats(msg, scratch, false, false)
	return g, values, ok, err
}

func FastDecodeRegularLLStats(msg []byte, scratch []float32) (g GRIBFile, values []float32, st Stats, ok bool, err error) {
	return decodeFastPathStats(msg, scratch, false, true)
}

type Stats struct {
	Min, Max  float64
	HasFinite bool
}

func decodeFastPath(msg []byte, scratch []float32, metaOnly bool) (g GRIBFile, values []float32, ok bool, err error) {
	g, values, _, ok, err = decodeFastPathStats(msg, scratch, metaOnly, false)
	return g, values, ok, err
}

func decodeFastPathStats(msg []byte, scratch []float32, metaOnly, withStats bool) (g GRIBFile, values []float32, st Stats, ok bool, err error) {
	if len(msg) < 16 {
		return GRIBFile{}, scratch, Stats{}, false, errors.New("grib msg too short")
	}
	if string(msg[:4]) != "GRIB" {
		return GRIBFile{}, scratch, Stats{}, false, errors.New("not a GRIB message")
	}
	if msg[7] != 2 {
		return GRIBFile{}, scratch, Stats{}, false, fmt.Errorf("unsupported edition %d", msg[7])
	}
	totalLen := int(binary.BigEndian.Uint64(msg[8:16]))
	if totalLen != len(msg) {
		return GRIBFile{}, scratch, Stats{}, false, fmt.Errorf("msg length mismatch: %d vs %d", totalLen, len(msg))
	}
	discipline := int(msg[6])

	var (
		gotS1, gotS3, gotS4, gotS5, gotS7 bool
		hasBitmap                         bool
		bitmap                            []byte

		year, month, day, hour, minute, second int

		ni, nj                                                       int
		la1Raw, la2Raw, lo1Raw, lo2Raw                               int32
		dxRaw, dyRaw                                                 uint32
		basicAngle, subdivisions                                     uint32
		gridTplNum                                                   uint16
		productTplNum                                                uint16
		paramCategory, paramNumber                                   int
		typeOfFirstSurface, typeOfSecondSurface                      int
		scaleFactorFirst, scaleFactorSecond                          int8
		scaledValueFirst, scaledValueSecond                          int32
		timeUnit, forecastTime                                       int
		dataRepTplNum                                                uint16
		numberOfValues                                               int
		referenceValue                                               float32
		binaryScale, decimalScale                                    int16
		bitsPerValue                                                 int
		dataSection                                                  []byte
	)

	pos := 16
	for pos < totalLen-4 {
		if pos+4 > totalLen {
			return GRIBFile{}, scratch, Stats{}, false, errors.New("truncated section header")
		}
		if string(msg[pos:pos+4]) == "7777" {
			break
		}
		secLen := int(binary.BigEndian.Uint32(msg[pos : pos+4]))
		if secLen < 5 || pos+secLen > totalLen {
			return GRIBFile{}, scratch, Stats{}, false, fmt.Errorf("invalid section length %d at %d", secLen, pos)
		}
		secNum := msg[pos+4]
		body := msg[pos : pos+secLen]
		switch secNum {
		case 1:
			if secLen < 21 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 1 too short")
			}
			year = int(binary.BigEndian.Uint16(body[12:14]))
			month = int(body[14])
			day = int(body[15])
			hour = int(body[16])
			minute = int(body[17])
			second = int(body[18])
			gotS1 = true
		case 3:
			if secLen < 14 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 3 too short")
			}
			gridTplNum = binary.BigEndian.Uint16(body[12:14])
			if gridTplNum != 0 {
				return GRIBFile{}, scratch, Stats{}, false, nil
			}
			if secLen < 72 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 3 template 0 too short")
			}
			ni = int(binary.BigEndian.Uint32(body[30:34]))
			nj = int(binary.BigEndian.Uint32(body[34:38]))
			basicAngle = binary.BigEndian.Uint32(body[38:42])
			subdivisions = binary.BigEndian.Uint32(body[42:46])
			la1Raw = decodeGribInt32(body[46:50])
			lo1Raw = decodeGribInt32(body[50:54])
			la2Raw = decodeGribInt32(body[55:59])
			lo2Raw = decodeGribInt32(body[59:63])
			dxRaw = binary.BigEndian.Uint32(body[63:67])
			dyRaw = binary.BigEndian.Uint32(body[67:71])
			gotS3 = true
		case 4:
			if secLen < 9 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 4 too short")
			}
			productTplNum = binary.BigEndian.Uint16(body[7:9])
			if productTplNum != 0 && productTplNum != 1 && productTplNum != 8 {
				return GRIBFile{}, scratch, Stats{}, false, nil
			}
			if secLen < 34 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 4 template 0 too short")
			}
			paramCategory = int(body[9])
			paramNumber = int(body[10])
			timeUnit = int(body[17])
			forecastTime = int(int32(binary.BigEndian.Uint32(body[18:22])))
			typeOfFirstSurface = int(body[22])
			scaleFactorFirst = int8(body[23])
			scaledValueFirst = int32(binary.BigEndian.Uint32(body[24:28]))
			typeOfSecondSurface = int(body[28])
			scaleFactorSecond = int8(body[29])
			scaledValueSecond = int32(binary.BigEndian.Uint32(body[30:34]))
			gotS4 = true
		case 5:
			if secLen < 11 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 5 too short")
			}
			numberOfValues = int(binary.BigEndian.Uint32(body[5:9]))
			dataRepTplNum = binary.BigEndian.Uint16(body[9:11])
			if dataRepTplNum != 0 {
				return GRIBFile{}, scratch, Stats{}, false, nil
			}
			if secLen < 21 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 5 template 0 too short")
			}
			refBits := binary.BigEndian.Uint32(body[11:15])
			referenceValue = math.Float32frombits(refBits)
			binaryScale = decodeGribInt16(body[15:17])
			decimalScale = decodeGribInt16(body[17:19])
			bitsPerValue = int(body[19])
			gotS5 = true
		case 6:
			if secLen < 6 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 6 too short")
			}
			ind := body[5]
			switch ind {
			case 255:
				hasBitmap = false
			case 0:
				hasBitmap = true
				bitmap = body[6:secLen]
			default:
				return GRIBFile{}, scratch, Stats{}, false, nil
			}
		case 7:
			if secLen < 5 {
				return GRIBFile{}, scratch, Stats{}, false, errors.New("section 7 too short")
			}
			dataSection = body[5:secLen]
			gotS7 = true
		}
		pos += secLen
	}
	if !(gotS1 && gotS3 && gotS4 && gotS5 && gotS7) {
		return GRIBFile{}, scratch, Stats{}, false, nil
	}

	if numberOfValues > ni*nj {
		return GRIBFile{}, scratch, Stats{}, false, fmt.Errorf("numberOfValues %d > ni*nj %d", numberOfValues, ni*nj)
	}
	totalPoints := ni * nj

	var scale float64 = 1e6
	if basicAngle != 0 && subdivisions != 0xFFFFFFFF {
		scale = float64(subdivisions) / float64(basicAngle)
	}
	invScale := 1.0 / scale

	la1Deg := float64(la1Raw) * invScale
	la2Deg := float64(la2Raw) * invScale
	lo1Deg := float64(lo1Raw) * invScale
	lo2Deg := float64(lo2Raw) * invScale
	dxDeg := float64(dxRaw) * invScale
	dyDeg := float64(dyRaw) * invScale

	missingValue := 9999.0

	typeOfLevel, level, bottomLevel := decodeLevel(
		typeOfFirstSurface, scaleFactorFirst, scaledValueFirst,
		typeOfSecondSurface, scaleFactorSecond, scaledValueSecond,
	)
	shortName, units := lookupShortName(discipline, paramCategory, paramNumber, typeOfFirstSurface, level)

	referenceTime := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	forecastDuration := timeUnitDuration(timeUnit, forecastTime)

	gribType := int32((discipline & 0xFF) | ((paramCategory & 0xFF) << 8) | ((paramNumber & 0xFF) << 16))

	if !metaOnly {
		if cap(scratch) < totalPoints {
			scratch = make([]float32, totalPoints)
		} else {
			scratch = scratch[:totalPoints]
		}

		var es error
		st, es = unpackSimplePackedStats(dataSection, bitmap, hasBitmap, totalPoints, numberOfValues,
			bitsPerValue, referenceValue, binaryScale, decimalScale, float32(missingValue), scratch, withStats)
		if es != nil {
			return GRIBFile{}, scratch, Stats{}, false, es
		}
	}

	h := GribHeader{
		Type:              gribType,
		ShortName:         shortName,
		Units:             units,
		Nx:                ni,
		Ny:                nj,
		La1:               la1Deg,
		La2:               la2Deg,
		Lo1:               lo1Deg,
		Lo2:               lo2Deg,
		DX:                dxDeg,
		DY:                dyDeg,
		Discipline:        discipline,
		ParameterCategory: paramCategory,
		ParameterNumber:   paramNumber,
		ReferenceTime:     referenceTime.Add(forecastDuration),
		ForecastTime:      forecastTime,
		MissingValue:      missingValue,
		TypeOfLevel:       typeOfLevel,
		Level:             level,
		BottomLevel:       bottomLevel,
	}

	if err := buildDistinct(&h); err != nil {
		return GRIBFile{}, scratch, Stats{}, false, err
	}

	if metaOnly {
		return GRIBFile{Header: h}, nil, Stats{}, true, nil
	}
	return GRIBFile{Header: h, DataValues: scratch}, scratch, st, true, nil
}

// decodeGribInt16 / decodeGribInt32 use GRIB2 sign-bit-magnitude (high bit = sign).
func decodeGribInt16(b []byte) int16 {
	v := binary.BigEndian.Uint16(b)
	if v&0x8000 != 0 {
		return -int16(v &^ 0x8000)
	}
	return int16(v)
}

func decodeGribInt32(b []byte) int32 {
	v := binary.BigEndian.Uint32(b)
	if v&0x80000000 != 0 {
		return -int32(v &^ 0x80000000)
	}
	return int32(v)
}

// unpackSimplePackedStats decodes values into out and, when withStats is
// true, tracks min/max on the packed integer codes (one mul at the end
// instead of a second pass over the float32 array).
func unpackSimplePackedStats(data, bitmap []byte, hasBitmap bool,
	totalPoints, numberOfValues, bitsPerValue int,
	referenceValue float32, binaryScale, decimalScale int16, missing float32,
	out []float32, withStats bool) (Stats, error) {

	var st Stats
	if bitsPerValue == 0 {
		v := float32(float64(referenceValue) / math.Pow10(int(decimalScale)))
		fillF32(out[:totalPoints], v)
		if hasBitmap {
			applyBitmapMissing(out[:totalPoints], bitmap, missing)
		}
		if withStats {
			st.Min = float64(v)
			st.Max = float64(v)
			st.HasFinite = totalPoints > 0
			if hasBitmap {
				st.HasFinite = false
				for i := 0; i < (totalPoints+7)>>3; i++ {
					if bitmap[i] != 0 {
						st.HasFinite = true
						break
					}
				}
			}
		}
		return st, nil
	}

	scaleBin := math.Ldexp(1, int(binaryScale))
	scaleDec := math.Pow10(-int(decimalScale))
	scale := float32(scaleBin * scaleDec)
	ref := float32(float64(referenceValue) * scaleDec)

	var minX, maxX uint32 = ^uint32(0), 0
	var anyFinite bool

	if bitsPerValue == 16 && !hasBitmap {
		if len(data)*8 < totalPoints*16 {
			return st, fmt.Errorf("section 7 truncated: %d bytes for %d×16-bit", len(data), totalPoints)
		}
		if withStats {
			for i := 0; i < totalPoints; i++ {
				x := uint32(data[2*i])<<8 | uint32(data[2*i+1])
				out[i] = ref + float32(x)*scale
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
			anyFinite = totalPoints > 0
		} else {
			for i := 0; i < totalPoints; i++ {
				x := uint32(data[2*i])<<8 | uint32(data[2*i+1])
				out[i] = ref + float32(x)*scale
			}
		}
	} else if bitsPerValue == 16 && hasBitmap {
		if len(data) < numberOfValues*2 {
			return st, fmt.Errorf("section 7 truncated: %d bytes for %d×16-bit", len(data), numberOfValues)
		}
		di := 0
		if withStats {
			for i := 0; i < totalPoints; i++ {
				bit := bitmap[i>>3] & (1 << (7 - uint(i&7)))
				if bit == 0 {
					out[i] = missing
					continue
				}
				x := uint32(data[2*di])<<8 | uint32(data[2*di+1])
				di++
				out[i] = ref + float32(x)*scale
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				anyFinite = true
			}
		} else {
			for i := 0; i < totalPoints; i++ {
				bit := bitmap[i>>3] & (1 << (7 - uint(i&7)))
				if bit == 0 {
					out[i] = missing
					continue
				}
				x := uint32(data[2*di])<<8 | uint32(data[2*di+1])
				di++
				out[i] = ref + float32(x)*scale
			}
		}
	} else if bitsPerValue == 8 && !hasBitmap {
		for i := 0; i < totalPoints; i++ {
			x := uint32(data[i])
			out[i] = ref + float32(x)*scale
			if withStats {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
		}
		if withStats {
			anyFinite = totalPoints > 0
		}
	} else if bitsPerValue == 8 && hasBitmap {
		di := 0
		for i := 0; i < totalPoints; i++ {
			bit := bitmap[i>>3] & (1 << (7 - uint(i&7)))
			if bit == 0 {
				out[i] = missing
				continue
			}
			x := uint32(data[di])
			out[i] = ref + float32(x)*scale
			di++
			if withStats {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				anyFinite = true
			}
		}
	} else {
		count := numberOfValues
		if !hasBitmap {
			count = totalPoints
		}
		values := make([]uint32, count)
		if err := unpackBits(data, count, bitsPerValue, values); err != nil {
			return st, err
		}
		if !hasBitmap {
			for i := 0; i < totalPoints; i++ {
				x := values[i]
				out[i] = ref + float32(x)*scale
				if withStats {
					if x < minX {
						minX = x
					}
					if x > maxX {
						maxX = x
					}
				}
			}
			if withStats {
				anyFinite = totalPoints > 0
			}
		} else {
			di := 0
			for i := 0; i < totalPoints; i++ {
				bit := bitmap[i>>3] & (1 << (7 - uint(i&7)))
				if bit == 0 {
					out[i] = missing
					continue
				}
				x := values[di]
				out[i] = ref + float32(x)*scale
				di++
				if withStats {
					if x < minX {
						minX = x
					}
					if x > maxX {
						maxX = x
					}
					anyFinite = true
				}
			}
		}
	}

	if withStats && anyFinite {
		st.Min = float64(ref + float32(minX)*scale)
		st.Max = float64(ref + float32(maxX)*scale)
		st.HasFinite = true
	}
	return st, nil
}

func unpackBits(data []byte, count, nb int, out []uint32) error {
	if nb < 1 || nb > 32 {
		return fmt.Errorf("unsupported bitsPerValue %d", nb)
	}
	mask := uint64(1)<<uint(nb) - 1
	var bitOff int
	totalBits := count * nb
	if len(data)*8 < totalBits {
		return fmt.Errorf("section 7 truncated for bit-unpacking")
	}
	for i := 0; i < count; i++ {
		byteIdx := bitOff >> 3
		bitInByte := bitOff & 7
		var w uint64
		end := byteIdx + 8
		if end > len(data) {
			end = len(data)
		}
		for j := byteIdx; j < end; j++ {
			w = (w << 8) | uint64(data[j])
		}
		shift := 8*(end-byteIdx) - bitInByte - nb
		out[i] = uint32((w >> uint(shift)) & mask)
		bitOff += nb
	}
	return nil
}

func fillF32(b []float32, v float32) {
	for i := range b {
		b[i] = v
	}
}

func applyBitmapMissing(out []float32, bitmap []byte, missing float32) {
	for i := range out {
		bit := bitmap[i>>3] & (1 << (7 - uint(i&7)))
		if bit == 0 {
			out[i] = missing
		}
	}
}

// GRIB2 Code Table 4.4.
func timeUnitDuration(unit, forecast int) time.Duration {
	switch unit {
	case 0:
		return time.Duration(forecast) * time.Minute
	case 1:
		return time.Duration(forecast) * time.Hour
	case 2:
		return time.Duration(forecast) * 24 * time.Hour
	case 3:
		return time.Duration(forecast) * 24 * time.Hour * 30
	case 4:
		return time.Duration(forecast) * 24 * time.Hour * 365
	case 5:
		return time.Duration(forecast) * 24 * time.Hour * 365 * 10
	case 6:
		return time.Duration(forecast) * 24 * time.Hour * 365 * 30
	case 7:
		return time.Duration(forecast) * 24 * time.Hour * 365 * 100
	case 10:
		return time.Duration(forecast) * 3 * time.Hour
	case 11:
		return time.Duration(forecast) * 6 * time.Hour
	case 12:
		return time.Duration(forecast) * 12 * time.Hour
	case 13:
		return time.Duration(forecast) * time.Second
	}
	return 0
}

func decodeLevel(typeFirst int, sfFirst int8, valFirst int32,
	typeSecond int, sfSecond int8, valSecond int32) (string, int, int) {
	scale1 := scaleFactorToFloat(sfFirst)
	scale2 := scaleFactorToFloat(sfSecond)
	val1 := int(float64(valFirst) * scale1)
	val2 := int(float64(valSecond) * scale2)
	if typeSecond == 255 || typeSecond == 0 {
		val2 = 0
	}
	switch typeFirst {
	case 0, 255:
		return "", 0, 0
	case 1:
		return "surface", 0, 0
	case 100:
		return "isobaricInhPa", val1 / 100, 0
	case 103:
		return "heightAboveGround", val1, 0
	case 101:
		return "meanSea", 0, 0
	case 102:
		return "heightAboveSea", val1, 0
	case 105:
		return "hybrid", val1, 0
	case 106:
		if typeSecond == 106 {
			return "depthBelowLandLayer", val1, val2
		}
		return "depthBelowLand", val1, 0
	case 108:
		return "depthBelowSea", val1, 0
	case 200:
		return "entireAtmosphere", 0, 0
	case 220:
		return "atmosphereSingleLayer", 0, 0
	case 8:
		return "nominalTop", 0, 0
	}
	return fmt.Sprintf("level_%d", typeFirst), val1, val2
}

func scaleFactorToFloat(sf int8) float64 {
	if sf == -127 || sf == 0 {
		return 1
	}
	return math.Pow10(-int(sf))
}

// For heightAboveGround surfaces eccodes prepends the level metres to the
// base shortName ("2t", "10u"); empty result falls back to "param_d_c_p".
func lookupShortName(d, c, p, surface, level int) (string, string) {
	base, units := baseShortName(d, c, p)
	if base == "" {
		return "", units
	}
	if surface == 103 && level > 0 && level < 100 {
		switch base {
		case "t", "u", "v", "ws", "si10", "wgust":
			return fmt.Sprintf("%d%s", level, base), units
		}
	}
	return base, units
}

func baseShortName(d, c, p int) (string, string) {
	switch {
	case d == 0 && c == 0 && p == 0:
		return "t", "K"
	case d == 0 && c == 0 && p == 6:
		return "tmax", "K"
	case d == 0 && c == 0 && p == 7:
		return "tmin", "K"
	case d == 0 && c == 0 && p == 17:
		return "skt", "K"
	case d == 0 && c == 1 && p == 0:
		return "q", "kg kg-1"
	case d == 0 && c == 1 && p == 1:
		return "r", "%"
	case d == 0 && c == 1 && p == 8:
		return "tp", "kg m-2"
	case d == 0 && c == 1 && p == 11:
		return "sd", "kg m-2"
	case d == 0 && c == 2 && p == 1:
		return "ws", "m s-1"
	case d == 0 && c == 2 && p == 2:
		return "u", "m s-1"
	case d == 0 && c == 2 && p == 3:
		return "v", "m s-1"
	case d == 0 && c == 2 && p == 22:
		return "wgust", "m s-1"
	case d == 0 && c == 3 && p == 0:
		return "pres", "Pa"
	case d == 0 && c == 3 && p == 1:
		return "prmsl", "Pa"
	case d == 0 && c == 3 && p == 5:
		return "h", "gpm"
	case d == 0 && c == 6 && p == 1:
		return "tcc", "%"
	case d == 0 && c == 6 && p == 3:
		return "lcc", "%"
	case d == 0 && c == 6 && p == 4:
		return "mcc", "%"
	case d == 0 && c == 6 && p == 5:
		return "hcc", "%"
	case d == 0 && c == 4 && p == 7:
		return "ssrd", "J m-2"
	case d == 0 && c == 4 && p == 9:
		return "ssr", "J m-2"
	case d == 2 && c == 0 && p == 0:
		return "lsm", ""
	}
	return "", ""
}

// Step from a normalised lon0 using DX/DY. eccodes' distinctLongitudes
// wraps antimeridian-crossing grids into a contiguous monotone range
// (e.g. [-3.94..20.34] not [356.06..380.34]); the tiler's lonGrid0
// detection assumes the same convention.
func buildDistinct(h *GribHeader) error {
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

	lats := make([]float64, h.Ny)
	lons := make([]float64, h.Nx)

	dy := h.DY
	if h.Ny > 1 {
		if h.La2 < h.La1 {
			dy = -dy
		}
		for i := 0; i < h.Ny; i++ {
			lats[i] = h.La1 + dy*float64(i)
		}
	} else if h.Ny == 1 {
		lats[0] = h.La1
	}

	// eccodes returns distinctLongitudes in raw form when Lo1 <= Lo2 (e.g.
	// a global GFS grid stays in [0, 360)); only antimeridian-crossing
	// grids (Lo1 > Lo2, e.g. ICON-D2 around 0°) are normalised into a
	// monotone window starting at Lo1-360.
	if h.Nx > 1 {
		dx := h.DX
		lo0 := h.Lo1
		if h.Lo1 > h.Lo2 {
			lo0 = h.Lo1 - 360
		}
		for i := 0; i < h.Nx; i++ {
			lons[i] = lo0 + dx*float64(i)
		}
	} else if h.Nx == 1 {
		lons[0] = h.Lo1
	}

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
