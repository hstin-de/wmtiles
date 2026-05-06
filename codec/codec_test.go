package codec

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/hstin-de/wmtiles/quantize"
	"github.com/klauspost/compress/zstd"
)

func makeEncoder(t *testing.T) *Encoder {
	t.Helper()
	enc, err := NewEncoder(zstd.SpeedDefault)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	t.Cleanup(func() { enc.Close() })
	return enc
}

func makeEncoderWithDelta(t *testing.T) *Encoder {
	t.Helper()
	enc, err := NewEncoderWithOpts(zstd.SpeedDefault, true)
	if err != nil {
		t.Fatalf("NewEncoderWithOpts: %v", err)
	}
	t.Cleanup(func() { enc.Close() })
	return enc
}

func TestConstantTile(t *testing.T) {
	enc := makeEncoder(t)
	const n = 256
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	in := bytes.Repeat([]byte{0x42, 0x00}, n)
	blob := enc.EncodeBest(in, p, n)
	if len(blob) != 5 {
		t.Errorf("constant blob should be 5 bytes, got %d", len(blob))
	}
	if blob[0] != IDConstant {
		t.Errorf("expected IDConstant tag, got 0x%02X", blob[0])
	}
	out := make([]byte, len(in))
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("constant roundtrip mismatch")
	}
}

func TestBitshuffleZstdU16(t *testing.T) {
	enc := makeEncoder(t)
	const n = 256
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 0.01, Offset: 0}
	in := make([]byte, n*2)
	for i := range n {
		v := uint16(40000 + (i*7)%128)
		in[2*i] = byte(v)
		in[2*i+1] = byte(v >> 8)
	}
	blob, err := enc.EncodeWith(IDBitshuffleZstd, in, p, n)
	if err != nil {
		t.Fatal(err)
	}
	if blob[0] != IDBitshuffleZstd {
		t.Errorf("bad tag")
	}
	out := make([]byte, n*2)
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("bitshuffle_zstd roundtrip mismatch")
	}
}

func TestDeltaZstdU8(t *testing.T) {
	enc := makeEncoder(t)
	const w = 16
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU8, Scale: 1, Offset: 0}
	in := make([]byte, n)
	for i := range n {
		in[i] = byte(i % 13)
	}
	blob, err := enc.EncodeWith(IDDeltaZstd, in, p, n)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, n)
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("delta_zstd u8 roundtrip mismatch")
	}
}

func TestDeltaZstdU16Wraparound(t *testing.T) {
	enc := makeEncoder(t)
	const w = 8
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	rng := rand.New(rand.NewPCG(42, 0))
	in := make([]byte, n*2)
	for i := range n {
		v := uint16(rng.UintN(65535))
		in[2*i] = byte(v)
		in[2*i+1] = byte(v >> 8)
	}
	blob, err := enc.EncodeWith(IDDeltaZstd, in, p, n)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, n*2)
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("delta_zstd u16 wraparound roundtrip mismatch")
	}
}

func TestF32RawZstd(t *testing.T) {
	enc := makeEncoder(t)
	const n = 64
	p := quantize.Params{DType: quantize.DTypeF32, Scale: 1, Offset: 0}
	in := make([]byte, n*4)
	for i := range n * 4 {
		in[i] = byte(i)
	}
	blob := enc.EncodeBest(in, p, n)
	out := make([]byte, n*4)
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("f32 best-codec roundtrip mismatch")
	}
}

func TestDeltaRejectsF32(t *testing.T) {
	enc := makeEncoder(t)
	p := quantize.Params{DType: quantize.DTypeF32, Scale: 1, Offset: 0}
	if _, err := enc.EncodeWith(IDDeltaZstd, []byte{1, 2, 3, 4}, p, 1); err == nil {
		t.Errorf("expected error for delta_zstd on f32")
	}
}

func TestEncodeBestPicksConstant(t *testing.T) {
	enc := makeEncoder(t)
	const n = 256
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	in := bytes.Repeat([]byte{1, 0}, n)
	blob := enc.EncodeBest(in, p, n)
	if blob[0] != IDConstant {
		t.Errorf("EncodeBest didn't pick constant for uniform tile")
	}
}

func smoothU16Tile(w int, seed uint16) []byte {
	out := make([]byte, w*w*2)
	for r := range w {
		for c := range w {
			v := seed + uint16(r*8+c)
			out[2*(r*w+c)] = byte(v)
			out[2*(r*w+c)+1] = byte(v >> 8)
		}
	}
	return out
}

func noisyU16Tile(w int, seed uint64) []byte {
	out := make([]byte, w*w*2)
	rng := rand.New(rand.NewPCG(seed, 0xdeadbeef))
	for i := range w * w {
		v := uint16(rng.UintN(65535))
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out
}

func TestEncodeBestSampledRoundtripAcrossPhases(t *testing.T) {
	enc := makeEncoder(t)
	const w = 16
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}

	total := samplerSampleSize + samplerExploitSize + 50
	for i := range total {
		var in []byte
		if i%2 == 0 {
			in = smoothU16Tile(w, uint16(i))
		} else {
			in = noisyU16Tile(w, uint64(i))
		}
		blob := enc.EncodeBestSampled(in, p, n, "var")
		out := make([]byte, len(in))
		if err := Decode(blob, p, n, out); err != nil {
			t.Fatalf("tile %d decode: %v", i, err)
		}
		if !bytes.Equal(in, out) {
			t.Fatalf("tile %d roundtrip mismatch", i)
		}
	}
}

func TestEncodeBestSampledConvergesOnSmoothFields(t *testing.T) {
	enc := makeEncoderWithDelta(t)
	const w = 16
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}

	for i := range samplerSampleSize {
		in := smoothU16Tile(w, uint16(i))
		_ = enc.EncodeBestSampled(in, p, n, "smooth")
	}
	s := enc.samplers["smooth"]
	if s == nil {
		t.Fatal("sampler state missing")
	}
	if s.mode != samplerModeExploit {
		t.Fatalf("expected exploit mode after %d samples, got %v", samplerSampleSize, s.mode)
	}
	if s.winner != IDDeltaZstd && s.winner != IDLorenzoZstd {
		t.Fatalf("expected delta_zstd or lorenzo_zstd winner on smooth field, got 0x%02X (bs=%d, dz=%d, lz=%d)",
			s.winner, s.bitshuffleWins, s.deltaWins, s.lorenzoWins)
	}

	in := smoothU16Tile(w, 0xbeef)
	blob := enc.EncodeBestSampled(in, p, n, "smooth")
	if blob[0] != IDDeltaZstd && blob[0] != IDLorenzoZstd {
		t.Fatalf("exploit-phase blob tag = 0x%02X, want IDDeltaZstd or IDLorenzoZstd", blob[0])
	}
}

func TestEncodeBestSampledResamples(t *testing.T) {
	enc := makeEncoderWithDelta(t)
	const w = 16
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}

	for i := range samplerSampleSize + samplerExploitSize {
		in := smoothU16Tile(w, uint16(i))
		_ = enc.EncodeBestSampled(in, p, n, "k")
	}
	_ = enc.EncodeBestSampled(smoothU16Tile(w, 0), p, n, "k")
	s := enc.samplers["k"]
	if s.mode != samplerModeSample {
		t.Fatalf("expected resample mode after exploit window, got %v (count=%d)", s.mode, s.countInPhase)
	}
	if s.bitshuffleWins+s.deltaWins+s.lorenzoWins == 0 {
		t.Fatalf("resample phase should have recorded the trigger tile")
	}
}

func TestEncodeBestSampledF32SkipsSampler(t *testing.T) {
	enc := makeEncoder(t)
	const n = 64
	p := quantize.Params{DType: quantize.DTypeF32, Scale: 1, Offset: 0}
	in := make([]byte, n*4)
	for i := range n * 4 {
		in[i] = byte(i)
	}
	blob := enc.EncodeBestSampled(in, p, n, "f32var")
	if blob[0] != IDBitshuffleZstd {
		t.Errorf("f32 sampled blob tag = 0x%02X, want IDBitshuffleZstd", blob[0])
	}
	if _, ok := enc.samplers["f32var"]; ok {
		t.Errorf("f32 path should not allocate sampler state")
	}
	out := make([]byte, len(in))
	if err := Decode(blob, p, n, out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("f32 roundtrip mismatch")
	}
}

func BenchmarkEncodeBest(b *testing.B) {
	enc, err := NewEncoder(zstd.SpeedDefault)
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	const w = 256
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	tile := smoothU16Tile(w, 0)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = enc.EncodeBest(tile, p, n)
	}
}

func BenchmarkEncodeBestSampled(b *testing.B) {
	enc, err := NewEncoder(zstd.SpeedDefault)
	if err != nil {
		b.Fatal(err)
	}
	defer enc.Close()
	const w = 256
	const n = w * w
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	tile := smoothU16Tile(w, 0)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = enc.EncodeBestSampled(tile, p, n, "v")
	}
}

func TestEncodeBestSampledConstantBypass(t *testing.T) {
	enc := makeEncoder(t)
	const n = 256
	p := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	in := bytes.Repeat([]byte{0x42, 0x00}, n)
	for range 5 {
		blob := enc.EncodeBestSampled(in, p, n, "const")
		if blob[0] != IDConstant {
			t.Fatalf("constant tile blob tag = 0x%02X, want IDConstant", blob[0])
		}
	}
	if _, ok := enc.samplers["const"]; ok {
		t.Errorf("constant tiles should not consume sampler state")
	}
}
