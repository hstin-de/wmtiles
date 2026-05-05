package varint

import (
	"bytes"
	"math"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 16383, 16384, 1<<21 - 1, 1 << 21,
		1<<28 - 1, 1 << 28, 1<<35 - 1, 1 << 35, math.MaxUint32, math.MaxUint64}
	for _, v := range cases {
		buf := Append(nil, v)
		if got := EncodedLen(v); got != len(buf) {
			t.Errorf("EncodedLen(%d)=%d, encoded as %d bytes", v, got, len(buf))
		}
		got, n, err := Read(buf)
		if err != nil {
			t.Errorf("Read(%d) error: %v", v, err)
			continue
		}
		if got != v {
			t.Errorf("Read: got %d, want %d", got, v)
		}
		if n != len(buf) {
			t.Errorf("Read consumed %d, want %d", n, len(buf))
		}
	}
}

func TestEncodingLengths(t *testing.T) {
	cases := []struct {
		v    uint64
		want int
	}{
		{0, 1},
		{127, 1},
		{128, 2},
		{16383, 2},
		{16384, 3},
		{1<<21 - 1, 3},
		{1 << 21, 4},
		{1<<28 - 1, 4},
		{1 << 28, 5},
	}
	for _, c := range cases {
		if got := EncodedLen(c.v); got != c.want {
			t.Errorf("EncodedLen(%d)=%d, want %d", c.v, got, c.want)
		}
	}
}

func TestReadFrom(t *testing.T) {
	buf := Append(nil, 12345)
	r := bytes.NewReader(buf)
	v, err := ReadFrom(r)
	if err != nil || v != 12345 {
		t.Errorf("ReadFrom: %v %v", v, err)
	}
}

func TestTruncated(t *testing.T) {
	if _, _, err := Read([]byte{0x80}); err != ErrTruncated {
		t.Errorf("expected ErrTruncated, got %v", err)
	}
}

func TestOverflow(t *testing.T) {
	bad := bytes.Repeat([]byte{0xff}, MaxLen+1)
	if _, _, err := Read(bad); err != ErrOverflow {
		t.Errorf("expected ErrOverflow, got %v", err)
	}
}
