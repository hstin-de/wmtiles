package codec

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/hstin-de/wmtiles/quantize"
)

func TestLorenzoRoundtripU8(t *testing.T) {
	const w = 64
	rng := rand.New(rand.NewPCG(1, 2))
	src := make([]byte, w*w)
	for i := range src {
		src[i] = byte(rng.IntN(256))
	}
	enc := makeEncoderWithDelta(t)
	p := quantize.Params{DType: quantize.DTypeU8, Scale: 1, Offset: 0}

	blob, err := enc.EncodeWith(IDLorenzoZstd, src, p, w*w)
	if err != nil {
		t.Fatal(err)
	}
	if blob[0] != IDLorenzoZstd {
		t.Fatalf("first byte %02X != IDLorenzoZstd", blob[0])
	}

	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out := make([]byte, w*w)
	if err := dec.Decode(blob, p, w*w, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src, out) {
		t.Fatal("u8 lorenzo roundtrip mismatch")
	}
}

func TestLorenzoRoundtripU16(t *testing.T) {
	const w = 64
	rng := rand.New(rand.NewPCG(3, 4))
	src := make([]byte, w*w*2)
	for i := 0; i < w*w; i++ {
		v := uint16(rng.IntN(65536))
		src[2*i] = byte(v)
		src[2*i+1] = byte(v >> 8)
	}
	enc := makeEncoderWithDelta(t)
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}

	blob, err := enc.EncodeWith(IDLorenzoZstd, src, p, w*w)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out := make([]byte, w*w*2)
	if err := dec.Decode(blob, p, w*w, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src, out) {
		t.Fatal("u16 lorenzo roundtrip mismatch")
	}
}

// On a smooth quadratic field the 2D predictor should beat 1D delta because
// the row-to-row second difference is what cancels in the Lorenzo predictor.
func TestLorenzoBeatsDeltaOnSmoothField(t *testing.T) {
	const w = 256
	src := make([]byte, w*w*2)
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			v := uint16(1000 + x + y + (x*y)/16)
			i := y*w + x
			src[2*i] = byte(v)
			src[2*i+1] = byte(v >> 8)
		}
	}
	enc := makeEncoderWithDelta(t)
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}

	bs := enc.encodeBitshuffleZstd(src, p, w*w)
	dz := enc.encodeDeltaZstd(src, p, w*w)
	lz := enc.encodeLorenzoZstd(src, p, w*w)
	t.Logf("smooth quadratic 256² u16: bs=%d delta=%d lorenzo=%d", len(bs), len(dz), len(lz))
	if len(lz) >= len(dz) {
		t.Errorf("expected lorenzo (%d) < delta (%d) on smooth quadratic", len(lz), len(dz))
	}
}
