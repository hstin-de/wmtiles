package codec

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/hstin-de/wmtiles/bitshuffle"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/klauspost/compress/zstd"
)

// codec tag: first byte of every encoded tile blob, dispatches the rest of the payload
const (
	IDReservedZero   byte = 0x00
	IDConstant       byte = 0x01
	IDRawZstd        byte = 0x02
	IDBitshuffleZstd byte = 0x03
	IDDeltaZstd      byte = 0x04
)

var ErrUnknownCodec = errors.New("codec: unknown tag")

type Encoder struct {
	zw         *zstd.Encoder
	scratch    []byte
	scratch2   []byte
	samplers   map[string]*samplerState
	allowDelta bool
}

func NewEncoder(level zstd.EncoderLevel) (*Encoder, error) {
	return NewEncoderWithOpts(level, false)
}

// NewEncoderWithOpts builds a per-worker encoder. allowDelta enables the
// bitshuffle-vs-delta sampler; when false (the recommended default for speed),
// EncodeBestSampled commits unconditionally to bitshuffle+zstd. delta tends to
// compress smooth fields slightly better but is ~3× more CPU at this scale,
// since zstd handles delta-preprocessed bytes worse than bit-plane-shuffled ones
func NewEncoderWithOpts(level zstd.EncoderLevel, allowDelta bool) (*Encoder, error) {
	// concurrency=1: each Encoder is owned by a single goroutine so EncodeAll reuses scratch state
	zw, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &Encoder{zw: zw, allowDelta: allowDelta}, nil
}

// per-block bandit: try both bitshuffle and delta for the first samplerSampleSize tiles,
// then commit to the cheaper one for the next samplerExploitSize, then re-sample.
//
// the sampler is per-(Encoder, variable). at scale (e.g. full GFS, 696 vars × 24
// workers = 16k samplers), a sample size of 100 means ~1.6M sample slots, which
// often exceeds the total tile count — every tile ends up in sample mode, paying
// for both encoders. small sample is enough to pick a winner; exploit dominates
// total work for any non-trivial encode
const (
	samplerSampleSize  = 4
	samplerExploitSize = 1000
)

type samplerMode uint8

const (
	samplerModeSample samplerMode = iota
	samplerModeExploit
)

type samplerState struct {
	mode           samplerMode
	countInPhase   int
	bitshuffleWins int
	deltaWins      int
	winner         byte
}

func (e *Encoder) Close() error {
	return e.zw.Close()
}

type Decoder struct {
	zr      *zstd.Decoder
	scratch []byte
}

func NewDecoder() (*Decoder, error) {
	zr, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &Decoder{zr: zr}, nil
}

func (d *Decoder) Close() {
	if d.zr != nil {
		d.zr.Close()
		d.zr = nil
	}
}

func (d *Decoder) Decode(blob []byte, p quantize.Params, nPixels int, out []byte) error {
	if len(blob) < 1 {
		return errors.New("codec: empty blob")
	}
	switch blob[0] {
	case IDConstant:
		return decodeConstant(blob[1:], p, nPixels, out)
	case IDRawZstd:
		_, err := d.zr.DecodeAll(blob[1:], out[:0])
		if err != nil {
			return err
		}
		return nil
	case IDBitshuffleZstd:
		return d.decodeBitshuffleZstd(blob[1:], p, nPixels, out)
	case IDDeltaZstd:
		return d.decodeDeltaZstd(blob[1:], p, nPixels, out)
	}
	return fmt.Errorf("%w: 0x%02X", ErrUnknownCodec, blob[0])
}

func (d *Decoder) decodeBitshuffleZstd(payload []byte, p quantize.Params, nPixels int, out []byte) error {
	stride := p.DType.Bytes()
	bsLen := bitshuffle.EncodedLen(stride, nPixels)
	if cap(d.scratch) < bsLen {
		d.scratch = make([]byte, bsLen)
	}
	scratch := d.scratch[:0]
	scratch, err := d.zr.DecodeAll(payload, scratch)
	if err != nil {
		return err
	}
	if len(scratch) != bsLen {
		return fmt.Errorf("codec: bitshuffle_zstd inner length %d, want %d", len(scratch), bsLen)
	}
	bitshuffle.Decode(scratch, stride, nPixels, out)
	return nil
}

func (d *Decoder) decodeDeltaZstd(payload []byte, p quantize.Params, nPixels int, out []byte) error {
	stride := p.DType.Bytes()
	w := isqrt(nPixels)
	if w*w != nPixels {
		return errors.New("codec: delta_zstd requires square tile")
	}
	if cap(d.scratch) < len(out) {
		d.scratch = make([]byte, len(out))
	}
	scratch := d.scratch[:0]
	scratch, err := d.zr.DecodeAll(payload, scratch)
	if err != nil {
		return err
	}
	if len(scratch) != len(out) {
		return fmt.Errorf("codec: delta_zstd inner length %d, want %d", len(scratch), len(out))
	}
	deltaDecode(scratch, out, w, stride)
	return nil
}

func (e *Encoder) EncodeBest(quantized []byte, p quantize.Params, nPixels int) []byte {
	if isConstant(quantized, p.DType.Bytes()) {
		return encodeConstant(quantized[:p.DType.Bytes()])
	}

	best := e.encodeBitshuffleZstd(quantized, p, nPixels)
	if p.DType != quantize.DTypeF32 {
		alt := e.encodeDeltaZstd(quantized, p, nPixels)
		if len(alt) < len(best) {
			best = alt
		}
	}
	return best
}

func (e *Encoder) EncodeBestSampled(quantized []byte, p quantize.Params, nPixels int, key string) []byte {
	if isConstant(quantized, p.DType.Bytes()) {
		return encodeConstant(quantized[:p.DType.Bytes()])
	}
	if p.DType == quantize.DTypeF32 || !e.allowDelta {
		return e.encodeBitshuffleZstd(quantized, p, nPixels)
	}

	if e.samplers == nil {
		e.samplers = make(map[string]*samplerState)
	}
	s, ok := e.samplers[key]
	if !ok {
		s = &samplerState{mode: samplerModeSample, winner: IDBitshuffleZstd}
		e.samplers[key] = s
	}

	if s.mode == samplerModeExploit {
		s.countInPhase++
		if s.countInPhase >= samplerExploitSize {
			s.mode = samplerModeSample
			s.countInPhase = 0
			s.bitshuffleWins = 0
			s.deltaWins = 0
		}
		if s.winner == IDDeltaZstd {
			return e.encodeDeltaZstd(quantized, p, nPixels)
		}
		return e.encodeBitshuffleZstd(quantized, p, nPixels)
	}

	bs := e.encodeBitshuffleZstd(quantized, p, nPixels)
	dz := e.encodeDeltaZstd(quantized, p, nPixels)
	var best []byte
	if len(dz) < len(bs) {
		best = dz
		s.deltaWins++
	} else {
		best = bs
		s.bitshuffleWins++
	}
	s.countInPhase++
	if s.countInPhase >= samplerSampleSize {
		if s.deltaWins > s.bitshuffleWins {
			s.winner = IDDeltaZstd
		} else {
			s.winner = IDBitshuffleZstd
		}
		s.mode = samplerModeExploit
		s.countInPhase = 0
	}
	return best
}

func (e *Encoder) EncodeWith(id byte, quantized []byte, p quantize.Params, nPixels int) ([]byte, error) {
	switch id {
	case IDConstant:
		if !isConstant(quantized, p.DType.Bytes()) {
			return nil, errors.New("codec: tile is not constant")
		}
		return encodeConstant(quantized[:p.DType.Bytes()]), nil
	case IDRawZstd:
		return e.encodeRawZstd(quantized), nil
	case IDBitshuffleZstd:
		return e.encodeBitshuffleZstd(quantized, p, nPixels), nil
	case IDDeltaZstd:
		if p.DType == quantize.DTypeF32 {
			return nil, errors.New("codec: delta_zstd not allowed for f32")
		}
		return e.encodeDeltaZstd(quantized, p, nPixels), nil
	}
	return nil, fmt.Errorf("codec: unknown tag 0x%02X", id)
}

func isConstant(b []byte, stride int) bool {
	if len(b) <= stride {
		return true
	}
	for i := stride; i < len(b); i += stride {
		for j := range stride {
			if b[i+j] != b[j] {
				return false
			}
		}
	}
	return true
}

func encodeConstant(value []byte) []byte {
	out := make([]byte, 5)
	out[0] = IDConstant
	copy(out[1:], value)
	return out
}

func decodeConstant(payload []byte, p quantize.Params, nPixels int, out []byte) error {
	stride := p.DType.Bytes()
	if len(payload) < 4 {
		return errors.New("codec: constant payload too short")
	}
	val := payload[:stride]
	for i := range nPixels {
		copy(out[i*stride:], val)
	}
	return nil
}

func (e *Encoder) encodeRawZstd(quantized []byte) []byte {
	out := make([]byte, 1, 1+len(quantized)/2)
	out[0] = IDRawZstd
	out = e.zw.EncodeAll(quantized, out)
	return out
}

func (e *Encoder) encodeBitshuffleZstd(quantized []byte, p quantize.Params, nPixels int) []byte {
	stride := p.DType.Bytes()
	bsLen := bitshuffle.EncodedLen(stride, nPixels)
	if cap(e.scratch) < bsLen {
		e.scratch = make([]byte, bsLen)
	}
	scratch := e.scratch[:bsLen]
	bitshuffle.Encode(quantized, stride, nPixels, scratch)
	out := make([]byte, 1, 1+bsLen/2)
	out[0] = IDBitshuffleZstd
	out = e.zw.EncodeAll(scratch, out)
	return out
}

func (e *Encoder) encodeDeltaZstd(quantized []byte, p quantize.Params, nPixels int) []byte {
	stride := p.DType.Bytes()
	w := isqrt(nPixels)
	if w*w != nPixels {
		return e.encodeRawZstd(quantized)
	}
	if cap(e.scratch2) < len(quantized) {
		e.scratch2 = make([]byte, len(quantized))
	}
	delta := e.scratch2[:len(quantized)]
	deltaEncode(quantized, delta, w, stride)
	out := make([]byte, 1, 1+len(delta)/2)
	out[0] = IDDeltaZstd
	out = e.zw.EncodeAll(delta, out)
	return out
}

func deltaEncode(src, dst []byte, w, stride int) {
	rowBytes := w * stride
	copy(dst[:rowBytes], src[:rowBytes])
	for r := 1; r < w; r++ {
		base := r * rowBytes
		switch stride {
		case 1:
			for c := range w {
				dst[base+c] = src[base+c] - src[base-rowBytes+c]
			}
		case 2:
			for c := range w {
				cur := uint16(src[base+2*c]) | uint16(src[base+2*c+1])<<8
				prev := uint16(src[base-rowBytes+2*c]) | uint16(src[base-rowBytes+2*c+1])<<8
				d := cur - prev
				dst[base+2*c] = byte(d)
				dst[base+2*c+1] = byte(d >> 8)
			}
		}
	}
}

func deltaDecode(src, dst []byte, w, stride int) {
	rowBytes := w * stride
	copy(dst[:rowBytes], src[:rowBytes])
	for r := 1; r < w; r++ {
		base := r * rowBytes
		switch stride {
		case 1:
			for c := range w {
				dst[base+c] = src[base+c] + dst[base-rowBytes+c]
			}
		case 2:
			for c := range w {
				d := uint16(src[base+2*c]) | uint16(src[base+2*c+1])<<8
				prev := uint16(dst[base-rowBytes+2*c]) | uint16(dst[base-rowBytes+2*c+1])<<8
				cur := prev + d
				dst[base+2*c] = byte(cur)
				dst[base+2*c+1] = byte(cur >> 8)
			}
		}
	}
}

func Decode(blob []byte, p quantize.Params, nPixels int, out []byte) error {
	d, err := NewDecoder()
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Decode(blob, p, nPixels, out)
}

func isqrt(n int) int {
	if n < 0 {
		return 0
	}
	x := int(0)
	for x*x <= n {
		x++
	}
	return x - 1
}

func LittleEndianUint32(b []byte) uint32 {
	return binary.LittleEndian.Uint32(b)
}
