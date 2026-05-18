export const HEADER_SIZE = 256;
export const SNAPSHOT_HEADER_SIZE = 128;
export const SNAPSHOT_TRAILER_SIZE = 16;
export const BLOCK_HEADER_SIZE = 64;
export const COLD_START_BYTES = 64 * 1024;
export const MAX_BLOCK_ROOT = 16 * 1024 - BLOCK_HEADER_SIZE;

export const MAGIC = [0x57, 0x4d, 0x54, 0x49, 0x4c, 0x45, 0x53, 0x00] as const;
export const HEADER_MAGIC_TAIL = 0xe7e7dead;
export const SNAPSHOT_TRAILER_MAGIC = 0xc0ffee42;
export const BLOCK_MAGIC = 0xb10cc0de;

export const FLAG_COLD_START_IN_WINDOW = 1 << 0;
export const FLAG_HAS_PREVIOUS_SNAPSHOT = 1 << 1;
export const FLAG_TIME_CATALOG_REGULAR = 1 << 2;

export const BLOCK_FLAG_HAS_LEAF_DIRECTORIES = 1 << 0;
export const BLOCK_FLAG_HAS_DICT = 1 << 1;
export const BLOCK_FLAG_RAW_GRID = 1 << 2;

export const RAW_GRID_HEADER_SIZE = 64;
export const RAW_GRID_SCHEMA_VERSION = 1;

export const COMP_NONE = 0;
export const COMP_GZIP = 1;
export const COMP_ZSTD = 2;

export type DType = 0 | 1 | 3;
export const DTYPE_U8: DType = 0;
export const DTYPE_U16: DType = 1;
export const DTYPE_F32: DType = 3;

export const SENTINEL_U8 = 0xff;
export const SENTINEL_U16 = 0xffff;

// Castagnoli (iSCSI) CRC32C, reflected polynomial 0x82F63B78 — matches Go's
// hash/crc32.Castagnoli, which is what the writer uses for header & snapshot CRCs.
const CRC32C_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let k = 0; k < 8; k++) {
      c = c & 1 ? 0x82f63b78 ^ (c >>> 1) : c >>> 1;
    }
    t[i] = c;
  }
  return t;
})();

export function crc32c(buf: Uint8Array): number {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) {
    c = CRC32C_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  }
  return (c ^ 0xffffffff) >>> 0;
}

export interface Header {
  formatVersion: number;
  flags: number;
  headerCRC: number;
  activeSnapshotOffset: number;
  activeSnapshotLength: number;
  previousSnapshotOffset: number;
  previousSnapshotLength: number;
  fileLogicalEnd: number;
  snapshotGeneration: number;
  internalCompression: number;
  tilePixelSizeLog2: number;
  minZoom: number;
  maxZoom: number;
  bboxLonMin: number;
  bboxLatMin: number;
  bboxLonMax: number;
  bboxLatMax: number;
}

export interface SnapshotHeader {
  schemaVersion: number;
  snapshotGeneration: number;
  creationTimeMs: number;
  referenceTimeMs: number;
  numVariables: number;
  numTimeSteps: number;
  numBlocks: number;
  variableCatalogOff: number;
  variableCatalogLen: number;
  timeCatalogOff: number;
  timeCatalogLen: number;
  blockTableRootOff: number;
  blockTableRootLen: number;
  blockTableLeavesOff: number;
  blockTableLeavesLen: number;
  metadataOff: number;
  metadataLen: number;
}

export interface SnapshotTrailer {
  snapshotTotalLength: number;
  crc32c: number;
}

export interface VariableEntry {
  id: number;
  name: string;
  unit: string;
  defaultDType: number;
  defaultCodec: number;
  defaultPrecisionHint: number;
  colormap: string;
  vmin: number;
  vmax: number;
}

export type TimeCatalog =
  | { regular: true; startMs: number; intervalMs: number; count: number }
  | { regular: false; count: number; timestampsMs: number[] };

export interface BlockTableEntry {
  variableID: number;
  timeID: number;
  compositeKey: bigint;
  isLeafPointer: boolean;
  blockOffset: number;
  blockLength: number;
  dtype: number;
  codec: number;
  scale: number;
  offset: number;
  nodata: number;
  valueMin: number;
  valueMax: number;
  numAddressedTiles: number;
  numDirectoryEntries: number;
  numTileContents: number;
}

export interface BlockHeader {
  blockFormatVersion: number;
  blockFlags: number;
  rootDirectoryOffset: number;
  rootDirectoryLength: number;
  leafDirectoriesOffset: number;
  leafDirectoriesLength: number;
  tileDataOffset: number;
  tileDataLength: number;
  numAddressedTiles: number;
  numDirectoryEntries: number;
}

export interface VarintBig {
  value: bigint;
  used: number;
}
export interface VarintNum {
  value: number;
  used: number;
}

// LEB128, mirrors Go varint.Read: bigint because uint64 > Number.MAX_SAFE_INTEGER
export function readVarint(buf: Uint8Array, pos: number): VarintBig {
  let v = 0n;
  let shift = 0n;
  for (let i = 0; i < 10; i++) {
    const c = buf[pos + i];
    if (c === undefined) throw new FormatError("varint: truncated");
    v |= BigInt(c & 0x7f) << shift;
    if ((c & 0x80) === 0) return { value: v, used: i + 1 };
    shift += 7n;
  }
  throw new FormatError("varint: overflow");
}

export function readVarintNum(buf: Uint8Array, pos: number): VarintNum {
  const { value, used } = readVarint(buf, pos);
  return { value: Number(value), used };
}

// undo zigzag: even maps to non-negative, odd maps to negative
function unzigzag64(v: bigint): bigint {
  return (v >> 1n) ^ -(v & 1n);
}

import { decompress as zstdDecompress } from "fzstd";
import { FormatError } from "./errors.js";

// uses the Streams API DecompressionStream so we don't bundle pako;
// works in modern browsers and Node ≥18 (no extra dependency)
async function gunzip(buf: Uint8Array): Promise<Uint8Array> {
  try {
    const stream = new Blob([buf as BlobPart])
      .stream()
      .pipeThrough(new DecompressionStream("gzip"));
    return new Uint8Array(await new Response(stream).arrayBuffer());
  } catch (err) {
    throw new FormatError("gzip decompression failed", { cause: err });
  }
}

export async function decompressInternal(
  buf: Uint8Array,
  comp: number,
  zstdOverride?: (b: Uint8Array) => Uint8Array,
): Promise<Uint8Array> {
  switch (comp) {
    case COMP_NONE:
      return buf;
    case COMP_GZIP:
      return await gunzip(buf);
    case COMP_ZSTD:
      try {
        return (zstdOverride ?? zstdDecompress)(buf);
      } catch (err) {
        throw new FormatError("zstd decompression failed", { cause: err });
      }
    default:
      throw new FormatError(`unknown internal compression ${comp}`);
  }
}

export function parseHeader(buf: Uint8Array): Header {
  for (let i = 0; i < 8; i++) {
    if (buf[i] !== MAGIC[i]) throw new FormatError("bad magic: not a WMTiles file");
  }
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const u64 = (off: number) => Number(dv.getBigUint64(off, true));
  const tail = dv.getUint32(252, true);
  if (tail !== HEADER_MAGIC_TAIL) {
    throw new FormatError(`header magic tail mismatch (got 0x${tail.toString(16)})`);
  }
  return {
    formatVersion: dv.getUint16(8, true),
    flags: dv.getUint16(10, true),
    headerCRC: dv.getUint32(12, true),
    activeSnapshotOffset: u64(16),
    activeSnapshotLength: u64(24),
    previousSnapshotOffset: u64(32),
    previousSnapshotLength: u64(40),
    fileLogicalEnd: u64(48),
    snapshotGeneration: u64(56),
    internalCompression: buf[64],
    tilePixelSizeLog2: buf[65],
    minZoom: buf[66],
    maxZoom: buf[67],
    bboxLonMin: dv.getInt32(68, true) / 1e7,
    bboxLatMin: dv.getInt32(72, true) / 1e7,
    bboxLonMax: dv.getInt32(76, true) / 1e7,
    bboxLatMax: dv.getInt32(80, true) / 1e7,
  };
}

export function parseSnapshotHeader(buf: Uint8Array): SnapshotHeader {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const u64 = (off: number) => Number(dv.getBigUint64(off, true));
  const i64 = (off: number) => Number(dv.getBigInt64(off, true));
  return {
    schemaVersion: u64(0),
    snapshotGeneration: u64(8),
    creationTimeMs: i64(16),
    referenceTimeMs: i64(24),
    numVariables: dv.getUint16(32, true),
    numTimeSteps: dv.getUint32(34, true),
    numBlocks: u64(40),
    variableCatalogOff: u64(48),
    variableCatalogLen: u64(56),
    timeCatalogOff: u64(64),
    timeCatalogLen: u64(72),
    blockTableRootOff: u64(80),
    blockTableRootLen: u64(88),
    blockTableLeavesOff: u64(96),
    blockTableLeavesLen: u64(104),
    metadataOff: u64(112),
    metadataLen: u64(120),
  };
}

export function parseSnapshotTrailer(buf: Uint8Array): SnapshotTrailer {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const m = dv.getUint32(0, true);
  if (m !== SNAPSHOT_TRAILER_MAGIC) {
    throw new FormatError(
      `snapshot trailer magic mismatch (got 0x${m.toString(16)})`,
    );
  }
  return {
    snapshotTotalLength: Number(dv.getBigUint64(4, true)),
    crc32c: dv.getUint32(12, true),
  };
}

export function parseVariableCatalog(
  buf: Uint8Array,
  count: number,
): VariableEntry[] {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const dec = new TextDecoder();
  let pos = 0;
  const out: VariableEntry[] = [];
  for (let i = 0; i < count; i++) {
    const id = dv.getUint16(pos, true);
    pos += 2;
    const nl = buf[pos];
    pos += 1;
    const name = dec.decode(buf.subarray(pos, pos + nl));
    pos += nl;
    const ul = buf[pos];
    pos += 1;
    const unit = dec.decode(buf.subarray(pos, pos + ul));
    pos += ul;
    const defaultDType = buf[pos];
    pos += 1;
    const defaultCodec = buf[pos];
    pos += 1;
    const defaultPrecisionHint = dv.getFloat64(pos, true);
    pos += 8;
    const cl = buf[pos];
    pos += 1;
    const colormap = dec.decode(buf.subarray(pos, pos + cl));
    pos += cl;
    const vmin = dv.getFloat64(pos, true);
    pos += 8;
    const vmax = dv.getFloat64(pos, true);
    pos += 8;
    out.push({
      id,
      name,
      unit,
      defaultDType,
      defaultCodec,
      defaultPrecisionHint,
      colormap,
      vmin,
      vmax,
    });
  }
  return out;
}

export function parseTimeCatalog(
  buf: Uint8Array,
  regular: boolean,
): TimeCatalog {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  if (regular) {
    return {
      regular: true,
      startMs: Number(dv.getBigInt64(0, true)),
      intervalMs: Number(dv.getBigInt64(8, true)),
      count: dv.getUint32(16, true),
    };
  }
  const n = dv.getUint32(0, true);
  if (n === 0) return { regular: false, count: 0, timestampsMs: [] };
  let pos = 4;
  const stamps = new Array<number>(n);
  const first = readVarint(buf, pos);
  pos += first.used;
  let prev = unzigzag64(first.value);
  stamps[0] = Number(prev);
  for (let i = 1; i < n; i++) {
    const r = readVarint(buf, pos);
    pos += r.used;
    prev += unzigzag64(r.value);
    stamps[i] = Number(prev);
  }
  return { regular: false, count: n, timestampsMs: stamps };
}

export function timeCatalogTimeAt(tc: TimeCatalog, idx: number): number {
  if (tc.regular) return tc.startMs + idx * tc.intervalMs;
  return tc.timestampsMs[idx];
}

export function parseBlockTable(buf: Uint8Array): BlockTableEntry[] {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  let pos = 0;
  const r0 = readVarintNum(buf, pos);
  pos += r0.used;
  const count = r0.value;
  if (count === 0) return [];

  const entries: BlockTableEntry[] = new Array(count);
  for (let i = 0; i < count; i++) {
    entries[i] = {} as BlockTableEntry;
  }

  let prev = 0n;
  for (let i = 0; i < count; i++) {
    const r = readVarint(buf, pos);
    pos += r.used;
    prev += r.value;
    entries[i].variableID = Number(prev >> 32n);
    entries[i].timeID = Number(prev & 0xffffffffn);
    entries[i].compositeKey = prev;
  }
  for (let i = 0; i < count; i++) {
    entries[i].isLeafPointer = buf[pos + i] !== 0;
  }
  pos += count;
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    entries[i].blockOffset = r.value;
  }
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    entries[i].blockLength = r.value;
  }
  for (let i = 0; i < count; i++) entries[i].dtype = buf[pos + i];
  pos += count;
  for (let i = 0; i < count; i++) entries[i].codec = buf[pos + i];
  pos += count;
  for (let i = 0; i < count; i++) {
    entries[i].scale = dv.getFloat64(pos + i * 8, true);
  }
  pos += count * 8;
  for (let i = 0; i < count; i++) {
    entries[i].offset = dv.getFloat64(pos + i * 8, true);
  }
  pos += count * 8;
  for (let i = 0; i < count; i++) {
    entries[i].nodata = dv.getUint32(pos + i * 4, true);
  }
  pos += count * 4;
  for (let i = 0; i < count; i++) {
    entries[i].valueMin = dv.getFloat64(pos + i * 8, true);
  }
  pos += count * 8;
  for (let i = 0; i < count; i++) {
    entries[i].valueMax = dv.getFloat64(pos + i * 8, true);
  }
  pos += count * 8;
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    entries[i].numAddressedTiles = r.value;
  }
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    entries[i].numDirectoryEntries = r.value;
  }
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    entries[i].numTileContents = r.value;
  }
  return entries;
}

export function lookupBlockTable(
  entries: BlockTableEntry[],
  variableID: number,
  timeID: number,
): BlockTableEntry | null {
  if (entries.length === 0) return null;
  const target = (BigInt(variableID) << 32n) | BigInt(timeID);
  let lo = 0;
  let hi = entries.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (entries[mid].compositeKey >= target) hi = mid;
    else lo = mid + 1;
  }
  if (lo < entries.length && entries[lo].compositeKey === target) {
    return entries[lo];
  }
  if (lo > 0 && entries[lo - 1].isLeafPointer) {
    return entries[lo - 1];
  }
  return null;
}

export function parseBlockHeader(buf: Uint8Array): BlockHeader {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const m = dv.getUint32(0, true);
  if (m !== BLOCK_MAGIC) {
    throw new FormatError(`block magic mismatch (got 0x${m.toString(16)})`);
  }
  const u64 = (off: number) => Number(dv.getBigUint64(off, true));
  return {
    blockFormatVersion: dv.getUint16(4, true),
    blockFlags: dv.getUint16(6, true),
    rootDirectoryOffset: u64(8),
    rootDirectoryLength: dv.getUint32(16, true),
    leafDirectoriesOffset: u64(24),
    leafDirectoriesLength: u64(32),
    tileDataOffset: u64(40),
    tileDataLength: u64(48),
    numAddressedTiles: dv.getUint32(56, true),
    numDirectoryEntries: dv.getUint32(60, true),
  };
}

export interface CoarseEntry {
  /** relative to block's leafDirectories region */
  offset: number;
  /** zero = empty cell (shouldn't happen in valid files) */
  length: number;
}

export interface RawGridSection {
  schemaVersion: number;
  chunkSizeLog2: number;
  /** side length in chunks (log2); 0 = trivial 1×1 */
  coarseSizeLog2: number;
  nx: number;
  ny: number;
  lat0: number;
  lon0: number;
  dy: number;
  dx: number;
  missingValue: number;
  chunkCountX: number;
  chunkCountY: number;
  /** row-major: cy * coarseCountX + cx */
  coarseTable: CoarseEntry[];
}

export interface FineIndex {
  /** cell-row-major, length = cellW * cellH */
  chunkOffsets: Float64Array;
  chunkLengths: Float64Array;
  cellW: number;
  cellH: number;
}

export function parseRawGridSection(buf: Uint8Array): RawGridSection {
  if (buf.length < RAW_GRID_HEADER_SIZE) {
    throw new FormatError(`raw grid: need ${RAW_GRID_HEADER_SIZE} bytes, got ${buf.length}`);
  }
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const schemaVersion = dv.getUint8(0);
  if (schemaVersion !== RAW_GRID_SCHEMA_VERSION) {
    throw new FormatError(`raw grid: unsupported schema version ${schemaVersion}`);
  }
  const chunkSizeLog2 = dv.getUint8(1);
  const coarseSizeLog2 = dv.getUint8(2);
  if (coarseSizeLog2 > 8) {
    throw new FormatError(`raw grid: coarse size log2 ${coarseSizeLog2} > 8`);
  }
  const nx = dv.getUint32(4, true);
  const ny = dv.getUint32(8, true);
  const lat0 = dv.getFloat64(16, true);
  const lon0 = dv.getFloat64(24, true);
  const dy = dv.getFloat64(32, true);
  const dx = dv.getFloat64(40, true);
  const missingValue = dv.getFloat64(48, true);
  const chunkCountX = dv.getUint32(56, true);
  const chunkCountY = dv.getUint32(60, true);
  if (chunkCountX === 0 || chunkCountY === 0) {
    throw new FormatError("raw grid: zero-sized chunk grid");
  }
  const cs = 1 << coarseSizeLog2;
  const coarseCountX = Math.ceil(chunkCountX / cs);
  const coarseCountY = Math.ceil(chunkCountY / cs);
  const coarseCount = coarseCountX * coarseCountY;
  const need = RAW_GRID_HEADER_SIZE + 8 * coarseCount;
  if (buf.length < need) {
    throw new FormatError(`raw grid: root truncated (need ${need}, got ${buf.length})`);
  }
  const coarseTable: CoarseEntry[] = new Array(coarseCount);
  let pos = RAW_GRID_HEADER_SIZE;
  for (let i = 0; i < coarseCount; i++) {
    coarseTable[i] = {
      offset: dv.getUint32(pos, true),
      length: dv.getUint32(pos + 4, true),
    };
    pos += 8;
  }
  return {
    schemaVersion,
    chunkSizeLog2,
    coarseSizeLog2,
    nx,
    ny,
    lat0,
    lon0,
    dy,
    dx,
    missingValue,
    chunkCountX,
    chunkCountY,
    coarseTable,
  };
}

// expectedCount = cellW * cellH for the cell.
export function parseFineIndex(buf: Uint8Array, expectedCount: number): FineIndex {
  const chunkOffsets = new Float64Array(expectedCount);
  const chunkLengths = new Float64Array(expectedCount);
  let pos = 0;
  for (let i = 0; i < expectedCount; i++) {
    let v = 0;
    let shift = 0;
    for (let k = 0; k < 10; k++) {
      const c = buf[pos + k];
      if (c === undefined) throw new FormatError("fine index varint: truncated");
      if (shift < 28) {
        v |= (c & 0x7f) << shift;
      } else {
        v += (c & 0x7f) * Math.pow(2, shift);
      }
      if ((c & 0x80) === 0) {
        pos += k + 1;
        chunkOffsets[i] = v;
        v = -1;
        break;
      }
      shift += 7;
    }
    if (v !== -1) throw new FormatError("fine index varint: overflow");
  }
  for (let i = 0; i < expectedCount; i++) {
    let v = 0;
    let shift = 0;
    for (let k = 0; k < 10; k++) {
      const c = buf[pos + k];
      if (c === undefined) throw new FormatError("fine index varint: truncated");
      if (shift < 28) {
        v |= (c & 0x7f) << shift;
      } else {
        v += (c & 0x7f) * Math.pow(2, shift);
      }
      if ((c & 0x80) === 0) {
        pos += k + 1;
        chunkLengths[i] = v;
        v = -1;
        break;
      }
      shift += 7;
    }
    if (v !== -1) throw new FormatError("fine index varint: overflow");
  }
  // cellW/cellH are caller-known; this struct just holds raw flat arrays.
  return { chunkOffsets, chunkLengths, cellW: 0, cellH: 0 };
}

export function rawGridChunkSize(s: RawGridSection): number {
  return 1 << s.chunkSizeLog2;
}

export function rawGridChunkWidth(s: RawGridSection, cx: number): number {
  const cs = rawGridChunkSize(s);
  return Math.min((cx + 1) * cs, s.nx) - cx * cs;
}

export function rawGridChunkHeight(s: RawGridSection, cy: number): number {
  const cs = rawGridChunkSize(s);
  return Math.min((cy + 1) * cs, s.ny) - cy * cs;
}

export function rawGridCoarseSize(s: RawGridSection): number {
  return 1 << s.coarseSizeLog2;
}

export function rawGridCoarseCountX(s: RawGridSection): number {
  const cs = rawGridCoarseSize(s);
  return Math.ceil(s.chunkCountX / cs);
}

export function rawGridCoarseCountY(s: RawGridSection): number {
  const cs = rawGridCoarseSize(s);
  return Math.ceil(s.chunkCountY / cs);
}

// edge cells truncate.
export function rawGridCoarseCellExtent(
  s: RawGridSection,
  coarseCx: number,
  coarseCy: number,
): { w: number; h: number } {
  const cs = rawGridCoarseSize(s);
  return {
    w: Math.min(cs, s.chunkCountX - coarseCx * cs),
    h: Math.min(cs, s.chunkCountY - coarseCy * cs),
  };
}

export function rawGridCoarseIndexOf(
  s: RawGridSection,
  cx: number,
  cy: number,
): { coarseIdx: number; localIdx: number; cellW: number; cellH: number } {
  const cs = rawGridCoarseSize(s);
  const coarseCx = Math.floor(cx / cs);
  const coarseCy = Math.floor(cy / cs);
  const coarseCountX = rawGridCoarseCountX(s);
  const { w: cellW, h: cellH } = rawGridCoarseCellExtent(s, coarseCx, coarseCy);
  const localCx = cx - coarseCx * cs;
  const localCy = cy - coarseCy * cs;
  return {
    coarseIdx: coarseCy * coarseCountX + coarseCx,
    localIdx: localCy * cellW + localCx,
    cellW,
    cellH,
  };
}

export interface Directory {
  count: number;
  tileIDs: BigInt64Array;
  runLen: Uint32Array;
  length: Uint32Array;
  offsets: Float64Array;
}

export interface DirEntry {
  isLeaf: boolean;
  length: number;
  offset: number;
  tileID: bigint;
}

export function parseDirectory(buf: Uint8Array): Directory {
  let pos = 0;
  const r0 = readVarintNum(buf, pos);
  pos += r0.used;
  const count = r0.value;
  if (count === 0) {
    return {
      count: 0,
      tileIDs: new BigInt64Array(),
      runLen: new Uint32Array(),
      length: new Uint32Array(),
      offsets: new Float64Array(),
    };
  }

  const tileIDs = new BigInt64Array(count);
  let prev = 0n;
  for (let i = 0; i < count; i++) {
    const r = readVarint(buf, pos);
    pos += r.used;
    prev += r.value;
    tileIDs[i] = prev;
  }
  const runLen = new Uint32Array(count);
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    runLen[i] = r.value;
  }
  const length = new Uint32Array(count);
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    length[i] = r.value;
  }
  const offsets = new Float64Array(count);
  for (let i = 0; i < count; i++) {
    const r = readVarintNum(buf, pos);
    pos += r.used;
    if (r.value === 0) {
      if (i === 0) throw new FormatError("directory: first offset cannot be implicit");
      offsets[i] = offsets[i - 1] + length[i - 1];
    } else {
      offsets[i] = r.value - 1;
    }
  }
  return { count, tileIDs, runLen, length, offsets };
}

export function findTile(dir: Directory, target: bigint): DirEntry | null {
  if (!dir.count) return null;
  const ids = dir.tileIDs;
  let lo = 0;
  let hi = dir.count;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (ids[mid] > target) hi = mid;
    else lo = mid + 1;
  }
  const idx = lo - 1;
  if (idx < 0) return null;
  const rl = dir.runLen[idx];
  const tid = ids[idx];
  if (rl === 0) {
    return {
      isLeaf: true,
      length: dir.length[idx],
      offset: dir.offsets[idx],
      tileID: tid,
    };
  }
  if (target < tid + BigInt(rl)) {
    return {
      isLeaf: false,
      length: dir.length[idx],
      offset: dir.offsets[idx],
      tileID: tid,
    };
  }
  return null;
}
