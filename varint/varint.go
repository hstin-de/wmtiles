package varint

import (
	"errors"
	"io"
)

var (
	ErrOverflow  = errors.New("varint: overflow")
	ErrTruncated = errors.New("varint: truncated input")
)

// LEB128 of a uint64 fits in at most 10 bytes (9 full continuation bytes + 1 byte for the top bit)
const MaxLen = 10

func Append(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func EncodedLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func Read(b []byte) (uint64, int, error) {
	var v uint64
	var shift uint
	for i, c := range b {
		if i >= MaxLen {
			return 0, 0, ErrOverflow
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, ErrTruncated
}

func ReadFrom(r io.ByteReader) (uint64, error) {
	var v uint64
	var shift uint
	for i := range MaxLen {
		c, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && i > 0 {
				return 0, ErrTruncated
			}
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
	return 0, ErrOverflow
}
