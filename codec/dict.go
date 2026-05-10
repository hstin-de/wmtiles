package codec

import (
	"errors"
	"fmt"

	"github.com/DataDog/zstd"
	"github.com/hstin-de/wmtiles/bitshuffle"
	"github.com/hstin-de/wmtiles/quantize"
)

// Dict wraps a per-block zstd dictionary. Bytes is the raw dict (trained ZDICT
// or raw-content prefix); Bp is the digested compress/decompress processor.
type Dict struct {
	Bytes []byte
	Bp    *zstd.BulkProcessor
}

// NewDict digests raw into a Dict. raw may be a trained ZDICT (magic
// 0xEC30A437) or arbitrary content; libzstd's ZSTD_dct_auto picks the format.
func NewDict(raw []byte, level int) (*Dict, error) {
	if len(raw) == 0 {
		return nil, errors.New("codec: empty dict")
	}
	if level == 0 {
		level = zstd.DefaultCompression
	}
	bp, err := zstd.NewBulkProcessor(raw, level)
	if err != nil {
		return nil, fmt.Errorf("codec: dict bulk processor: %w", err)
	}
	return &Dict{Bytes: raw, Bp: bp}, nil
}

// DecodeWithDict mirrors Decoder.Decode but routes inner zstd decompression
// through dict. Constant-coded tiles ignore the dict.
func (d *Decoder) DecodeWithDict(blob []byte, p quantize.Params, nPixels int, dict *Dict, out []byte) error {
	if dict == nil {
		return d.Decode(blob, p, nPixels, out)
	}
	if len(blob) < 1 {
		return errors.New("codec: empty blob")
	}
	switch blob[0] {
	case IDConstant:
		return decodeConstant(blob[1:], p, nPixels, out)
	case IDRawZstd:
		raw, err := dict.Bp.Decompress(nil, blob[1:])
		if err != nil {
			return err
		}
		if len(raw) != len(out) {
			return fmt.Errorf("codec: raw_zstd dict-decoded length %d, want %d", len(raw), len(out))
		}
		copy(out, raw)
		return nil
	case IDBitshuffleZstd:
		stride := p.DType.Bytes()
		bsLen := bitshuffle.EncodedLen(stride, nPixels)
		raw, err := dict.Bp.Decompress(nil, blob[1:])
		if err != nil {
			return err
		}
		if len(raw) != bsLen {
			return fmt.Errorf("codec: bitshuffle_zstd dict-decoded length %d, want %d", len(raw), bsLen)
		}
		bitshuffle.Decode(raw, stride, nPixels, out)
		return nil
	case IDDeltaZstd:
		stride := p.DType.Bytes()
		w := isqrt(nPixels)
		if w*w != nPixels {
			return errors.New("codec: delta_zstd requires square tile")
		}
		raw, err := dict.Bp.Decompress(nil, blob[1:])
		if err != nil {
			return err
		}
		if len(raw) != len(out) {
			return fmt.Errorf("codec: delta_zstd dict-decoded length %d, want %d", len(raw), len(out))
		}
		deltaDecode(raw, out, w, stride)
		return nil
	case IDLorenzoZstd:
		stride := p.DType.Bytes()
		if stride != 1 && stride != 2 {
			return fmt.Errorf("codec: lorenzo_zstd unsupported stride %d", stride)
		}
		w := isqrt(nPixels)
		if w*w != nPixels {
			return errors.New("codec: lorenzo_zstd requires square tile")
		}
		raw, err := dict.Bp.Decompress(nil, blob[1:])
		if err != nil {
			return err
		}
		if len(raw) != len(out) {
			return fmt.Errorf("codec: lorenzo_zstd dict-decoded length %d, want %d", len(raw), len(out))
		}
		lorenzoDecode(raw, out, w, stride)
		return nil
	}
	return fmt.Errorf("%w: 0x%02X", ErrUnknownCodec, blob[0])
}

// DecodeToFloat32WithDict is the dict-aware variant of DecodeToFloat32.
func (d *Decoder) DecodeToFloat32WithDict(blob []byte, p quantize.Params, nPixels int, dict *Dict, out []float32) error {
	if dict == nil {
		return d.DecodeToFloat32(blob, p, nPixels, out)
	}
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
	if err := d.DecodeWithDict(blob, p, nPixels, dict, tb); err != nil {
		return err
	}
	quantize.Decode(tb, p, out[:nPixels])
	return nil
}

// ExtractInnerBytes peels the codec tag and returns the post-transform
// pre-zstd inner bytes. Constant tiles return (nil, nil). This unwraps only
// the zstd layer — bitshuffle/delta/lorenzo transforms remain applied.
func ExtractInnerBytes(blob []byte, p quantize.Params, nPixels int, zr zstd.Ctx) (tag byte, inner []byte, err error) {
	if len(blob) < 1 {
		return 0, nil, errors.New("codec: empty blob")
	}
	tag = blob[0]
	stride := p.DType.Bytes()
	switch tag {
	case IDConstant:
		return tag, nil, nil
	case IDRawZstd, IDDeltaZstd, IDLorenzoZstd:
		expected := nPixels * stride
		out := make([]byte, expected)
		n, err := zr.DecompressInto(out, blob[1:])
		if err != nil {
			return tag, nil, err
		}
		if n != expected {
			return tag, nil, fmt.Errorf("codec: %02X inner length %d, want %d", tag, n, expected)
		}
		return tag, out, nil
	case IDBitshuffleZstd:
		expected := bitshuffle.EncodedLen(stride, nPixels)
		out := make([]byte, expected)
		n, err := zr.DecompressInto(out, blob[1:])
		if err != nil {
			return tag, nil, err
		}
		if n != expected {
			return tag, nil, fmt.Errorf("codec: bitshuffle_zstd inner length %d, want %d", n, expected)
		}
		return tag, out, nil
	}
	return tag, nil, fmt.Errorf("%w: 0x%02X", ErrUnknownCodec, tag)
}

// RepackWithDict zstd-compresses inner against dict and returns tag + payload.
func RepackWithDict(tag byte, inner []byte, dict *Dict) ([]byte, error) {
	if dict == nil || dict.Bp == nil {
		return nil, errors.New("codec: nil dict")
	}
	body, err := dict.Bp.Compress(nil, inner)
	if err != nil {
		return nil, fmt.Errorf("codec: dict compress: %w", err)
	}
	out := make([]byte, 1+len(body))
	out[0] = tag
	copy(out[1:], body)
	return out, nil
}

// EncodeInnerOnly returns (IDConstant, value) for constant tiles or
// (IDReservedZero, copy(quant)) for the rest, deferring transform + zstd to
// the per-block dict pass. Returned bytes are a fresh allocation, safe to hand
// across goroutines.
func (e *Encoder) EncodeInnerOnly(quant []byte, p quantize.Params, nPixels int) (byte, []byte) {
	stride := p.DType.Bytes()
	if isConstant(quant, stride) {
		val := make([]byte, stride)
		copy(val, quant[:stride])
		return IDConstant, val
	}
	out := make([]byte, len(quant))
	copy(out, quant)
	return IDReservedZero, out
}

// ApplyTransform writes the bitshuffle/delta/lorenzo/raw transform of quant
// into dst. dst must be sized via TransformedLen.
func ApplyTransform(tag byte, quant []byte, p quantize.Params, nPixels int, dst []byte) error {
	stride := p.DType.Bytes()
	switch tag {
	case IDRawZstd:
		copy(dst, quant)
		return nil
	case IDBitshuffleZstd:
		bitshuffle.Encode(quant, stride, nPixels, dst)
		return nil
	case IDDeltaZstd:
		w := isqrt(nPixels)
		if w*w != nPixels {
			return errors.New("codec: delta requires square tile")
		}
		deltaEncode(quant, dst, w, stride)
		return nil
	case IDLorenzoZstd:
		if stride != 1 && stride != 2 {
			return fmt.Errorf("codec: lorenzo unsupported stride %d", stride)
		}
		w := isqrt(nPixels)
		if w*w != nPixels {
			return errors.New("codec: lorenzo requires square tile")
		}
		lorenzoEncode(quant, dst, w, stride)
		return nil
	}
	return fmt.Errorf("codec: ApplyTransform: unknown tag 0x%02X", tag)
}

// TransformedLen returns the byte count ApplyTransform writes for tag.
func TransformedLen(tag byte, p quantize.Params, nPixels int) int {
	stride := p.DType.Bytes()
	if tag == IDBitshuffleZstd {
		return bitshuffle.EncodedLen(stride, nPixels)
	}
	return nPixels * stride
}

// PackConstantBlob materialises a 5-byte IDConstant blob from a value.
func PackConstantBlob(val []byte) []byte {
	out := make([]byte, 5)
	out[0] = IDConstant
	copy(out[1:], val)
	return out
}
