import { decompress as zstdDecompress } from "fzstd";
import {
  DTYPE_F32,
  DTYPE_U16,
  DTYPE_U8,
  SENTINEL_U16,
  SENTINEL_U8,
  type BlockTableEntry,
} from "./format.js";

export const CODEC_CONSTANT = 0x01;
export const CODEC_RAW_ZSTD = 0x02;
export const CODEC_BITSHUFFLE_ZSTD = 0x03;
export const CODEC_DELTA_ZSTD = 0x04;

export function dtypeBytes(d: number): number {
  if (d === DTYPE_U8) return 1;
  if (d === DTYPE_U16) return 2;
  if (d === DTYPE_F32) return 4;
  return 0;
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
    throw new Error(`delta_zstd: unsupported stride ${stride}`);
  }
  return dst;
}

export function decodeCodec(
  blob: Uint8Array,
  dtype: number,
  nPixels: number,
): Uint8Array {
  if (blob.length < 1) throw new Error("codec: empty blob");
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
      const out = zstdDecompress(payload);
      if (out.length !== total) {
        throw new Error(`raw_zstd: got ${out.length}, want ${total}`);
      }
      return out;
    }
    case CODEC_BITSHUFFLE_ZSTD: {
      const inner = zstdDecompress(payload);
      const expectedInner = 8 * stride * ((nPixels + 7) >> 3);
      if (inner.length !== expectedInner) {
        throw new Error(
          `bitshuffle inner len ${inner.length}, want ${expectedInner}`,
        );
      }
      return bitshuffleDecode(inner, stride, nPixels);
    }
    case CODEC_DELTA_ZSTD: {
      const inner = zstdDecompress(payload);
      if (inner.length !== total) {
        throw new Error(
          `delta_zstd inner len ${inner.length}, want ${total}`,
        );
      }
      const w = Math.round(Math.sqrt(nPixels));
      if (w * w !== nPixels) {
        throw new Error("delta_zstd requires square tile");
      }
      return deltaDecode(inner, w, stride);
    }
    default:
      throw new Error(`unknown codec 0x${tag.toString(16)}`);
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
