package quantize

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestU16RoundtripError(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	const N = 1024
	values := make([]float32, N)
	for i := range values {
		values[i] = float32(200 + rng.Float64()*130)
	}
	p := FitParams(200, 330, 0)
	if p.DType != DTypeU16 {
		t.Fatalf("expected u16, got %d", p.DType)
	}

	buf := make([]byte, N*p.DType.Bytes())
	out := make([]float32, N)
	Encode(values, p, buf)
	Decode(buf, p, out)

	maxErr := 0.0
	for i, v := range values {
		e := math.Abs(float64(v - out[i]))
		if e > maxErr {
			maxErr = e
		}
	}
	tol := MaxAbsError(p) * 1.0001
	if maxErr > tol {
		t.Errorf("max error %g > tol %g", maxErr, tol)
	}
}

func TestU8FitWithPrecision(t *testing.T) {
	p := FitParams(0, 100, 1.0)
	if p.DType != DTypeU8 {
		t.Errorf("expected u8, got %d", p.DType)
	}
}

func TestU16FitWithPrecision(t *testing.T) {
	p := FitParams(0, 1000, 0.1)
	if p.DType != DTypeU16 {
		t.Errorf("expected u16, got %d", p.DType)
	}
}

func TestF32FallbackTooMuchPrecision(t *testing.T) {
	p := FitParams(0, 1, 1e-7)
	if p.DType != DTypeF32 {
		t.Errorf("expected f32 fallback, got %d", p.DType)
	}
}

func TestNaNCanonical(t *testing.T) {
	weirdNaNs := []float32{
		float32(math.NaN()),
		math.Float32frombits(0x7F800001),
		math.Float32frombits(0xFFC00000),
		math.Float32frombits(0x7FAAAAAA),
	}
	p := Params{DType: DTypeF32, Scale: 1, Offset: 0}
	buf := make([]byte, 4)
	for _, v := range weirdNaNs {
		CanonicalizeF32([]float32{v}, buf)
		bits := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
		if bits != CanonicalQuietNaN {
			t.Errorf("NaN %x → %x, want %x", math.Float32bits(v), bits, CanonicalQuietNaN)
		}
	}
	out := make([]float32, 1)
	Decode(buf, p, out)
	if !math.IsNaN(float64(out[0])) {
		t.Errorf("decoded canonical NaN was not NaN")
	}
}

func TestU16NaNSentinel(t *testing.T) {
	p := FitParams(0, 100, 0)
	values := []float32{50, float32(math.NaN()), 0, 100}
	buf := make([]byte, len(values)*2)
	out := make([]float32, len(values))
	Encode(values, p, buf)
	Decode(buf, p, out)
	if !math.IsNaN(float64(out[1])) {
		t.Errorf("NaN didn't round-trip via sentinel")
	}
	if math.Abs(float64(out[0]-50)) > MaxAbsError(p) {
		t.Errorf("regular value error too high")
	}
}

func TestClamp(t *testing.T) {
	p := FitParams(0, 10, 0)
	values := []float32{-5, 15}
	buf := make([]byte, 4)
	out := make([]float32, 2)
	Encode(values, p, buf)
	Decode(buf, p, out)
	if math.Abs(float64(out[0])) > MaxAbsError(p) {
		t.Errorf("expected clamp to vmin, got %f", out[0])
	}
	if math.Abs(float64(out[1])-10) > MaxAbsError(p) {
		t.Errorf("expected clamp to vmax, got %f", out[1])
	}
}

func TestConstantRange(t *testing.T) {
	p := FitParams(42, 42, 0)
	if p.Scale != 0 || p.Offset != 42 {
		t.Errorf("constant range should give scale=0, offset=vmin: %+v", p)
	}
}
