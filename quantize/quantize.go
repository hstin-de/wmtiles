package quantize

import (
	"math"
	"unsafe"
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
		// honour precision as the actual quantisation step. previous behaviour
		// always picked Scale = rng/MaxQ, giving finer resolution than the
		// caller asked for and forcing every bit-plane to carry signal.
		// using precision as Scale leaves high bit-planes empty whenever the
		// requested resolution is coarser than the dtype's full grid; bitshuffle
		// then collapses those planes and zstd compresses them to almost nothing
		needU8 := math.Ceil(rng/precision) + 1
		if needU8 <= 255 {
			return Params{DType: DTypeU8, Scale: precision, Offset: vmin}
		}
		needU16 := math.Ceil(rng/precision) + 1
		if needU16 <= 65535 {
			return Params{DType: DTypeU16, Scale: precision, Offset: vmin}
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
	// Stay in float32 throughout. The per-pixel float64 promotion was
	// the bulk of the cost and float32 precision dominates the
	// quantization budget anyway.
	inv32 := float32(1.0 / p.Scale)
	off32 := float32(p.Offset)
	for i, v := range values {
		if isNaN32(v) {
			out[i] = SentinelU8
			continue
		}
		// math.Round is banker's-rounding via a function call; for quantization
		// the half-to-even vs half-up distinction is well below 1 LSB and dwarfed
		// by the precision budget, so use the cheap +0.5 trick after clamping
		r := (v - off32) * inv32
		if r <= 0 {
			out[i] = 0
		} else if r >= 254 {
			out[i] = 254
		} else {
			out[i] = uint8(r + 0.5)
		}
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
	n := len(values)
	if len(out) < 2*n {
		return
	}
	// Alias out as []uint16: one store per pixel. On-disk format is LE u16,
	// which matches host endianness on all supported targets.
	dst := (*[1 << 30]uint16)(unsafe.Pointer(unsafe.SliceData(out)))[:n:n]
	if p.Scale == 0 {
		for i, v := range values {
			if isNaN32(v) {
				dst[i] = SentinelU16
			} else {
				dst[i] = 0
			}
		}
		return
	}
	// Same float32-throughout rationale as QuantizeU8.
	inv32 := float32(1.0 / p.Scale)
	off32 := float32(p.Offset)
	for i, v := range values {
		if isNaN32(v) {
			dst[i] = SentinelU16
			continue
		}
		r := (v - off32) * inv32
		switch {
		case r <= 0:
			dst[i] = 0
		case r >= 65534:
			dst[i] = 65534
		default:
			dst[i] = uint16(r + 0.5)
		}
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
