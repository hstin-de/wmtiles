import { decompress as zstdDecompress } from "fzstd";
import {
  DTYPE_F32,
  DTYPE_U16,
  DTYPE_U8,
  SENTINEL_U16,
  SENTINEL_U8,
  type BlockTableEntry,
} from "./format.js";
import { FormatError } from "./errors.js";

export const CODEC_CONSTANT = 0x01;
export const CODEC_RAW_ZSTD = 0x02;
export const CODEC_BITSHUFFLE_ZSTD = 0x03;
export const CODEC_DELTA_ZSTD = 0x04;
export const CODEC_LORENZO_ZSTD = 0x05;

export function dtypeBytes(d: number): number {
  if (d === DTYPE_U8) return 1;
  if (d === DTYPE_U16) return 2;
  if (d === DTYPE_F32) return 4;
  return 0;
}

function zstdPayload(payload: Uint8Array, codec: string): Uint8Array {
  try {
    return zstdDecompress(payload);
  } catch (err) {
    throw new FormatError(`${codec}: zstd decompression failed`, {
      cause: err,
    });
  }
}

// straight one-bit-at-a-time port: no 8x8 fast path. Tile sizes are small (≤1MB)
// and this runs once per tile fetch, so the simpler form is fine
function bitshuffleDecode(
  src: Uint8Array,
  elemSize: number,
  elemCount: number,
): Uint8Array {
  const bytesPerPlane = (elemCount + 7) >> 3;
  const totalBits = elemSize * 8;
  const dst = new Uint8Array(elemSize * elemCount);
  for (let bitIndex = 0; bitIndex < totalBits; bitIndex++) {
    const b = bitIndex >> 3;
    const k = bitIndex & 7;
    const planeBase = bitIndex * bytesPerPlane;
    const dstBitMask = 1 << k;
    for (let elem = 0; elem < elemCount; elem++) {
      const byteOff = elem >> 3;
      const bitOff = 7 - (elem & 7);
      if ((src[planeBase + byteOff] >> bitOff) & 1) {
        dst[elem * elemSize + b] |= dstBitMask;
      }
    }
  }
  return dst;
}

function deltaDecode(src: Uint8Array, w: number, stride: number): Uint8Array {
  const rowBytes = w * stride;
  const dst = new Uint8Array(src.length);
  dst.set(src.subarray(0, rowBytes));
  if (stride === 1) {
    for (let r = 1; r < w; r++) {
      const base = r * rowBytes;
      for (let c = 0; c < w; c++) {
        dst[base + c] = (src[base + c] + dst[base - rowBytes + c]) & 0xff;
      }
    }
  } else if (stride === 2) {
    for (let r = 1; r < w; r++) {
      const base = r * rowBytes;
      for (let c = 0; c < w; c++) {
        const d = src[base + 2 * c] | (src[base + 2 * c + 1] << 8);
        const prev =
          dst[base - rowBytes + 2 * c] |
          (dst[base - rowBytes + 2 * c + 1] << 8);
        const cur = (prev + d) & 0xffff;
        dst[base + 2 * c] = cur & 0xff;
        dst[base + 2 * c + 1] = (cur >> 8) & 0xff;
      }
    }
  } else {
    throw new FormatError(`delta_zstd: unsupported stride ${stride}`);
  }
  return dst;
}

function lorenzoDecode(src: Uint8Array, w: number, stride: number): Uint8Array {
  const rowBytes = w * stride;
  const dst = new Uint8Array(src.length);
  if (stride === 1) {
    dst[0] = src[0];
    for (let c = 1; c < w; c++) {
      dst[c] = (src[c] + dst[c - 1]) & 0xff;
    }
    for (let r = 1; r < w; r++) {
      const base = r * rowBytes;
      const prevRow = base - rowBytes;
      dst[base] = (src[base] + dst[prevRow]) & 0xff;
      for (let c = 1; c < w; c++) {
        const pred =
          dst[base + c - 1] + dst[prevRow + c] - dst[prevRow + c - 1];
        dst[base + c] = (src[base + c] + pred) & 0xff;
      }
    }
  } else if (stride === 2) {
    const ldD = (p: number) => dst[p] | (dst[p + 1] << 8);
    const stD = (p: number, v: number) => {
      dst[p] = v & 0xff;
      dst[p + 1] = (v >> 8) & 0xff;
    };
    const ldS = (p: number) => src[p] | (src[p + 1] << 8);
    stD(0, ldS(0));
    for (let c = 1; c < w; c++) {
      stD(2 * c, (ldS(2 * c) + ldD(2 * (c - 1))) & 0xffff);
    }
    for (let r = 1; r < w; r++) {
      const base = r * rowBytes;
      const prevRow = base - rowBytes;
      stD(base, (ldS(base) + ldD(prevRow)) & 0xffff);
      for (let c = 1; c < w; c++) {
        const pred =
          ldD(base + 2 * (c - 1)) +
          ldD(prevRow + 2 * c) -
          ldD(prevRow + 2 * (c - 1));
        stD(base + 2 * c, (ldS(base + 2 * c) + pred) & 0xffff);
      }
    }
  } else {
    throw new FormatError(`lorenzo_zstd: unsupported stride ${stride}`);
  }
  return dst;
}

export function decodeCodec(
  blob: Uint8Array,
  dtype: number,
  nPixels: number,
): Uint8Array {
  if (blob.length < 1) throw new FormatError("codec: empty blob");
  const tag = blob[0];
  const stride = dtypeBytes(dtype);
  const total = stride * nPixels;
  const payload = blob.subarray(1);

  switch (tag) {
    case CODEC_CONSTANT: {
      const out = new Uint8Array(total);
      for (let i = 0; i < nPixels; i++) {
        for (let j = 0; j < stride; j++) out[i * stride + j] = payload[j];
      }
      return out;
    }
    case CODEC_RAW_ZSTD: {
      const out = zstdPayload(payload, "raw_zstd");
      if (out.length !== total) {
        throw new FormatError(`raw_zstd: got ${out.length}, want ${total}`);
      }
      return out;
    }
    case CODEC_BITSHUFFLE_ZSTD: {
      const inner = zstdPayload(payload, "bitshuffle_zstd");
      const expectedInner = 8 * stride * ((nPixels + 7) >> 3);
      if (inner.length !== expectedInner) {
        throw new FormatError(
          `bitshuffle inner len ${inner.length}, want ${expectedInner}`,
        );
      }
      return bitshuffleDecode(inner, stride, nPixels);
    }
    case CODEC_DELTA_ZSTD: {
      const inner = zstdPayload(payload, "delta_zstd");
      if (inner.length !== total) {
        throw new FormatError(
          `delta_zstd inner len ${inner.length}, want ${total}`,
        );
      }
      const w = Math.round(Math.sqrt(nPixels));
      if (w * w !== nPixels) {
        throw new FormatError("delta_zstd requires square tile");
      }
      return deltaDecode(inner, w, stride);
    }
    case CODEC_LORENZO_ZSTD: {
      const inner = zstdPayload(payload, "lorenzo_zstd");
      if (inner.length !== total) {
        throw new FormatError(
          `lorenzo_zstd inner len ${inner.length}, want ${total}`,
        );
      }
      const w = Math.round(Math.sqrt(nPixels));
      if (w * w !== nPixels) {
        throw new FormatError("lorenzo_zstd requires square tile");
      }
      return lorenzoDecode(inner, w, stride);
    }
    default:
      throw new FormatError(`unknown codec 0x${tag.toString(16)}`);
  }
}

export function dequantize(
  bytes: Uint8Array,
  blk: BlockTableEntry,
  nPixels: number,
): Float32Array {
  const out = new Float32Array(nPixels);
  switch (blk.dtype) {
    case DTYPE_U8: {
      for (let i = 0; i < nPixels; i++) {
        const q = bytes[i];
        out[i] = q === SENTINEL_U8 ? NaN : q * blk.scale + blk.offset;
      }
      break;
    }
    case DTYPE_U16: {
      for (let i = 0; i < nPixels; i++) {
        const q = bytes[2 * i] | (bytes[2 * i + 1] << 8);
        out[i] = q === SENTINEL_U16 ? NaN : q * blk.scale + blk.offset;
      }
      break;
    }
    case DTYPE_F32: {
      const dv = new DataView(
        bytes.buffer,
        bytes.byteOffset,
        bytes.byteLength,
      );
      for (let i = 0; i < nPixels; i++) {
        out[i] = dv.getFloat32(4 * i, true);
      }
      break;
    }
  }
  return out;
}
