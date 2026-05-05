import {
  BLOCK_HEADER_SIZE,
  COLD_START_BYTES,
  FLAG_HAS_PREVIOUS_SNAPSHOT,
  FLAG_TIME_CATALOG_REGULAR,
  HEADER_SIZE,
  MAX_BLOCK_ROOT,
  SNAPSHOT_TRAILER_SIZE,
  decompressInternal,
  findTile,
  lookupBlockTable,
  parseBlockHeader,
  parseBlockTable,
  parseDirectory,
  parseHeader,
  parseSnapshotHeader,
  parseSnapshotTrailer,
  parseTimeCatalog,
  parseVariableCatalog,
  type BlockHeader,
  type BlockTableEntry,
  type Directory,
  type Header,
  type SnapshotHeader,
  type TimeCatalog,
  type VariableEntry,
} from "./format.js";
import { decodeCodec, dequantize } from "./decoder.js";
import { encode3D } from "./tileid.js";

export interface RangeFetcher {
  fetchRange(offset: number, length: number): Promise<Uint8Array>;
}

export function httpRangeFetcher(url: string, init?: RequestInit): RangeFetcher {
  return {
    async fetchRange(offset, length) {
      if (length === 0) return new Uint8Array();
      const headers = new Headers(init?.headers);
      headers.set("Range", `bytes=${offset}-${offset + length - 1}`);
      const resp = await fetch(url, { ...init, headers });
      if (!resp.ok && resp.status !== 206) {
        throw new Error(
          `HTTP ${resp.status} fetching bytes=${offset}-${offset + length - 1}`,
        );
      }
      return new Uint8Array(await resp.arrayBuffer());
    },
  };
}

export function bytesFetcher(buf: Uint8Array): RangeFetcher {
  return {
    async fetchRange(offset, length) {
      if (length === 0) return new Uint8Array();
      return buf.subarray(offset, offset + length);
    },
  };
}

interface CachedBlock {
  header: BlockHeader;
  root: Directory;
}

export interface TileCoord {
  z: number;
  x: number;
  y: number;
}

export interface CoalesceOptions {
  maxGapBytes?: number;
  maxRequestBytes?: number;
}

export class WMT {
  private src: RangeFetcher;
  header!: Header;
  snapshotHeader!: SnapshotHeader;
  catalog!: VariableEntry[];
  varByName!: Map<string, VariableEntry>;
  timeCatalog!: TimeCatalog;
  blockTableRoot!: BlockTableEntry[];
  pixSize!: number;
  nPixels!: number;

  private blockTableLeavesOffAbs = 0;
  private btLeafCache = new Map<number, BlockTableEntry[]>();
  // *Fetches maps coalesce concurrent loads for the same offset into one in-flight promise
  private btLeafFetches = new Map<number, Promise<BlockTableEntry[]>>();
  private blockCache = new Map<number, CachedBlock>();
  private blockFetches = new Map<number, Promise<CachedBlock>>();
  private blockLeafCache = new Map<string, Directory>();
  private coldBuf: Uint8Array | null = null;

  constructor(src: RangeFetcher) {
    this.src = src;
  }

  async open(): Promise<this> {
    this.coldBuf = await this.src.fetchRange(0, COLD_START_BYTES);
    if (this.coldBuf.length < HEADER_SIZE) {
      throw new Error(
        `cold-start fetch returned ${this.coldBuf.length} B, need ≥${HEADER_SIZE}`,
      );
    }
    this.header = parseHeader(this.coldBuf.subarray(0, HEADER_SIZE));
    if (this.header.formatVersion !== 1) {
      throw new Error(
        `unsupported format version ${this.header.formatVersion}`,
      );
    }
    this.pixSize = 1 << this.header.tilePixelSizeLog2;
    this.nPixels = this.pixSize * this.pixSize;

    try {
      await this.loadSnapshot(
        this.header.activeSnapshotOffset,
        this.header.activeSnapshotLength,
      );
    } catch (err) {
      if (
        (this.header.flags & FLAG_HAS_PREVIOUS_SNAPSHOT) !== 0 &&
        this.header.previousSnapshotLength > 0 &&
        this.header.previousSnapshotOffset > 0
      ) {
        await this.loadSnapshot(
          this.header.previousSnapshotOffset,
          this.header.previousSnapshotLength,
        );
      } else {
        throw err;
      }
    }
    return this;
  }

  private async loadSnapshot(off: number, length: number): Promise<void> {
    const snapBuf = await this.readAbs(off, length);
    this.snapshotHeader = parseSnapshotHeader(snapBuf.subarray(0, 128));
    parseSnapshotTrailer(
      snapBuf.subarray(snapBuf.length - SNAPSHOT_TRAILER_SIZE),
    );

    const sh = this.snapshotHeader;
    const comp = this.header.internalCompression;

    const varBlob = snapBuf.subarray(
      sh.variableCatalogOff,
      sh.variableCatalogOff + sh.variableCatalogLen,
    );
    const varRaw = await decompressInternal(varBlob, comp);
    this.catalog = parseVariableCatalog(varRaw, sh.numVariables);
    this.varByName = new Map(this.catalog.map((v) => [v.name, v]));

    const regular = (this.header.flags & FLAG_TIME_CATALOG_REGULAR) !== 0;
    let tcBlob = snapBuf.subarray(
      sh.timeCatalogOff,
      sh.timeCatalogOff + sh.timeCatalogLen,
    );
    if (!regular) tcBlob = await decompressInternal(tcBlob, comp);
    this.timeCatalog = parseTimeCatalog(tcBlob, regular);

    const rootBlob = snapBuf.subarray(
      sh.blockTableRootOff,
      sh.blockTableRootOff + sh.blockTableRootLen,
    );
    const rootRaw = await decompressInternal(rootBlob, comp);
    this.blockTableRoot = parseBlockTable(rootRaw);
    this.blockTableLeavesOffAbs = off + sh.blockTableLeavesOff;
  }

  stepTimeMs(t: number): number {
    if (this.timeCatalog.regular) {
      return this.timeCatalog.startMs + t * this.timeCatalog.intervalMs;
    }
    return this.timeCatalog.timestampsMs[t];
  }

  async lookupBlock(
    variableID: number,
    timeID: number,
  ): Promise<BlockTableEntry | null> {
    let row = lookupBlockTable(this.blockTableRoot, variableID, timeID);
    if (!row) return null;
    if (row.isLeafPointer) {
      const leaf = await this.loadBlockTableLeaf(
        row.blockOffset,
        row.blockLength,
      );
      row = lookupBlockTable(leaf, variableID, timeID);
      if (!row || row.isLeafPointer) return null;
    }
    return row;
  }

  async getTilePixels(
    varName: string,
    t: number,
    z: number,
    x: number,
    y: number,
  ): Promise<Float32Array | null> {
    const v = this.varByName.get(varName);
    if (!v) throw new Error(`unknown variable ${varName}`);
    const n = 1 << z;
    if (x < 0 || x >= n || y < 0 || y >= n) return null;

    const blk = await this.lookupBlock(v.id, t);
    if (!blk) return null;

    const tid = encode3D(z, x, y);
    const { header: blkHdr, root } = await this.loadBlockHeader(
      blk.blockOffset,
      blk.blockLength,
    );

    let entry = findTile(root, tid);
    if (!entry) return null;
    if (entry.isLeaf) {
      const leaf = await this.loadBlockLeaf(
        blk.blockOffset,
        blkHdr,
        entry.offset,
        entry.length,
      );
      entry = findTile(leaf, tid);
      if (!entry || entry.isLeaf) return null;
    }
    const blob = await this.src.fetchRange(
      blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
      entry.length,
    );
    const decoded = decodeCodec(blob, blk.dtype, this.nPixels);
    return dequantize(decoded, blk, this.nPixels);
  }

  async getTilesInBlock(
    varName: string,
    t: number,
    coords: TileCoord[],
    opts: CoalesceOptions = {},
  ): Promise<Array<Float32Array | null>> {
    const maxGap = opts.maxGapBytes ?? 64 * 1024;
    const maxReq = opts.maxRequestBytes ?? 4 * 1024 * 1024;

    const out: Array<Float32Array | null> = new Array(coords.length).fill(null);
    if (coords.length === 0) return out;

    const v = this.varByName.get(varName);
    if (!v) throw new Error(`unknown variable ${varName}`);
    const blk = await this.lookupBlock(v.id, t);
    if (!blk) {
      for (let i = 0; i < coords.length; i++) {
        out[i] = nanArray(this.nPixels);
      }
      return out;
    }
    const { header: blkHdr, root } = await this.loadBlockHeader(
      blk.blockOffset,
      blk.blockLength,
    );

    type Need = { i: number; fileOff: number; length: number };
    const needs: Need[] = [];
    for (let i = 0; i < coords.length; i++) {
      const c = coords[i];
      const n = 1 << c.z;
      if (
        c.z < this.header.minZoom ||
        c.z > this.header.maxZoom ||
        c.x < 0 || c.x >= n || c.y < 0 || c.y >= n
      ) {
        out[i] = nanArray(this.nPixels);
        continue;
      }
      const tid = encode3D(c.z, c.x, c.y);
      let entry = findTile(root, tid);
      if (!entry) {
        out[i] = nanArray(this.nPixels);
        continue;
      }
      if (entry.isLeaf) {
        const leaf = await this.loadBlockLeaf(
          blk.blockOffset,
          blkHdr,
          entry.offset,
          entry.length,
        );
        entry = findTile(leaf, tid);
        if (!entry || entry.isLeaf) {
          out[i] = nanArray(this.nPixels);
          continue;
        }
      }
      needs.push({
        i,
        fileOff: blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
        length: entry.length,
      });
    }
    if (needs.length === 0) return out;

    needs.sort((a, b) => a.fileOff - b.fileOff);

    interface Group {
      start: number;
      end: number;
      members: Need[];
    }
    const groups: Group[] = [];
    for (const n of needs) {
      const end = n.fileOff + n.length;
      const last = groups[groups.length - 1];
      if (last) {
        const gap = n.fileOff > last.end ? n.fileOff - last.end : 0;
        const newEnd = end > last.end ? end : last.end;
        if (gap <= maxGap && newEnd - last.start <= maxReq) {
          last.end = newEnd;
          last.members.push(n);
          continue;
        }
      }
      groups.push({ start: n.fileOff, end, members: [n] });
    }

    for (const g of groups) {
      const buf = await this.src.fetchRange(g.start, g.end - g.start);
      for (const m of g.members) {
        const localOff = m.fileOff - g.start;
        const blob = buf.subarray(localOff, localOff + m.length);
        const decoded = decodeCodec(blob, blk.dtype, this.nPixels);
        out[m.i] = dequantize(decoded, blk, this.nPixels);
      }
    }
    return out;
  }

  private async readAbs(offset: number, length: number): Promise<Uint8Array> {
    if (length === 0) return new Uint8Array();
    // serve from the cold-start prefetch when the range is in window: saves a round trip
    if (this.coldBuf && offset + length <= this.coldBuf.length) {
      return this.coldBuf.subarray(offset, offset + length);
    }
    return await this.src.fetchRange(offset, length);
  }

  private async loadBlockTableLeaf(
    leafOff: number,
    leafLen: number,
  ): Promise<BlockTableEntry[]> {
    const cached = this.btLeafCache.get(leafOff);
    if (cached) return cached;
    let inFlight = this.btLeafFetches.get(leafOff);
    if (!inFlight) {
      inFlight = (async () => {
        const raw = await this.readAbs(
          this.blockTableLeavesOffAbs + leafOff,
          leafLen,
        );
        const buf = await decompressInternal(raw, this.header.internalCompression);
        const entries = parseBlockTable(buf);
        this.btLeafCache.set(leafOff, entries);
        this.btLeafFetches.delete(leafOff);
        return entries;
      })();
      this.btLeafFetches.set(leafOff, inFlight);
    }
    return await inFlight;
  }

  private async loadBlockHeader(
    blockOff: number,
    blockLen: number,
  ): Promise<CachedBlock> {
    const cached = this.blockCache.get(blockOff);
    if (cached) return cached;
    let inFlight = this.blockFetches.get(blockOff);
    if (!inFlight) {
      inFlight = (async () => {
        const want = Math.min(BLOCK_HEADER_SIZE + MAX_BLOCK_ROOT, blockLen);
        const prefix = await this.readAbs(blockOff, want);
        const header = parseBlockHeader(prefix.subarray(0, BLOCK_HEADER_SIZE));
        const rootEnd = header.rootDirectoryOffset + header.rootDirectoryLength;
        let rootRaw: Uint8Array;
        if (rootEnd <= prefix.length) {
          rootRaw = prefix.subarray(header.rootDirectoryOffset, rootEnd);
        } else {
          rootRaw = await this.src.fetchRange(
            blockOff + header.rootDirectoryOffset,
            header.rootDirectoryLength,
          );
        }
        const rootBuf = await decompressInternal(
          rootRaw,
          this.header.internalCompression,
        );
        const root = parseDirectory(rootBuf);
        const out = { header, root };
        this.blockCache.set(blockOff, out);
        this.blockFetches.delete(blockOff);
        return out;
      })();
      this.blockFetches.set(blockOff, inFlight);
    }
    return await inFlight;
  }

  private async loadBlockLeaf(
    blockOff: number,
    header: BlockHeader,
    leafOff: number,
    leafLen: number,
  ): Promise<Directory> {
    const cacheKey = `${blockOff}:${leafOff}`;
    const cached = this.blockLeafCache.get(cacheKey);
    if (cached) return cached;
    const raw = await this.src.fetchRange(
      blockOff + header.leafDirectoriesOffset + leafOff,
      leafLen,
    );
    const buf = await decompressInternal(raw, this.header.internalCompression);
    const dir = parseDirectory(buf);
    this.blockLeafCache.set(cacheKey, dir);
    return dir;
  }
}

function nanArray(n: number): Float32Array {
  const a = new Float32Array(n);
  for (let i = 0; i < n; i++) a[i] = NaN;
  return a;
}
