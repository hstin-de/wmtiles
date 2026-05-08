package codec

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/DataDog/zstd"
	"github.com/hstin-de/wmtiles/bitshuffle"
	"github.com/hstin-de/wmtiles/quantize"
)

// codec tag: first byte of every encoded tile blob, dispatches the rest of the payload
const (
	IDReservedZero   byte = 0x00
	IDConstant       byte = 0x01
	IDRawZstd        byte = 0x02
	IDBitshuffleZstd byte = 0x03
	IDDeltaZstd      byte = 0x04
	IDLorenzoZstd    byte = 0x05
)

var ErrUnknownCodec = errors.New("codec: unknown tag")

type Encoder struct {
	zw         zstd.Ctx
	level      int
	scratch    []byte
	scratch2   []byte
	scratch3   []byte
	samplers   map[string]*samplerState
	allowDelta bool
}

func NewEncoder(level int) (*Encoder, error) {
	return NewEncoderWithOpts(level, false)
}

// allowDelta gates the predictor codecs (delta+zstd, lorenzo+zstd) which
// win on smooth fields at ~3× the CPU of bitshuffle alone
func NewEncoderWithOpts(level int, allowDelta bool) (*Encoder, error) {
	if level == 0 {
		level = zstd.DefaultCompression
	}
	return &Encoder{zw: zstd.NewCtx(), level: level, allowDelta: allowDelta}, nil
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
	lorenzoWins    int
	winner         byte
}

func (e *Encoder) Close() error {
	return nil
}

type Decoder struct {
	zr        zstd.Ctx
	scratch   []byte
	tileBytes []byte
}

func NewDecoder() (*Decoder, error) {
	return &Decoder{zr: zstd.NewCtx()}, nil
}

func (d *Decoder) Close() {}

func (d *Decoder) Decode(blob []byte, p quantize.Params, nPixels int, out []byte) error {
	if len(blob) < 1 {
		return errors.New("codec: empty blob")
	}
	switch blob[0] {
	case IDConstant:
		return decodeConstant(blob[1:], p, nPixels, out)
	case IDRawZstd:
		_, err := d.zr.DecompressInto(out, blob[1:])
		return err
	case IDBitshuffleZstd:
		return d.decodeBitshuffleZstd(blob[1:], p, nPixels, out)
	case IDDeltaZstd:
		return d.decodeDeltaZstd(blob[1:], p, nPixels, out)
	case IDLorenzoZstd:
		return d.decodeLorenzoZstd(blob[1:], p, nPixels, out)
	}
	return fmt.Errorf("%w: 0x%02X", ErrUnknownCodec, blob[0])
}

func (d *Decoder) DecodeToFloat32(blob []byte, p quantize.Params, nPixels int, out []float32) error {
	if len(blob) < 1 {
		return errors.New("codec: empty blob")
	}
	if len(out) < nPixels {
		return fmt.Errorf("codec: out has %d, want >=%d", len(out), nPixels)
	}
	stride := p.DType.Bytes()
	need := nPixels * stride
	if cap(d.tileBytes) < need {
		d.tileBytes = make([]byte, need)
	}
	tb := d.tileBytes[:need]
	if err := d.Decode(blob, p, nPixels, tb); err != nil {
		return err
	}
	quantize.Decode(tb, p, out[:nPixels])
	return nil
}

func (d *Decoder) decodeBitshuffleZstd(payload []byte, p quantize.Params, nPixels int, out []byte) error {
	stride := p.DType.Bytes()
	bsLen := bitshuffle.EncodedLen(stride, nPixels)
	if cap(d.scratch) < bsLen {
		d.scratch = make([]byte, bsLen)
	}
	scratch := d.scratch[:bsLen]
	n, err := d.zr.DecompressInto(scratch, payload)
	if err != nil {
		return err
	}
	if n != bsLen {
		return fmt.Errorf("codec: bitshuffle_zstd inner length %d, want %d", n, bsLen)
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
	scratch := d.scratch[:len(out)]
	n, err := d.zr.DecompressInto(scratch, payload)
	if err != nil {
		return err
	}
	if n != len(out) {
		return fmt.Errorf("codec: delta_zstd inner length %d, want %d", n, len(out))
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
			s.lorenzoWins = 0
		}
		switch s.winner {
		case IDDeltaZstd:
			return e.encodeDeltaZstd(quantized, p, nPixels)
		case IDLorenzoZstd:
			if blob := e.encodeLorenzoZstd(quantized, p, nPixels); blob != nil {
				return blob
			}
			return e.encodeBitshuffleZstd(quantized, p, nPixels)
		default:
			return e.encodeBitshuffleZstd(quantized, p, nPixels)
		}
	}

	bs := e.encodeBitshuffleZstd(quantized, p, nPixels)
	dz := e.encodeDeltaZstd(quantized, p, nPixels)
	lz := e.encodeLorenzoZstd(quantized, p, nPixels)

	best := bs
	bestID := IDBitshuffleZstd
	if len(dz) < len(best) {
		best = dz
		bestID = IDDeltaZstd
	}
	if lz != nil && len(lz) < len(best) {
		best = lz
		bestID = IDLorenzoZstd
	}

	switch bestID {
	case IDLorenzoZstd:
		s.lorenzoWins++
	case IDDeltaZstd:
		s.deltaWins++
	default:
		s.bitshuffleWins++
	}
	s.countInPhase++
	if s.countInPhase >= samplerSampleSize {
		s.winner = IDBitshuffleZstd
		bestCount := s.bitshuffleWins
		if s.deltaWins > bestCount {
			s.winner = IDDeltaZstd
			bestCount = s.deltaWins
		}
		if s.lorenzoWins > bestCount {
			s.winner = IDLorenzoZstd
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
	case IDLorenzoZstd:
		if p.DType == quantize.DTypeF32 {
			return nil, errors.New("codec: lorenzo_zstd not allowed for f32")
		}
		blob := e.encodeLorenzoZstd(quantized, p, nPixels)
		if blob == nil {
			return nil, errors.New("codec: lorenzo_zstd refused (non-square tile or unsupported stride)")
		}
		return blob, nil
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

// libzstd's CompressLevel overwrites dst (it's a reusable buffer, not an
// append target), so we can't pre-fill the codec tag — prepend after
func (e *Encoder) compressWithTag(tag byte, src []byte) []byte {
	body, err := e.zw.CompressLevel(nil, src, e.level)
	if err != nil {
		panic(fmt.Sprintf("codec: zstd compress: %v", err))
	}
	out := make([]byte, 1+len(body))
	out[0] = tag
	copy(out[1:], body)
	return out
}

func (e *Encoder) encodeRawZstd(quantized []byte) []byte {
	return e.compressWithTag(IDRawZstd, quantized)
}

func (e *Encoder) encodeBitshuffleZstd(quantized []byte, p quantize.Params, nPixels int) []byte {
	stride := p.DType.Bytes()
	bsLen := bitshuffle.EncodedLen(stride, nPixels)
	if cap(e.scratch) < bsLen {
		e.scratch = make([]byte, bsLen)
	}
	scratch := e.scratch[:bsLen]
	bitshuffle.Encode(quantized, stride, nPixels, scratch)
	return e.compressWithTag(IDBitshuffleZstd, scratch)
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
	return e.compressWithTag(IDDeltaZstd, delta)
}

// returns nil on non-square tiles or stride outside u8/u16
func (e *Encoder) encodeLorenzoZstd(quantized []byte, p quantize.Params, nPixels int) []byte {
	stride := p.DType.Bytes()
	if stride != 1 && stride != 2 {
		return nil
	}
	w := isqrt(nPixels)
	if w*w != nPixels {
		return nil
	}
	if cap(e.scratch3) < len(quantized) {
		e.scratch3 = make([]byte, len(quantized))
	}
	residual := e.scratch3[:len(quantized)]
	lorenzoEncode(quantized, residual, w, stride)
	return e.compressWithTag(IDLorenzoZstd, residual)
}

func (d *Decoder) decodeLorenzoZstd(payload []byte, p quantize.Params, nPixels int, out []byte) error {
	stride := p.DType.Bytes()
	if stride != 1 && stride != 2 {
		return fmt.Errorf("codec: lorenzo_zstd unsupported stride %d", stride)
	}
	w := isqrt(nPixels)
	if w*w != nPixels {
		return errors.New("codec: lorenzo_zstd requires square tile")
	}
	if cap(d.scratch) < len(out) {
		d.scratch = make([]byte, len(out))
	}
	scratch := d.scratch[:len(out)]
	n, err := d.zr.DecompressInto(scratch, payload)
	if err != nil {
		return err
	}
	if n != len(out) {
		return fmt.Errorf("codec: lorenzo_zstd inner length %d, want %d", n, len(out))
	}
	lorenzoDecode(scratch, out, w, stride)
	return nil
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

// 2D residual against q(x-1,y)+q(x,y-1)-q(x-1,y-1); top row falls back to 1D
// x-delta, left column to 1D y-delta. Arithmetic wraps mod 2^(stride*8) so the
// inverse is exact
func lorenzoEncode(src, dst []byte, w, stride int) {
	rowBytes := w * stride
	switch stride {
	case 1:
		// top-left stays as-is
		dst[0] = src[0]
		// top row: 1D x-delta
		for c := 1; c < w; c++ {
			dst[c] = src[c] - src[c-1]
		}
		for r := 1; r < w; r++ {
			base := r * rowBytes
			prevRow := base - rowBytes
			// left column: 1D y-delta
			dst[base] = src[base] - src[prevRow]
			for c := 1; c < w; c++ {
				pred := src[base+c-1] + src[prevRow+c] - src[prevRow+c-1]
				dst[base+c] = src[base+c] - pred
			}
		}
	case 2:
		ld := func(p int) uint16 { return uint16(src[p]) | uint16(src[p+1])<<8 }
		st := func(p int, v uint16) {
			dst[p] = byte(v)
			dst[p+1] = byte(v >> 8)
		}
		st(0, ld(0))
		for c := 1; c < w; c++ {
			st(2*c, ld(2*c)-ld(2*(c-1)))
		}
		for r := 1; r < w; r++ {
			base := r * rowBytes
			prevRow := base - rowBytes
			st(base, ld(base)-ld(prevRow))
			for c := 1; c < w; c++ {
				pred := ld(base+2*(c-1)) + ld(prevRow+2*c) - ld(prevRow+2*(c-1))
				st(base+2*c, ld(base+2*c)-pred)
			}
		}
	}
}

func lorenzoDecode(src, dst []byte, w, stride int) {
	rowBytes := w * stride
	switch stride {
	case 1:
		dst[0] = src[0]
		for c := 1; c < w; c++ {
			dst[c] = src[c] + dst[c-1]
		}
		for r := 1; r < w; r++ {
			base := r * rowBytes
			prevRow := base - rowBytes
			dst[base] = src[base] + dst[prevRow]
			for c := 1; c < w; c++ {
				pred := dst[base+c-1] + dst[prevRow+c] - dst[prevRow+c-1]
				dst[base+c] = src[base+c] + pred
			}
		}
	case 2:
		ldS := func(p int) uint16 { return uint16(src[p]) | uint16(src[p+1])<<8 }
		ldD := func(p int) uint16 { return uint16(dst[p]) | uint16(dst[p+1])<<8 }
		st := func(p int, v uint16) {
			dst[p] = byte(v)
			dst[p+1] = byte(v >> 8)
		}
		st(0, ldS(0))
		for c := 1; c < w; c++ {
			st(2*c, ldS(2*c)+ldD(2*(c-1)))
		}
		for r := 1; r < w; r++ {
			base := r * rowBytes
			prevRow := base - rowBytes
			st(base, ldS(base)+ldD(prevRow))
			for c := 1; c < w; c++ {
				pred := ldD(base+2*(c-1)) + ldD(prevRow+2*c) - ldD(prevRow+2*(c-1))
				st(base+2*c, ldS(base+2*c)+pred)
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
