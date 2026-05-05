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

export const COMP_NONE = 0;
export const COMP_GZIP = 1;
export const COMP_ZSTD = 2;

export type DType = 0 | 1 | 3;
export const DTYPE_U8: DType = 0;
export const DTYPE_U16: DType = 1;
export const DTYPE_F32: DType = 3;

export const SENTINEL_U8 = 0xff;
export const SENTINEL_U16 = 0xffff;

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
    if (c === undefined) throw new Error("varint: truncated");
    v |= BigInt(c & 0x7f) << shift;
    if ((c & 0x80) === 0) return { value: v, used: i + 1 };
    shift += 7n;
  }
  throw new Error("varint: overflow");
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

// uses the Streams API DecompressionStream so we don't bundle pako;
// works in modern browsers and Node ≥18 (no extra dependency)
async function gunzip(buf: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([buf as BlobPart])
    .stream()
    .pipeThrough(new DecompressionStream("gzip"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

export async function decompressInternal(
  buf: Uint8Array,
  comp: number,
): Promise<Uint8Array> {
  switch (comp) {
    case COMP_NONE:
      return buf;
    case COMP_GZIP:
      return await gunzip(buf);
    case COMP_ZSTD:
      return zstdDecompress(buf);
    default:
      throw new Error(`unknown internal compression ${comp}`);
  }
}

export function parseHeader(buf: Uint8Array): Header {
  for (let i = 0; i < 8; i++) {
    if (buf[i] !== MAGIC[i]) throw new Error("bad magic: not a WMTiles file");
  }
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const u64 = (off: number) => Number(dv.getBigUint64(off, true));
  const tail = dv.getUint32(252, true);
  if (tail !== HEADER_MAGIC_TAIL) {
    throw new Error(`header magic tail mismatch (got 0x${tail.toString(16)})`);
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
    throw new Error(
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
    throw new Error(`block magic mismatch (got 0x${m.toString(16)})`);
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
      if (i === 0) throw new Error("directory: first offset cannot be implicit");
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
