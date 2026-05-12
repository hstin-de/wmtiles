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

// Per output-byte position b: read 8 source bytes (one per bit plane) and
// unpack each into 8 destination bytes in one shot. 8× fewer outer iterations
// than the bit-at-a-time variant, no read-modify-write on dst.
function bitshuffleDecode(
  src: Uint8Array,
  elemSize: number,
  elemCount: number,
): Uint8Array {
  const bytesPerPlane = (elemCount + 7) >> 3;
  const dst = new Uint8Array(elemSize * elemCount);
  const wholeGroups = elemCount >>> 3;
  const tail = elemCount & 7;

  for (let b = 0; b < elemSize; b++) {
    const base0 = b * 8 * bytesPerPlane;
    const p0 = base0;
    const p1 = base0 + bytesPerPlane;
    const p2 = base0 + 2 * bytesPerPlane;
    const p3 = base0 + 3 * bytesPerPlane;
    const p4 = base0 + 4 * bytesPerPlane;
    const p5 = base0 + 5 * bytesPerPlane;
    const p6 = base0 + 6 * bytesPerPlane;
    const p7 = base0 + 7 * bytesPerPlane;

    for (let g = 0; g < wholeGroups; g++) {
      const s0 = src[p0 + g];
      const s1 = src[p1 + g];
      const s2 = src[p2 + g];
      const s3 = src[p3 + g];
      const s4 = src[p4 + g];
      const s5 = src[p5 + g];
      const s6 = src[p6 + g];
      const s7 = src[p7 + g];

      const groupBase = (g << 3) * elemSize + b;
      // element e in 0..7 reads bit (7-e) of each source byte
      dst[groupBase] =
        ((s0 >> 7) & 1) | (((s1 >> 7) & 1) << 1) | (((s2 >> 7) & 1) << 2) |
        (((s3 >> 7) & 1) << 3) | (((s4 >> 7) & 1) << 4) | (((s5 >> 7) & 1) << 5) |
        (((s6 >> 7) & 1) << 6) | (((s7 >> 7) & 1) << 7);
      dst[groupBase + elemSize] =
        ((s0 >> 6) & 1) | (((s1 >> 6) & 1) << 1) | (((s2 >> 6) & 1) << 2) |
        (((s3 >> 6) & 1) << 3) | (((s4 >> 6) & 1) << 4) | (((s5 >> 6) & 1) << 5) |
        (((s6 >> 6) & 1) << 6) | (((s7 >> 6) & 1) << 7);
      dst[groupBase + 2 * elemSize] =
        ((s0 >> 5) & 1) | (((s1 >> 5) & 1) << 1) | (((s2 >> 5) & 1) << 2) |
        (((s3 >> 5) & 1) << 3) | (((s4 >> 5) & 1) << 4) | (((s5 >> 5) & 1) << 5) |
        (((s6 >> 5) & 1) << 6) | (((s7 >> 5) & 1) << 7);
      dst[groupBase + 3 * elemSize] =
        ((s0 >> 4) & 1) | (((s1 >> 4) & 1) << 1) | (((s2 >> 4) & 1) << 2) |
        (((s3 >> 4) & 1) << 3) | (((s4 >> 4) & 1) << 4) | (((s5 >> 4) & 1) << 5) |
        (((s6 >> 4) & 1) << 6) | (((s7 >> 4) & 1) << 7);
      dst[groupBase + 4 * elemSize] =
        ((s0 >> 3) & 1) | (((s1 >> 3) & 1) << 1) | (((s2 >> 3) & 1) << 2) |
        (((s3 >> 3) & 1) << 3) | (((s4 >> 3) & 1) << 4) | (((s5 >> 3) & 1) << 5) |
        (((s6 >> 3) & 1) << 6) | (((s7 >> 3) & 1) << 7);
      dst[groupBase + 5 * elemSize] =
        ((s0 >> 2) & 1) | (((s1 >> 2) & 1) << 1) | (((s2 >> 2) & 1) << 2) |
        (((s3 >> 2) & 1) << 3) | (((s4 >> 2) & 1) << 4) | (((s5 >> 2) & 1) << 5) |
        (((s6 >> 2) & 1) << 6) | (((s7 >> 2) & 1) << 7);
      dst[groupBase + 6 * elemSize] =
        ((s0 >> 1) & 1) | (((s1 >> 1) & 1) << 1) | (((s2 >> 1) & 1) << 2) |
        (((s3 >> 1) & 1) << 3) | (((s4 >> 1) & 1) << 4) | (((s5 >> 1) & 1) << 5) |
        (((s6 >> 1) & 1) << 6) | (((s7 >> 1) & 1) << 7);
      dst[groupBase + 7 * elemSize] =
        (s0 & 1) | ((s1 & 1) << 1) | ((s2 & 1) << 2) |
        ((s3 & 1) << 3) | ((s4 & 1) << 4) | ((s5 & 1) << 5) |
        ((s6 & 1) << 6) | ((s7 & 1) << 7);
    }

    if (tail > 0) {
      const g = wholeGroups;
      for (let k = 0; k < 8; k++) {
        const sByte = src[base0 + k * bytesPerPlane + g];
        const mask = 1 << k;
        for (let e = 0; e < tail; e++) {
          if ((sByte >> (7 - e)) & 1) {
            dst[(g * 8 + e) * elemSize + b] |= mask;
          }
        }
      }
    }
  }
  return dst;
}

// Writes into Uint8/Uint16Array auto-truncate to the element width, so the
// `& 0xff` / `& 0xffff` masks are redundant.
function deltaDecode(src: Uint8Array, w: number, stride: number): Uint8Array {
  const rowBytes = w * stride;
  const dst = new Uint8Array(src.length);
  dst.set(src.subarray(0, rowBytes));
  if (stride === 1) {
    for (let r = 1; r < w; r++) {
      const base = r * rowBytes;
      const prev = base - rowBytes;
      for (let c = 0; c < w; c++) {
        dst[base + c] = src[base + c] + dst[prev + c];
      }
    }
  } else if (stride === 2) {
    const src16 = asU16(src);
    const dst16 = new Uint16Array(dst.buffer, dst.byteOffset, src.length >>> 1);
    for (let r = 1; r < w; r++) {
      const base = r * w;
      const prev = base - w;
      for (let c = 0; c < w; c++) {
        dst16[base + c] = src16[base + c] + dst16[prev + c];
      }
    }
  } else {
    throw new FormatError(`delta_zstd: unsupported stride ${stride}`);
  }
  return dst;
}

function lorenzoDecode(src: Uint8Array, w: number, stride: number): Uint8Array {
  const dst = new Uint8Array(src.length);
  if (stride === 1) {
    dst[0] = src[0];
    for (let c = 1; c < w; c++) {
      dst[c] = src[c] + dst[c - 1];
    }
    for (let r = 1; r < w; r++) {
      const base = r * w;
      const prevRow = base - w;
      // pa = above-left, pb = above. Each pixel: 1 load (current above) + 2 reg reuses.
      let pa = dst[prevRow];
      let plt = (dst[base] = (src[base] + pa) & 0xff);
      for (let c = 1; c < w; c++) {
        const pb = dst[prevRow + c];
        const cur = (src[base + c] + plt + pb - pa) & 0xff;
        dst[base + c] = cur;
        pa = pb;
        plt = cur;
      }
    }
  } else if (stride === 2) {
    const src16 = asU16(src);
    const dst16 = new Uint16Array(dst.buffer, dst.byteOffset, src.length >>> 1);
    dst16[0] = src16[0];
    for (let c = 1; c < w; c++) {
      dst16[c] = src16[c] + dst16[c - 1];
    }
    for (let r = 1; r < w; r++) {
      const base = r * w;
      const prevRow = base - w;
      let pa = dst16[prevRow];
      let plt = (dst16[base] = (src16[base] + pa) & 0xffff);
      for (let c = 1; c < w; c++) {
        const pb = dst16[prevRow + c];
        const cur = (src16[base + c] + plt + pb - pa) & 0xffff;
        dst16[base + c] = cur;
        pa = pb;
        plt = cur;
      }
    }
  } else {
    throw new FormatError(`lorenzo_zstd: unsupported stride ${stride}`);
  }
  return dst;
}

// view src as Uint16Array. Decoder inputs come from fresh allocations
// (zstd decompress, new Uint8Array), so byteOffset is normally 0. If misaligned,
// copy into an aligned buffer.
function asU16(src: Uint8Array): Uint16Array {
  if ((src.byteOffset & 1) === 0) {
    return new Uint16Array(src.buffer, src.byteOffset, src.length >>> 1);
  }
  const copy = new Uint8Array(src);
  return new Uint16Array(copy.buffer, 0, copy.length >>> 1);
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
      if (stride === 1) {
        out.fill(payload[0]);
      } else if (stride === 2) {
        new Uint16Array(out.buffer, 0, nPixels).fill(
          payload[0] | (payload[1] << 8),
        );
      } else if (stride === 4) {
        // payload[0..3] is a little-endian f32; reinterpret it as a u32 pattern
        // and broadcast via Uint32Array.fill for one bulk write.
        const u32 =
          payload[0] |
          (payload[1] << 8) |
          (payload[2] << 16) |
          (payload[3] << 24);
        new Uint32Array(out.buffer, 0, nPixels).fill(u32 >>> 0);
      } else {
        for (let i = 0; i < nPixels; i++) {
          for (let j = 0; j < stride; j++) out[i * stride + j] = payload[j];
        }
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
  const scale = blk.scale;
  const offset = blk.offset;
  switch (blk.dtype) {
    case DTYPE_U8: {
      for (let i = 0; i < nPixels; i++) {
        const q = bytes[i];
        out[i] = q === SENTINEL_U8 ? NaN : q * scale + offset;
      }
      break;
    }
    case DTYPE_U16: {
      if ((bytes.byteOffset & 1) === 0) {
        const u16 = new Uint16Array(bytes.buffer, bytes.byteOffset, nPixels);
        for (let i = 0; i < nPixels; i++) {
          const q = u16[i];
          out[i] = q === SENTINEL_U16 ? NaN : q * scale + offset;
        }
      } else {
        for (let i = 0; i < nPixels; i++) {
          const q = bytes[2 * i] | (bytes[2 * i + 1] << 8);
          out[i] = q === SENTINEL_U16 ? NaN : q * scale + offset;
        }
      }
      break;
    }
    case DTYPE_F32: {
      // little-endian host assumed (web/x86/ARM); format is LE throughout
      if ((bytes.byteOffset & 3) === 0) {
        const f32 = new Float32Array(bytes.buffer, bytes.byteOffset, nPixels);
        out.set(f32);
      } else {
        // realign once via memcpy, then bulk-read; avoids 4*nPixels DataView calls
        const aligned = new Uint8Array(nPixels * 4);
        aligned.set(bytes.subarray(0, nPixels * 4));
        out.set(new Float32Array(aligned.buffer, 0, nPixels));
      }
      break;
    }
  }
  return out;
}
