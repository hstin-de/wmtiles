import {
  BLOCK_HEADER_SIZE,
  COLD_START_BYTES,
  FLAG_HAS_PREVIOUS_SNAPSHOT,
  FLAG_TIME_CATALOG_REGULAR,
  HEADER_SIZE,
  MAX_BLOCK_ROOT,
  SNAPSHOT_TRAILER_SIZE,
  crc32c,
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
import { encode3D, latLonToTilePixel } from "./tileid.js";
import { debugSink, emitDebug } from "./debug.js";
import {
  FormatError,
  SourceError,
  TimeOutOfRangeError,
  UnknownVariableError,
} from "./errors.js";

// ---------- Public types ----------

export interface ByteSource {
  read(offset: number, length: number): Promise<Uint8Array>;
}

export type WMTSource = ByteSource | Uint8Array | ArrayBuffer | string | URL;

export interface OpenOptions {
  /** Used when opening from a URL string or URL object. */
  requestInit?: RequestInit;
}

export interface BBox {
  west: number;
  south: number;
  east: number;
  north: number;
}

export interface ZoomRange {
  min: number;
  max: number;
}

export interface ValueRange {
  min: number;
  max: number;
}

/** Either a step index (number) or an absolute time (Date). Always disjoint. */
export type TimeRef = number | Date;

export type TimeAxis =
  | { kind: "regular"; start: Date; intervalMs: number; count: number }
  | { kind: "irregular"; times: readonly Date[] };

export interface TileCoord {
  z: number;
  x: number;
  y: number;
}

export interface CoalesceOptions {
  maxGapBytes?: number;
  maxRequestBytes?: number;
}

export interface TileRequest {
  time: TimeRef;
  z: number;
  x: number;
  y: number;
}

export interface TilesRequest {
  time: TimeRef;
  coords: readonly TileCoord[];
  coalesce?: CoalesceOptions;
}

export interface SampleRequest {
  time: TimeRef;
  lat: number;
  lon: number;
  /** Defaults to maxZoom of the file. */
  z?: number;
}

export interface ForecastRequest {
  lat: number;
  lon: number;
  variables: readonly string[];
  /** Defaults to maxZoom of the file. */
  z?: number;
  /** Subset of time steps to include. Defaults to the full time axis. */
  timeRange?: { start?: TimeRef; end?: TimeRef };
}

export interface ForecastResult {
  readonly times: readonly Date[];
  /** One Float32Array per variable name; NaN marks missing/NoData. */
  readonly values: Readonly<Record<string, Float32Array>>;
}

export interface ValueRequest {
  lat: number;
  lon: number;
  time: TimeRef;
  variables: readonly string[];
  /** Defaults to maxZoom of the file. */
  z?: number;
}

export interface ValueResult {
  /** Resolved absolute time (matters when caller passed a step index). */
  readonly time: Date;
  /** NaN marks missing/NoData. Key set equals req.variables. */
  readonly values: Readonly<Record<string, number>>;
}

// ---------- Sources ----------

export function httpSource(url: string | URL, init?: RequestInit): ByteSource {
  const href = String(url);
  return {
    async read(offset, length) {
      if (length === 0) return new Uint8Array();
      const headers = new Headers(init?.headers);
      headers.set("Range", `bytes=${offset}-${offset + length - 1}`);
      let resp: Response;
      try {
        resp = await fetch(href, { ...init, headers });
      } catch (err) {
        throw new SourceError(
          `HTTP fetch failed for bytes=${offset}-${offset + length - 1}`,
          { cause: err },
        );
      }

      const body = async (): Promise<Uint8Array> => {
        try {
          return new Uint8Array(await resp.arrayBuffer());
        } catch (err) {
          throw new SourceError(
            `HTTP body read failed for bytes=${offset}-${offset + length - 1}`,
            { cause: err },
          );
        }
      };
      const trim = (buf: Uint8Array): Uint8Array =>
        buf.length > length ? buf.subarray(0, length) : buf;

      if (resp.status === 206) {
        return trim(await body());
      }

      if (resp.status === 200 && offset === 0) {
        return trim(await body());
      }

      if (resp.status === 200) {
        throw new SourceError(
          `HTTP server did not honor Range for bytes=${offset}-${offset + length - 1}`,
        );
      }

      throw new SourceError(
        `HTTP ${resp.status} fetching bytes=${offset}-${offset + length - 1}`,
      );
    },
  };
}

export function bytesSource(bytes: Uint8Array | ArrayBuffer): ByteSource {
  const buf = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  return {
    async read(offset, length) {
      if (length === 0) return new Uint8Array();
      return buf.subarray(offset, offset + length);
    },
  };
}

export async function open(
  source: WMTSource,
  options?: OpenOptions,
): Promise<WMT> {
  return await WMT.open(source, options);
}

function toByteSource(source: WMTSource, options?: OpenOptions): ByteSource {
  if (source instanceof Uint8Array || source instanceof ArrayBuffer) {
    return bytesSource(source);
  }
  if (typeof source === "string" || source instanceof URL) {
    return httpSource(source, options?.requestInit);
  }
  return source;
}

// ---------- Variable handle ----------

type TileFetcher = (
  varId: number,
  t: number,
  z: number,
  x: number,
  y: number,
) => Promise<Float32Array | null>;

type TilesFetcher = (
  varId: number,
  t: number,
  coords: readonly TileCoord[],
  opts?: CoalesceOptions,
) => Promise<Float32Array[]>;

type Sampler = (
  varId: number,
  t: number,
  lat: number,
  lon: number,
  z: number,
) => Promise<number | null>;

export interface Variable {
  /** Numeric ID assigned by the encoder. */
  readonly id: number;
  readonly name: string;
  readonly unit: string;
  readonly colormap: string;
  readonly range: ValueRange;
  readonly precisionHint: number;
  /** Fetch one tile of pixels. Returns null if the tile is missing/out of range. */
  tile(req: TileRequest): Promise<Float32Array | null>;
  /**
   * Fetch many tiles for the same time step in coalesced range requests.
   * Out-of-range or missing tiles are returned as NaN-filled arrays at their
   * position, so output length matches input length.
   */
  tiles(req: TilesRequest): Promise<Float32Array[]>;
  /**
   * Sample the nearest pixel value at (lat, lon). Returns null on invalid
   * coords or out-of-range zoom; NaN if the file marks NoData at that pixel.
   */
  sample(req: SampleRequest): Promise<number | null>;
}

class WMTVariable implements Variable {
  /** @internal */
  constructor(
    private readonly _timeIndexOf: (ref: TimeRef) => number,
    private readonly _fetchTile: TileFetcher,
    private readonly _fetchTiles: TilesFetcher,
    private readonly _sample: Sampler,
    private readonly _maxZoom: number,
    /** Numeric ID assigned by the encoder. */
    readonly id: number,
    readonly name: string,
    readonly unit: string,
    readonly colormap: string,
    readonly range: ValueRange,
    readonly precisionHint: number,
  ) {}

  /** Fetch one tile of pixels. Returns null if the tile is missing/out of range. */
  async tile(req: TileRequest): Promise<Float32Array | null> {
    const t = this._timeIndexOf(req.time);
    return this._fetchTile(this.id, t, req.z, req.x, req.y);
  }

  /**
   * Fetch many tiles for the same time step in 1–2 coalesced range requests.
   * Out-of-range or missing tiles are returned as NaN-filled arrays at their
   * position (never null), so output length matches input length.
   */
  async tiles(req: TilesRequest): Promise<Float32Array[]> {
    const t = this._timeIndexOf(req.time);
    return this._fetchTiles(this.id, t, req.coords, req.coalesce);
  }

  /**
   * Sample the nearest pixel value at (lat, lon). Returns null on invalid
   * coords or out-of-range zoom; NaN if the file marks NoData at that pixel.
   */
  async sample(req: SampleRequest): Promise<number | null> {
    const t = this._timeIndexOf(req.time);
    const z = req.z ?? this._maxZoom;
    return this._sample(this.id, t, req.lat, req.lon, z);
  }
}

// ---------- WMT reader ----------

interface CachedBlock {
  header: BlockHeader;
  root: Directory;
}

export class WMT {
  private readonly _src: ByteSource;
  private _header!: Header;
  private _snapshot!: SnapshotHeader;
  private _catalog!: VariableEntry[];
  private _blockTableRoot!: BlockTableEntry[];
  private _timeCatalog!: TimeCatalog;
  private _tileSize!: number;
  private _nPixels!: number;
  private _variables!: ReadonlyArray<Variable>;
  private _byName!: Map<string, Variable>;
  private _timeAxis: TimeAxis | null = null;

  private _btLeavesAbs = 0;
  private _btLeafCache = new Map<number, BlockTableEntry[]>();
  private _btLeafFetches = new Map<number, Promise<BlockTableEntry[]>>();
  private _blockCache = new Map<number, CachedBlock>();
  private _blockFetches = new Map<number, Promise<CachedBlock>>();
  private _leafCache = new Map<string, Directory>();
  private _coldBuf: Uint8Array | null = null;

  private constructor(src: ByteSource) {
    // Wrap once: when a debug sink is registered, each uncached read emits a
    // timed event. The wrapper itself is one async hop; if the sink is null
    // we skip the performance.now() calls entirely.
    this._src = {
      async read(offset, length) {
        if (!debugSink()) return src.read(offset, length);
        const t0 = performance.now();
        const buf = await src.read(offset, length);
        emitDebug({
          kind: "read",
          offset,
          length: buf.length,
          ms: performance.now() - t0,
        });
        return buf;
      },
    };
  }

  /** Open and parse a WMTiles file from a URL, bytes, or custom byte source. */
  static async open(source: WMTSource, options?: OpenOptions): Promise<WMT> {
    const w = new WMT(toByteSource(source, options));
    await w._open();
    return w;
  }

  // ---- Inspection ----

  get tileSize(): number {
    return this._tileSize;
  }
  get bbox(): BBox {
    return {
      west: this._header.bboxLonMin,
      south: this._header.bboxLatMin,
      east: this._header.bboxLonMax,
      north: this._header.bboxLatMax,
    };
  }
  get zoomRange(): ZoomRange {
    return { min: this._header.minZoom, max: this._header.maxZoom };
  }
  get snapshotGeneration(): number {
    return this._header.snapshotGeneration;
  }
  get creationTime(): Date {
    return new Date(this._snapshot.creationTimeMs);
  }
  get referenceTime(): Date {
    return new Date(this._snapshot.referenceTimeMs);
  }
  get variables(): ReadonlyArray<Variable> {
    return this._variables;
  }
  get timeStepCount(): number {
    return this._timeCatalog.count;
  }
  get timeAxis(): TimeAxis {
    if (this._timeAxis) return this._timeAxis;
    const tc = this._timeCatalog;
    this._timeAxis = tc.regular
      ? {
          kind: "regular",
          start: new Date(tc.startMs),
          intervalMs: tc.intervalMs,
          count: tc.count,
        }
      : {
          kind: "irregular",
          times: tc.timestampsMs.map((ms) => new Date(ms)),
        };
    return this._timeAxis;
  }

  // ---- Lookups ----

  /** Variable handle by name. Throws UnknownVariableError if absent. */
  variable(name: string): Variable {
    const v = this._byName.get(name);
    if (!v) throw new UnknownVariableError(name);
    return v;
  }

  findVariable(name: string): Variable | undefined {
    return this._byName.get(name);
  }

  /** Wall-clock time at the given step index. Throws if out of range. */
  timeAt(index: number): Date {
    if (!Number.isInteger(index) || index < 0 || index >= this._timeCatalog.count) {
      throw new TimeOutOfRangeError(
        `time index ${index} out of range [0, ${this._timeCatalog.count})`,
      );
    }
    const tc = this._timeCatalog;
    const ms = tc.regular
      ? tc.startMs + index * tc.intervalMs
      : tc.timestampsMs[index];
    return new Date(ms);
  }

  /**
   * Resolve a TimeRef to its step index. Numbers are treated as indices;
   * Dates must match a step exactly. Throws TimeOutOfRangeError otherwise.
   */
  timeIndexOf(ref: TimeRef): number {
    const tc = this._timeCatalog;
    if (typeof ref === "number") {
      if (!Number.isInteger(ref) || ref < 0 || ref >= tc.count) {
        throw new TimeOutOfRangeError(
          `time index ${ref} out of range [0, ${tc.count})`,
        );
      }
      return ref;
    }
    const ms = ref.getTime();
    if (!Number.isFinite(ms)) {
      throw new TimeOutOfRangeError("time is not a valid Date");
    }
    if (tc.regular) {
      const off = ms - tc.startMs;
      if (tc.intervalMs === 0) {
        if (off === 0) return 0;
        throw new TimeOutOfRangeError(
          `${ref.toISOString()} does not match the single time step`,
        );
      }
      if (off < 0 || off % tc.intervalMs !== 0) {
        throw new TimeOutOfRangeError(
          `${ref.toISOString()} does not align with the regular time grid`,
        );
      }
      const idx = off / tc.intervalMs;
      if (idx >= tc.count) {
        throw new TimeOutOfRangeError(
          `${ref.toISOString()} is past the last step (${tc.count - 1})`,
        );
      }
      return idx;
    }
    // irregular: binary search on sorted ms array
    const arr = tc.timestampsMs;
    let lo = 0;
    let hi = arr.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (arr[mid] < ms) lo = mid + 1;
      else hi = mid;
    }
    if (lo < arr.length && arr[lo] === ms) return lo;
    throw new TimeOutOfRangeError(
      `${ref.toISOString()} does not match any time step`,
    );
  }

  /**
   * Sample one or more variables at a point across the time axis. Returns a
   * Float32Array per variable; NaN marks slots where the point falls outside
   * the file, the tile is missing, or the pixel is NoData.
   */
  async forecast(req: ForecastRequest): Promise<ForecastResult> {
    // Resolve names eagerly so UnknownVariableError fires before any I/O.
    const vars = req.variables.map((name) => this.variable(name));

    const startIdx = req.timeRange?.start !== undefined
      ? this.timeIndexOf(req.timeRange.start)
      : 0;
    const endIdx = req.timeRange?.end !== undefined
      ? this.timeIndexOf(req.timeRange.end)
      : this._timeCatalog.count - 1;
    if (startIdx > endIdx) {
      throw new TimeOutOfRangeError(
        `forecast start index ${startIdx} is after end index ${endIdx}`,
      );
    }
    const T = endIdx - startIdx + 1;
    const z = req.z ?? this._header.maxZoom;

    const times: Date[] = new Array(T);
    for (let i = 0; i < T; i++) times[i] = this.timeAt(startIdx + i);

    const values: Record<string, Float32Array> = {};
    for (const v of vars) values[v.name] = nanArray(T);

    const jobs: Promise<void>[] = [];
    for (const v of vars) {
      const series = values[v.name];
      for (let i = 0; i < T; i++) {
        const slot = i;
        jobs.push(
          this._sample(v.id, startIdx + slot, req.lat, req.lon, z).then((val) => {
            // null = out-of-range/missing → keep the pre-filled NaN.
            if (val !== null) series[slot] = val;
          }),
        );
      }
    }
    await Promise.all(jobs);

    return { times, values };
  }

  /**
   * Snapshot of several variables at one point at one time. The dual of
   * forecast(): same point, single time step, many variables. NaN = missing.
   */
  async value(req: ValueRequest): Promise<ValueResult> {
    const vars = req.variables.map((name) => this.variable(name));
    const t = this.timeIndexOf(req.time);
    const z = req.z ?? this._header.maxZoom;

    const values: Record<string, number> = {};
    await Promise.all(
      vars.map(async (v) => {
        const val = await this._sample(v.id, t, req.lat, req.lon, z);
        // null = out-of-bbox / out-of-range zoom / missing tile → NaN.
        values[v.name] = val ?? NaN;
      }),
    );

    return { time: this.timeAt(t), values };
  }

  // ---- Internal: fetch helpers (called by Variable) ----

  private async _fetchTile(
    varId: number,
    t: number,
    z: number,
    x: number,
    y: number,
  ): Promise<Float32Array | null> {
    const n = 1 << z;
    if (z < this._header.minZoom || z > this._header.maxZoom) return null;
    if (x < 0 || x >= n || y < 0 || y >= n) return null;

    const blk = await this._lookupBlock(varId, t);
    if (!blk) return null;

    const tid = encode3D(z, x, y);
    const { header: blkHdr, root } = await this._loadBlockHeader(
      blk.blockOffset,
      blk.blockLength,
    );

    let entry = findTile(root, tid);
    if (!entry) return null;
    if (entry.isLeaf) {
      const leaf = await this._loadBlockLeaf(
        blk.blockOffset,
        blkHdr,
        entry.offset,
        entry.length,
      );
      entry = findTile(leaf, tid);
      if (!entry || entry.isLeaf) return null;
    }
    const log = debugSink();
    const tStart = log ? performance.now() : 0;
    const blob = await this._src.read(
      blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
      entry.length,
    );
    const tDecode = log ? performance.now() : 0;
    const decoded = decodeCodec(blob, blk.dtype, this._nPixels);
    const tDequant = log ? performance.now() : 0;
    const result = dequantize(decoded, blk, this._nPixels);
    if (log) {
      const tEnd = performance.now();
      emitDebug({
        kind: "tile",
        varId,
        t,
        z,
        x,
        y,
        codec: blob[0],
        dtype: blk.dtype,
        compressedBytes: blob.length,
        networkMs: tDecode - tStart,
        decodeMs: tDequant - tDecode,
        dequantizeMs: tEnd - tDequant,
        totalMs: tEnd - tStart,
      });
    }
    return result;
  }

  private async _fetchTiles(
    varId: number,
    t: number,
    coords: readonly TileCoord[],
    opts: CoalesceOptions = {},
  ): Promise<Float32Array[]> {
    const log = debugSink();
    const tStart = log ? performance.now() : 0;
    const maxGap = opts.maxGapBytes ?? 64 * 1024;
    const maxReq = opts.maxRequestBytes ?? 4 * 1024 * 1024;

    const out: Float32Array[] = new Array(coords.length);
    if (coords.length === 0) return out;

    const blk = await this._lookupBlock(varId, t);
    if (!blk) {
      for (let i = 0; i < coords.length; i++) {
        out[i] = nanArray(this._nPixels);
      }
      if (log) {
        emitDebug({
          kind: "tiles",
          varId,
          t,
          coordCount: coords.length,
          hitCount: 0,
          groupCount: 0,
          bytesFetched: 0,
          cpuMs: 0,
          totalMs: performance.now() - tStart,
        });
      }
      return out;
    }
    const { header: blkHdr, root } = await this._loadBlockHeader(
      blk.blockOffset,
      blk.blockLength,
    );

    type Need = { i: number; fileOff: number; length: number };
    const needs: Need[] = [];
    for (let i = 0; i < coords.length; i++) {
      const c = coords[i];
      const n = 1 << c.z;
      if (
        c.z < this._header.minZoom ||
        c.z > this._header.maxZoom ||
        c.x < 0 || c.x >= n || c.y < 0 || c.y >= n
      ) {
        out[i] = nanArray(this._nPixels);
        continue;
      }
      const tid = encode3D(c.z, c.x, c.y);
      let entry = findTile(root, tid);
      if (!entry) {
        out[i] = nanArray(this._nPixels);
        continue;
      }
      if (entry.isLeaf) {
        const leaf = await this._loadBlockLeaf(
          blk.blockOffset,
          blkHdr,
          entry.offset,
          entry.length,
        );
        entry = findTile(leaf, tid);
        if (!entry || entry.isLeaf) {
          out[i] = nanArray(this._nPixels);
          continue;
        }
      }
      needs.push({
        i,
        fileOff: blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
        length: entry.length,
      });
    }
    if (needs.length === 0) {
      if (log) {
        emitDebug({
          kind: "tiles",
          varId,
          t,
          coordCount: coords.length,
          hitCount: 0,
          groupCount: 0,
          bytesFetched: 0,
          cpuMs: 0,
          totalMs: performance.now() - tStart,
        });
      }
      return out;
    }

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

    let bytesFetched = 0;
    let cpuMs = 0;
    for (const g of groups) {
      const tFetch0 = log ? performance.now() : 0;
      const buf = await this._src.read(g.start, g.end - g.start);
      const fetchMs = log ? performance.now() - tFetch0 : 0;
      bytesFetched += buf.length;
      // attribute network time to each tile proportionally by bytes
      let groupBytes = 0;
      if (log) for (const m of g.members) groupBytes += m.length;
      const tCpu0 = log ? performance.now() : 0;
      for (const m of g.members) {
        const localOff = m.fileOff - g.start;
        const blob = buf.subarray(localOff, localOff + m.length);
        const tDecode0 = log ? performance.now() : 0;
        const decoded = decodeCodec(blob, blk.dtype, this._nPixels);
        const tDequant0 = log ? performance.now() : 0;
        out[m.i] = dequantize(decoded, blk, this._nPixels);
        if (log) {
          const tDequant1 = performance.now();
          const c = coords[m.i];
          emitDebug({
            kind: "tile",
            varId,
            t,
            z: c.z,
            x: c.x,
            y: c.y,
            codec: blob[0],
            dtype: blk.dtype,
            compressedBytes: m.length,
            networkMs: groupBytes > 0 ? (fetchMs * m.length) / groupBytes : 0,
            decodeMs: tDequant0 - tDecode0,
            dequantizeMs: tDequant1 - tDequant0,
            totalMs:
              (groupBytes > 0 ? (fetchMs * m.length) / groupBytes : 0) +
              (tDequant1 - tDecode0),
          });
        }
      }
      if (log) cpuMs += performance.now() - tCpu0;
    }
    if (log) {
      emitDebug({
        kind: "tiles",
        varId,
        t,
        coordCount: coords.length,
        hitCount: needs.length,
        groupCount: groups.length,
        bytesFetched,
        cpuMs,
        totalMs: performance.now() - tStart,
      });
    }
    return out;
  }

  private async _sample(
    varId: number,
    t: number,
    lat: number,
    lon: number,
    z: number,
  ): Promise<number | null> {
    if (z < this._header.minZoom || z > this._header.maxZoom) return null;
    const px = latLonToTilePixel(z, lat, lon, this._tileSize);
    if (!px) return null;
    const tile = await this._fetchTile(varId, t, z, px.x, px.y);
    if (!tile) return null;
    return tile[px.row * this._tileSize + px.col];
  }

  // ---- Internal: open / loaders ----

  private async _open(): Promise<void> {
    const log = debugSink();
    const tStart = log ? performance.now() : 0;
    this._coldBuf = await this._src.read(0, COLD_START_BYTES);
    if (this._coldBuf.length < HEADER_SIZE) {
      throw new FormatError(
        `cold-start fetch returned ${this._coldBuf.length} B, need ≥${HEADER_SIZE}`,
      );
    }
    this._header = parseHeader(this._coldBuf.subarray(0, HEADER_SIZE));
    if (this._header.formatVersion !== 1) {
      throw new FormatError(
        `unsupported format version ${this._header.formatVersion}`,
      );
    }
    this._tileSize = 1 << this._header.tilePixelSizeLog2;
    this._nPixels = this._tileSize * this._tileSize;

    try {
      await this._loadSnapshot(
        this._header.activeSnapshotOffset,
        this._header.activeSnapshotLength,
      );
    } catch (err) {
      if (
        (this._header.flags & FLAG_HAS_PREVIOUS_SNAPSHOT) !== 0 &&
        this._header.previousSnapshotLength > 0 &&
        this._header.previousSnapshotOffset > 0
      ) {
        await this._loadSnapshot(
          this._header.previousSnapshotOffset,
          this._header.previousSnapshotLength,
        );
      } else {
        throw err;
      }
    }

    if (log) {
      emitDebug({ kind: "open", totalMs: performance.now() - tStart });
    }
  }

  private async _loadSnapshot(off: number, length: number): Promise<void> {
    const snapBuf = await this._readAbs(off, length);
    if (snapBuf.length < SNAPSHOT_TRAILER_SIZE + 128) {
      throw new FormatError(`snapshot too short (${snapBuf.length} B)`);
    }
    this._snapshot = parseSnapshotHeader(snapBuf.subarray(0, 128));
    const trailerOff = snapBuf.length - SNAPSHOT_TRAILER_SIZE;
    const trailer = parseSnapshotTrailer(snapBuf.subarray(trailerOff));
    if (trailer.snapshotTotalLength !== snapBuf.length) {
      throw new FormatError(
        `snapshot trailer total ${trailer.snapshotTotalLength} != buf ${snapBuf.length}`,
      );
    }
    const got = crc32c(snapBuf.subarray(0, trailerOff));
    if (got !== trailer.crc32c) {
      throw new FormatError(
        `snapshot CRC mismatch: stored=0x${trailer.crc32c.toString(16)} computed=0x${got.toString(16)}`,
      );
    }

    const sh = this._snapshot;
    const comp = this._header.internalCompression;

    const varBlob = snapBuf.subarray(
      sh.variableCatalogOff,
      sh.variableCatalogOff + sh.variableCatalogLen,
    );
    const varRaw = await decompressInternal(varBlob, comp);
    this._catalog = parseVariableCatalog(varRaw, sh.numVariables);
    this._variables = this._catalog.map(
      (v) =>
        new WMTVariable(
          (ref) => this.timeIndexOf(ref),
          (varId, t, z, x, y) => this._fetchTile(varId, t, z, x, y),
          (varId, t, coords, opts) =>
            this._fetchTiles(varId, t, coords, opts),
          (varId, t, lat, lon, z) => this._sample(varId, t, lat, lon, z),
          this._header.maxZoom,
          v.id,
          v.name,
          v.unit,
          v.colormap,
          { min: v.vmin, max: v.vmax },
          v.defaultPrecisionHint,
        ),
    );
    this._byName = new Map(this._variables.map((v) => [v.name, v]));

    const regular = (this._header.flags & FLAG_TIME_CATALOG_REGULAR) !== 0;
    let tcBlob = snapBuf.subarray(
      sh.timeCatalogOff,
      sh.timeCatalogOff + sh.timeCatalogLen,
    );
    if (!regular) tcBlob = await decompressInternal(tcBlob, comp);
    this._timeCatalog = parseTimeCatalog(tcBlob, regular);
    this._timeAxis = null;

    const rootBlob = snapBuf.subarray(
      sh.blockTableRootOff,
      sh.blockTableRootOff + sh.blockTableRootLen,
    );
    const rootRaw = await decompressInternal(rootBlob, comp);
    this._blockTableRoot = parseBlockTable(rootRaw);
    this._btLeavesAbs = off + sh.blockTableLeavesOff;
  }

  private async _lookupBlock(
    varId: number,
    timeID: number,
  ): Promise<BlockTableEntry | null> {
    let row = lookupBlockTable(this._blockTableRoot, varId, timeID);
    if (!row) return null;
    if (row.isLeafPointer) {
      const leaf = await this._loadBlockTableLeaf(
        row.blockOffset,
        row.blockLength,
      );
      row = lookupBlockTable(leaf, varId, timeID);
      if (!row || row.isLeafPointer) return null;
    }
    return row;
  }

  private async _readAbs(offset: number, length: number): Promise<Uint8Array> {
    if (length === 0) return new Uint8Array();
    if (this._coldBuf && offset + length <= this._coldBuf.length) {
      return this._coldBuf.subarray(offset, offset + length);
    }
    return await this._src.read(offset, length);
  }

  private async _loadBlockTableLeaf(
    leafOff: number,
    leafLen: number,
  ): Promise<BlockTableEntry[]> {
    const cached = this._btLeafCache.get(leafOff);
    if (cached) return cached;
    let inFlight = this._btLeafFetches.get(leafOff);
    if (!inFlight) {
      inFlight = (async () => {
        const raw = await this._readAbs(this._btLeavesAbs + leafOff, leafLen);
        const buf = await decompressInternal(
          raw,
          this._header.internalCompression,
        );
        const entries = parseBlockTable(buf);
        this._btLeafCache.set(leafOff, entries);
        this._btLeafFetches.delete(leafOff);
        return entries;
      })();
      this._btLeafFetches.set(leafOff, inFlight);
    }
    return await inFlight;
  }

  private async _loadBlockHeader(
    blockOff: number,
    blockLen: number,
  ): Promise<CachedBlock> {
    const cached = this._blockCache.get(blockOff);
    if (cached) return cached;
    let inFlight = this._blockFetches.get(blockOff);
    if (!inFlight) {
      inFlight = (async () => {
        const want = Math.min(BLOCK_HEADER_SIZE + MAX_BLOCK_ROOT, blockLen);
        const prefix = await this._readAbs(blockOff, want);
        const header = parseBlockHeader(prefix.subarray(0, BLOCK_HEADER_SIZE));
        const rootEnd = header.rootDirectoryOffset + header.rootDirectoryLength;
        let rootRaw: Uint8Array;
        if (rootEnd <= prefix.length) {
          rootRaw = prefix.subarray(header.rootDirectoryOffset, rootEnd);
        } else {
          rootRaw = await this._src.read(
            blockOff + header.rootDirectoryOffset,
            header.rootDirectoryLength,
          );
        }
        const rootBuf = await decompressInternal(
          rootRaw,
          this._header.internalCompression,
        );
        const root = parseDirectory(rootBuf);
        const out = { header, root };
        this._blockCache.set(blockOff, out);
        this._blockFetches.delete(blockOff);
        return out;
      })();
      this._blockFetches.set(blockOff, inFlight);
    }
    return await inFlight;
  }

  private async _loadBlockLeaf(
    blockOff: number,
    header: BlockHeader,
    leafOff: number,
    leafLen: number,
  ): Promise<Directory> {
    const cacheKey = `${blockOff}:${leafOff}`;
    const cached = this._leafCache.get(cacheKey);
    if (cached) return cached;
    const raw = await this._src.read(
      blockOff + header.leafDirectoriesOffset + leafOff,
      leafLen,
    );
    const buf = await decompressInternal(raw, this._header.internalCompression);
    const dir = parseDirectory(buf);
    this._leafCache.set(cacheKey, dir);
    return dir;
  }
}

function nanArray(n: number): Float32Array {
  return new Float32Array(n).fill(NaN);
}
