package bitshuffle

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func TestRoundtripU8(t *testing.T) {
	cases := []int{8, 16, 32, 256, 1024}
	for _, n := range cases {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i * 7)
		}
		enc := make([]byte, EncodedLen(1, n))
		dec := make([]byte, n)
		Encode(src, 1, n, enc)
		Decode(enc, 1, n, dec)
		if !bytes.Equal(src, dec) {
			t.Errorf("roundtrip u8 n=%d failed", n)
		}
	}
}

func TestRoundtripU16(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	cases := []int{16, 64, 256, 65536}
	for _, n := range cases {
		src := make([]byte, n*2)
		for i := range src {
			src[i] = byte(rng.UintN(256))
		}
		enc := make([]byte, EncodedLen(2, n))
		dec := make([]byte, n*2)
		Encode(src, 2, n, enc)
		Decode(enc, 2, n, dec)
		if !bytes.Equal(src, dec) {
			t.Errorf("roundtrip u16 n=%d failed", n)
		}
	}
}

func TestRoundtripF32(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	const n = 256 * 256
	src := make([]byte, n*4)
	for i := range src {
		src[i] = byte(rng.UintN(256))
	}
	enc := make([]byte, EncodedLen(4, n))
	dec := make([]byte, n*4)
	Encode(src, 4, n, enc)
	Decode(enc, 4, n, dec)
	if !bytes.Equal(src, dec) {
		t.Errorf("roundtrip f32 256x256 failed")
	}
}

func TestSmoothFieldCompresses(t *testing.T) {
	const n = 1024
	src := make([]byte, n*2)
	for i := range n {
		v := uint16(40000 + i%4)
		src[2*i] = byte(v)
		src[2*i+1] = byte(v >> 8)
	}
	enc := make([]byte, EncodedLen(2, n))
	Encode(src, 2, n, enc)
	runs := 0
	i := 0
	for i < len(enc) {
		j := i + 1
		for j < len(enc) && enc[j] == enc[i] {
			j++
		}
		if j-i >= 4 {
			runs++
		}
		i = j
	}
	if runs < 4 {
		t.Errorf("smooth field bitshuffle should produce many runs, got %d", runs)
	}
}
