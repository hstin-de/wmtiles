package quantize

import (
	"math"
)

type DType uint8

const (
	DTypeU8  DType = 0
	DTypeU16 DType = 1
	DTypeF32 DType = 3
)

// pin every NaN to the same bit pattern so identical-payload tiles hash identically and dedup
const CanonicalQuietNaN uint32 = 0x7FC00000

// reserved top values for missing/NaN: valid range is [0, max-1]
const (
	SentinelU8  uint8  = 0xFF
	SentinelU16 uint16 = 0xFFFF
)

func (d DType) Bytes() int {
	switch d {
	case DTypeU8:
		return 1
	case DTypeU16:
		return 2
	case DTypeF32:
		return 4
	}
	return 0
}

type Params struct {
	DType  DType
	Scale  float64
	Offset float64
}

func (p Params) MaxQ() uint32 {
	switch p.DType {
	case DTypeU8:
		return 254
	case DTypeU16:
		return 65534
	}
	return 0
}

func FitParams(vmin, vmax, precision float64) Params {
	if !(vmin <= vmax) {
		return Params{DType: DTypeF32, Scale: 1, Offset: 0}
	}
	if vmin == vmax {
		return Params{DType: DTypeU16, Scale: 0, Offset: vmin}
	}
	rng := vmax - vmin
	if precision > 0 {
		needU8 := math.Ceil(rng/precision) + 1
		if needU8 <= 255 {
			return Params{DType: DTypeU8, Scale: rng / 254.0, Offset: vmin} // 254 not 255: top slot is the NaN sentinel
		}
		needU16 := math.Ceil(rng/precision) + 1
		if needU16 <= 65535 {
			return Params{DType: DTypeU16, Scale: rng / 65534.0, Offset: vmin} // 65534 not 65535, sentinel reserved
		}
		return Params{DType: DTypeF32, Scale: 1, Offset: 0}
	}
	return Params{DType: DTypeU16, Scale: rng / 65534.0, Offset: vmin}
}

func QuantizeU8(values []float32, p Params, out []byte) {
	if p.Scale == 0 {
		for i := range values {
			if isNaN32(values[i]) {
				out[i] = SentinelU8
			} else {
				out[i] = 0
			}
		}
		return
	}
	inv := 1.0 / p.Scale
	for i, v := range values {
		if isNaN32(v) {
			out[i] = SentinelU8
			continue
		}
		q := math.Round((float64(v) - p.Offset) * inv)
		if q < 0 {
			q = 0
		} else if q > 254 {
			q = 254
		}
		out[i] = uint8(q)
	}
}

func DequantizeU8(in []byte, p Params, out []float32) {
	for i, q := range in {
		if q == SentinelU8 {
			out[i] = float32(math.NaN())
			continue
		}
		out[i] = float32(float64(q)*p.Scale + p.Offset)
	}
}

func QuantizeU16(values []float32, p Params, out []byte) {
	if p.Scale == 0 {
		for i, v := range values {
			var q uint16
			if isNaN32(v) {
				q = SentinelU16
			}
			out[2*i] = byte(q)
			out[2*i+1] = byte(q >> 8)
		}
		return
	}
	inv := 1.0 / p.Scale
	for i, v := range values {
		var q uint16
		if isNaN32(v) {
			q = SentinelU16
		} else {
			r := math.Round((float64(v) - p.Offset) * inv)
			if r < 0 {
				r = 0
			} else if r > 65534 {
				r = 65534
			}
			q = uint16(r)
		}
		out[2*i] = byte(q)
		out[2*i+1] = byte(q >> 8)
	}
}

func DequantizeU16(in []byte, p Params, out []float32) {
	n := len(in) / 2
	for i := range n {
		q := uint16(in[2*i]) | uint16(in[2*i+1])<<8
		if q == SentinelU16 {
			out[i] = float32(math.NaN())
			continue
		}
		out[i] = float32(float64(q)*p.Scale + p.Offset)
	}
}

func CanonicalizeF32(values []float32, out []byte) {
	for i, v := range values {
		bits := math.Float32bits(v)
		if isNaN32(v) {
			bits = CanonicalQuietNaN
		}
		out[4*i+0] = byte(bits)
		out[4*i+1] = byte(bits >> 8)
		out[4*i+2] = byte(bits >> 16)
		out[4*i+3] = byte(bits >> 24)
	}
}

func DecodeF32(in []byte, out []float32) {
	n := len(in) / 4
	for i := range n {
		bits := uint32(in[4*i+0]) | uint32(in[4*i+1])<<8 | uint32(in[4*i+2])<<16 | uint32(in[4*i+3])<<24
		out[i] = math.Float32frombits(bits)
	}
}

func Encode(values []float32, p Params, out []byte) {
	switch p.DType {
	case DTypeU8:
		QuantizeU8(values, p, out)
	case DTypeU16:
		QuantizeU16(values, p, out)
	case DTypeF32:
		CanonicalizeF32(values, out)
	}
}

func Decode(in []byte, p Params, out []float32) {
	switch p.DType {
	case DTypeU8:
		DequantizeU8(in, p, out)
	case DTypeU16:
		DequantizeU16(in, p, out)
	case DTypeF32:
		DecodeF32(in, out)
	}
}

// v != v is true only for NaN: avoids the math.IsNaN float64 round-trip
func isNaN32(v float32) bool {
	return v != v
}

func MaxAbsError(p Params) float64 {
	return p.Scale / 2.0
}
