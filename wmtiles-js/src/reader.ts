import {
  BLOCK_FLAG_RAW_GRID,
  BLOCK_HEADER_SIZE,
  COLD_START_BYTES,
  FLAG_HAS_PREVIOUS_SNAPSHOT,
  FLAG_TIME_CATALOG_REGULAR,
  HEADER_SIZE,
  SNAPSHOT_TRAILER_SIZE,
  crc32c,
  decompressInternal,
  findTile,
  lookupBlockTable,
  parseBlockHeader,
  parseBlockTable,
  parseDirectory,
  parseHeader,
  parseRawGridSection,
  parseSnapshotHeader,
  parseSnapshotTrailer,
  parseTimeCatalog,
  parseVariableCatalog,
  rawGridChunkHeight,
  rawGridChunkWidth,
  rawGridCoarseCountX,
  rawGridCoarseCellExtent,
  rawGridCoarseIndexOf,
  parseFineIndex,
  type BlockHeader,
  type BlockTableEntry,
  type DirEntry,
  type Directory,
  type FineIndex,
  type Header,
  type RawGridSection,
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
import {
  WMTLayerImpl,
  type ArrowsLayerOptions,
  type ArrowsRendererState,
  type HeatmapLayerOptions,
  type HeatmapRendererState,
  type IsobarLayerOptions,
  type IsobarRendererState,
  type ParticlesLayerOptions,
  type ParticlesRendererState,
  type SymbolLayerOptions,
  type SymbolRendererState,
  type HatchLayerOptions,
  type HatchRendererState,
  type WMTArrowsLayer,
  type WMTHeatmapLayer,
  type WMTIsobarLayer,
  type WMTParticlesLayer,
  type WMTSymbolLayer,
  type WMTHatchLayer,
} from "./layers.js";

export interface ByteRange {
  offset: number;
  length: number;
}

export interface ByteSource {
  read(offset: number, length: number): Promise<Uint8Array>;
  // bulk fetch for disjoint ranges (HTTP multi-range). Result order = input order.
  readMulti?(ranges: readonly ByteRange[]): Promise<Uint8Array[]>;
  // null = not yet probed; true/false = known multipart support.
  isMultipartCapable?(): boolean | null;
}

export type WMTSource = ByteSource | Uint8Array | ArrayBuffer | string | URL;

export interface OpenOptions {
  requestInit?: RequestInit;
  // default 2 KB: fits layered raw-grid roots and most Hilbert roots; large
  // tile-pyramid roots tail-stitch (one extra RTT per such block, once).
  blockHeaderPrefetchBytes?: number;
  // sync; default is pure-JS fzstd (~150-250 MB/s). Override for native zstd.
  zstdDecompress?: (compressed: Uint8Array) => Uint8Array;
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
  /** Defaults to maxZoom of the file. Ignored for raw-grid blocks. */
  z?: number;
}

export interface SamplesRequest {
  time: TimeRef;
  points: ReadonlyArray<{ lat: number; lon: number }>;
  /** Defaults to maxZoom of the file. Ignored for raw-grid blocks. */
  z?: number;
  coalesce?: CoalesceOptions;
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


export function httpSource(url: string | URL, init?: RequestInit): ByteSource {
  const href = String(url);
  // Memoize once: once we know the origin won't honor multipart/byteranges,
  // stop wasting requests on it.
  let multipartSupported: boolean | null = null;

  async function readOne(offset: number, length: number): Promise<Uint8Array> {
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

    if (resp.status === 206) return trim(await body());
    if (resp.status === 200 && offset === 0) return trim(await body());
    if (resp.status === 200) {
      throw new SourceError(
        `HTTP server did not honor Range for bytes=${offset}-${offset + length - 1}`,
      );
    }
    throw new SourceError(
      `HTTP ${resp.status} fetching bytes=${offset}-${offset + length - 1}`,
    );
  }

  async function readMulti(
    ranges: readonly ByteRange[],
  ): Promise<Uint8Array[]> {
    if (ranges.length === 0) return [];
    if (ranges.length === 1) {
      return [await readOne(ranges[0].offset, ranges[0].length)];
    }

    const rangeStr = ranges
      .map((r) => `${r.offset}-${r.offset + r.length - 1}`)
      .join(",");
    const headers = new Headers(init?.headers);
    headers.set("Range", `bytes=${rangeStr}`);
    let resp: Response;
    try {
      resp = await fetch(href, { ...init, headers });
    } catch (err) {
      throw new SourceError(`HTTP multi-range fetch failed`, { cause: err });
    }

    if (resp.status !== 206) {
      throw new SourceError(`HTTP ${resp.status} on multi-range fetch`);
    }

    const ct = resp.headers.get("Content-Type") ?? "";
    if (ct.toLowerCase().startsWith("multipart/byteranges")) {
      const boundary = multipartBoundary(ct);
      if (!boundary) {
        throw new SourceError("multipart/byteranges without boundary");
      }
      const body = new Uint8Array(await resp.arrayBuffer());
      const parts = parseMultipartByteRanges(body, boundary, ranges.length);
      // RFC 7233: parts arrive in request order. Validate first part's
      // Content-Range as a sanity check against shuffled responses.
      if (parts.length !== ranges.length) {
        throw new SourceError(
          `multipart response had ${parts.length} parts, expected ${ranges.length}`,
        );
      }
      if (parts.length > 0 && parts[0].start !== ranges[0].offset) {
        throw new SourceError(
          `multipart first part starts at ${parts[0].start}, expected ${ranges[0].offset} (server reordered ranges?)`,
        );
      }
      const out: Uint8Array[] = new Array(ranges.length);
      for (let i = 0; i < ranges.length; i++) {
        const part = parts[i].data;
        out[i] = part.length > ranges[i].length
          ? part.subarray(0, ranges[i].length)
          : part;
      }
      return out;
    }

    // Server collapsed multi-range into a single 206 response. Bail out so the
    // caller can fall back to per-range fetches.
    throw new SourceError(
      `server returned single-range 206 (Content-Type=${ct}) instead of multipart/byteranges`,
    );
  }

  return {
    read: readOne,
    isMultipartCapable: () => multipartSupported,
    async readMulti(ranges) {
      if (ranges.length <= 1) {
        if (ranges.length === 0) return [];
        return [await readOne(ranges[0].offset, ranges[0].length)];
      }
      if (multipartSupported === false) {
        // Server already said no; fan out concurrently and let fetch/HTTP/2 sort it.
        return Promise.all(ranges.map((r) => readOne(r.offset, r.length)));
      }
      try {
        const out = await readMulti(ranges);
        multipartSupported = true;
        return out;
      } catch (err) {
        if (multipartSupported === null) {
          // First attempt failed: probably no multipart support. Note and fall back.
          multipartSupported = false;
          return Promise.all(ranges.map((r) => readOne(r.offset, r.length)));
        }
        throw err;
      }
    },
  };
}

function multipartBoundary(contentType: string): string | null {
  const m = contentType.match(/boundary=(?:"([^"]+)"|([^;]+))/i);
  if (!m) return null;
  return (m[1] ?? m[2]).trim();
}

interface MultipartPart {
  /** Parsed from Content-Range on the first part only; -1 for later parts. */
  start: number;
  data: Uint8Array;
}

function parseMultipartByteRanges(
  body: Uint8Array,
  boundary: string,
  expectedParts: number,
): MultipartPart[] {
  const dashBoundary = new TextEncoder().encode("--" + boundary);
  const parts: MultipartPart[] = expectedParts > 0 ? new Array(expectedParts) : [];
  let partIdx = 0;

  let i = indexOfBytes(body, dashBoundary, 0);
  while (i >= 0) {
    let after = i + dashBoundary.length;
    // Closing boundary (--boundary--): done.
    if (
      after + 1 < body.length &&
      body[after] === 0x2d &&
      body[after + 1] === 0x2d
    ) {
      break;
    }
    // Skip trailing CRLF after boundary line.
    if (
      after + 1 < body.length &&
      body[after] === 0x0d &&
      body[after + 1] === 0x0a
    ) {
      after += 2;
    }
    // Skip past headers: find \r\n\r\n. Only parse Content-Range for the first
    // part as a sanity check; trust request-order for the rest.
    const headersEnd = findCRLFCRLF(body, after);
    if (headersEnd < 0) break;
    let start = -1;
    if (partIdx === 0) {
      start = parseContentRangeStart(body, after, headersEnd);
      if (start < 0) {
        throw new SourceError("multipart part missing/invalid Content-Range");
      }
    }
    const bodyStart = headersEnd + 4;
    const nextBoundary = indexOfBytes(body, dashBoundary, bodyStart);
    if (nextBoundary < 0) break;
    // Trim the CRLF that precedes the next boundary.
    let bodyEnd = nextBoundary;
    if (
      bodyEnd >= 2 &&
      body[bodyEnd - 2] === 0x0d &&
      body[bodyEnd - 1] === 0x0a
    ) {
      bodyEnd -= 2;
    }
    const part: MultipartPart = { start, data: body.subarray(bodyStart, bodyEnd) };
    if (partIdx < parts.length) parts[partIdx] = part;
    else parts.push(part);
    partIdx++;
    i = nextBoundary;
  }
  if (partIdx < parts.length) parts.length = partIdx;
  return parts;
}

// byte-level scan, no TextDecoder: hundreds of headers per multipart body.
function parseContentRangeStart(
  body: Uint8Array,
  from: number,
  to: number,
): number {
  // Look for case-insensitive "content-range:" then "bytes" then a digit.
  const tag = "content-range:";
  outer: for (let i = from; i + tag.length < to; i++) {
    for (let j = 0; j < tag.length; j++) {
      const c = body[i + j];
      const lc = c >= 0x41 && c <= 0x5a ? c + 32 : c;
      if (lc !== tag.charCodeAt(j)) continue outer;
    }
    let k = i + tag.length;
    while (k < to && (body[k] === 0x20 || body[k] === 0x09)) k++;
    // "bytes"
    if (k + 5 > to) return -1;
    if (
      body[k] !== 0x62 && body[k] !== 0x42
    ) continue;
    k += 5;
    while (k < to && (body[k] === 0x20 || body[k] === 0x09)) k++;
    let n = 0;
    let any = false;
    while (k < to && body[k] >= 0x30 && body[k] <= 0x39) {
      n = n * 10 + (body[k] - 0x30);
      k++;
      any = true;
    }
    return any ? n : -1;
  }
  return -1;
}

function findCRLFCRLF(body: Uint8Array, from: number): number {
  const last = body.length - 4;
  for (let i = from; i <= last; i++) {
    if (
      body[i] === 0x0d &&
      body[i + 1] === 0x0a &&
      body[i + 2] === 0x0d &&
      body[i + 3] === 0x0a
    ) {
      return i;
    }
  }
  return -1;
}

// native indexOf as fast filter so the byte-loop only runs on candidates.
function indexOfBytes(
  haystack: Uint8Array,
  needle: Uint8Array,
  fromIdx: number,
): number {
  if (needle.length === 0) return fromIdx;
  const last = haystack.length - needle.length;
  const first = needle[0];
  const len = needle.length;
  let i = fromIdx;
  while (i <= last) {
    const found = haystack.indexOf(first, i);
    if (found < 0 || found > last) return -1;
    let match = true;
    for (let j = 1; j < len; j++) {
      if (haystack[found + j] !== needle[j]) {
        match = false;
        break;
      }
    }
    if (match) return found;
    i = found + 1;
  }
  return -1;
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

// merges same-microtask reads into shared range requests, sliced back per caller.
function coalescingSource(
  inner: ByteSource,
  options: { maxGapBytes?: number; maxRequestBytes?: number } = {},
): ByteSource {
  const maxGap = options.maxGapBytes ?? 256 * 1024;
  const maxReq = options.maxRequestBytes ?? 16 * 1024 * 1024;

  type Req = {
    off: number;
    len: number;
    resolve: (b: Uint8Array) => void;
    reject: (e: unknown) => void;
  };
  let queue: Req[] = [];
  let scheduled = false;

  function flush(): void {
    const batch = queue;
    queue = [];
    scheduled = false;
    if (batch.length === 0) return;
    if ((globalThis as { __WMT_COALESCE_LOG?: boolean }).__WMT_COALESCE_LOG) {
      // eslint-disable-next-line no-console
      console.log(`[coalesce] flush batch=${batch.length}`);
    }
    if (batch.length === 1) {
      const r = batch[0];
      inner.read(r.off, r.len).then(r.resolve, r.reject);
      return;
    }
    batch.sort((a, b) => a.off - b.off || a.len - b.len);

    // intentionally conservative: format is designed for small downloads,
    // wider merge trades that for round-trips on sparse-but-large files.
    type Run = { start: number; end: number; lo: number; hi: number };
    const runs: Run[] = [];
    let i = 0;
    while (i < batch.length) {
      let runEnd = batch[i].off + batch[i].len;
      const runStart = batch[i].off;
      let j = i;
      while (j + 1 < batch.length) {
        const nxt = batch[j + 1];
        const newEnd = Math.max(runEnd, nxt.off + nxt.len);
        if (newEnd - runStart > maxReq) break;
        if (nxt.off - runEnd > maxGap) break;
        runEnd = newEnd;
        j++;
      }
      runs.push({ start: runStart, end: runEnd, lo: i, hi: j });
      i = j + 1;
    }

    const distributeRun = (run: Run, buf: Uint8Array): void => {
      for (let k = run.lo; k <= run.hi; k++) {
        const r = batch[k];
        const start = r.off - run.start;
        // subarray clips silently if buf is short (server returned less),
        // matching httpSource semantics for ranges past EOF.
        r.resolve(buf.subarray(start, start + r.len));
      }
    };
    const failRun = (run: Run, err: unknown): void => {
      for (let k = run.lo; k <= run.hi; k++) batch[k].reject(err);
    };

    if (runs.length === 1 || !inner.readMulti) {
      for (const run of runs) {
        inner
          .read(run.start, run.end - run.start)
          .then((buf) => distributeRun(run, buf), (err) => failRun(run, err));
      }
      return;
    }

    // Multi-range path: one fetch for all disjoint runs.
    inner
      .readMulti(runs.map((r) => ({ offset: r.start, length: r.end - r.start })))
      .then(
        (bufs) => {
          for (let k = 0; k < runs.length; k++) distributeRun(runs[k], bufs[k]);
        },
        (err) => {
          for (const run of runs) failRun(run, err);
        },
      );
  }

  return {
    read(offset, length) {
      if (length === 0) return Promise.resolve(new Uint8Array());
      return new Promise<Uint8Array>((resolve, reject) => {
        queue.push({ off: offset, len: length, resolve, reject });
        if (!scheduled) {
          scheduled = true;
          queueMicrotask(flush);
        }
      });
    },
    isMultipartCapable: inner.isMultipartCapable
      ? () => inner.isMultipartCapable!()
      : undefined,
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

type SamplesBatch = (
  varId: number,
  t: number,
  points: ReadonlyArray<{ lat: number; lon: number }>,
  z: number,
  opts?: CoalesceOptions,
) => Promise<Float32Array>;

type RawGridCheck = (varId: number, t: number) => Promise<boolean>;

type RawGridSectionGetter = (
  varId: number,
  t: number,
) => Promise<RawGridSection | null>;

type SampleDetailFn = (
  varId: number,
  t: number,
  lat: number,
  lon: number,
) => Promise<SampleDetail | null>;

export interface SampleDetailNeighbour {
  /** Source-grid column (0..nx-1). */
  x: number;
  /** Source-grid row (0..ny-1). */
  y: number;
  /** Decoded value at (x, y). NaN means NoData. */
  value: number;
}

export interface SampleDetailChunk {
  cx: number;
  cy: number;
  index: number;
  /** Byte offset within the block's tile-data region. */
  offset: number;
  /** Byte length of the chunk payload. 0 means absent (all-NaN). */
  length: number;
  /** True when the chunk is recorded absent (length == 0). */
  absent: boolean;
}

export interface SampleDetail {
  /** Bilinear-interpolated value; NaN if any 2x2 neighbour is NaN or point is out of grid. */
  bilinear: number;
  /** Nearest-cell value; NaN if the nearest cell is NoData or the point is out of grid. */
  nearest: number;
  /** Fractional source-pixel coordinates (lon → gx, lat → gy). */
  gx: number;
  gy: number;
  /** The four bilinear neighbours in (x0,y0), (x1,y0), (x0,y1), (x1,y1) order. */
  neighbours: [
    SampleDetailNeighbour,
    SampleDetailNeighbour,
    SampleDetailNeighbour,
    SampleDetailNeighbour,
  ];
  /** Unique chunks the four neighbours map to. */
  chunks: SampleDetailChunk[];
}

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
  tiles(req: TilesRequest): Promise<Float32Array[]>;
  /** One value at (lat, lon). For raw-grid blocks this is a bilinear sample. */
  sample(req: SampleRequest): Promise<number | null>;
  /** Many values at (lat, lon). Chunk fetches are coalesced. */
  samples(req: SamplesRequest): Promise<Float32Array>;
  /** True when the (variable, time) block was encoded with --no-tiles. */
  isRawGrid(time: TimeRef): Promise<boolean>;
  /** Raw-grid descriptor; null for tile-pyramid blocks. */
  rawGridSection(time: TimeRef): Promise<RawGridSection | null>;
  /** Inspection helper: bilinear + nearest + 4 neighbours + chunk info. Null when the block is a tile pyramid. */
  sampleDetail(req: SampleRequest): Promise<SampleDetail | null>;
}

class WMTVariable implements Variable {
  /** @internal */
  constructor(
    private readonly _timeIndexOf: (ref: TimeRef) => number,
    private readonly _fetchTile: TileFetcher,
    private readonly _fetchTiles: TilesFetcher,
    private readonly _sample: Sampler,
    private readonly _samplesBatch: SamplesBatch,
    private readonly _isRawGrid: RawGridCheck,
    private readonly _rawGridSection: RawGridSectionGetter,
    private readonly _sampleDetail: SampleDetailFn,
    private readonly _maxZoom: number,
    readonly id: number,
    readonly name: string,
    readonly unit: string,
    readonly colormap: string,
    readonly range: ValueRange,
    readonly precisionHint: number,
  ) {}

  async tile(req: TileRequest): Promise<Float32Array | null> {
    const t = this._timeIndexOf(req.time);
    return this._fetchTile(this.id, t, req.z, req.x, req.y);
  }

  async tiles(req: TilesRequest): Promise<Float32Array[]> {
    const t = this._timeIndexOf(req.time);
    return this._fetchTiles(this.id, t, req.coords, req.coalesce);
  }

  async sample(req: SampleRequest): Promise<number | null> {
    const t = this._timeIndexOf(req.time);
    const z = req.z ?? this._maxZoom;
    return this._sample(this.id, t, req.lat, req.lon, z);
  }

  async samples(req: SamplesRequest): Promise<Float32Array> {
    const t = this._timeIndexOf(req.time);
    const z = req.z ?? this._maxZoom;
    return this._samplesBatch(this.id, t, req.points, z, req.coalesce);
  }

  async isRawGrid(time: TimeRef): Promise<boolean> {
    return this._isRawGrid(this.id, this._timeIndexOf(time));
  }

  async rawGridSection(time: TimeRef): Promise<RawGridSection | null> {
    return this._rawGridSection(this.id, this._timeIndexOf(time));
  }

  async sampleDetail(req: SampleRequest): Promise<SampleDetail | null> {
    const t = this._timeIndexOf(req.time);
    return this._sampleDetail(this.id, t, req.lat, req.lon);
  }
}


interface CachedBlock {
  header: BlockHeader;
  /** Hilbert directory; null when the block is a raw-grid block. */
  root: Directory | null;
  /** Raw-grid section; null when the block is a tile pyramid. */
  rawGrid: RawGridSection | null;
  /** Cached decoded chunk pixels (raw-grid only), keyed by chunk index. */
  chunkCache?: Map<number, Float32Array>;
  /** Cached parsed fine-indices (raw-grid only), keyed by coarse-cell index. */
  fineCache?: Map<number, FineIndex>;
  /** In-flight fine-index loads to dedup parallel callers. */
  fineFetches?: Map<number, Promise<FineIndex>>;
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
  private _leafFetches = new Map<string, Promise<Directory>>();
  private _coldBuf: Uint8Array | null = null;
  private _blockHeaderPrefetchBytes: number;
  private _zstdOverride?: (b: Uint8Array) => Uint8Array;

  private constructor(
    src: ByteSource,
    prefetchBytes: number,
    zstdOverride?: (b: Uint8Array) => Uint8Array,
  ) {
    const coalesced = coalescingSource(src);
    this._src = {
      async read(offset, length) {
        if (!debugSink()) return coalesced.read(offset, length);
        const t0 = performance.now();
        const buf = await coalesced.read(offset, length);
        emitDebug({
          kind: "read",
          offset,
          length: buf.length,
          ms: performance.now() - t0,
        });
        return buf;
      },
    };
    this._blockHeaderPrefetchBytes = prefetchBytes;
    this._zstdOverride = zstdOverride;
  }

  /** Open and parse a WMTiles file from a URL, bytes, or custom byte source. */
  static async open(source: WMTSource, options?: OpenOptions): Promise<WMT> {
    const prefetch =
      options?.blockHeaderPrefetchBytes !== undefined
        ? Math.max(BLOCK_HEADER_SIZE, options.blockHeaderPrefetchBytes)
        : 2 * 1024;
    const w = new WMT(
      toByteSource(source, options),
      prefetch,
      options?.zstdDecompress,
    );
    await w._open();
    return w;
  }

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

    // three explicit waves (headers, leaves, payloads) so the coalescer sees
    // one batch per phase regardless of path depth per (var, t).
    type Target = { name: string; slot: number; varId: number; t: number };
    const targets: Target[] = [];
    for (const v of vars) {
      for (let i = 0; i < T; i++) {
        targets.push({ name: v.name, slot: i, varId: v.id, t: startIdx + i });
      }
    }

    // wave 1: block headers
    type Resolved = { tgt: Target; blk: BlockTableEntry; cb: CachedBlock };
    const resolvedRaw = await Promise.all(
      targets.map(async (tgt): Promise<Resolved | null> => {
        const blk = await this._lookupBlock(tgt.varId, tgt.t);
        if (!blk) return null;
        const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
        return { tgt, blk, cb };
      }),
    );
    const resolved: Resolved[] = [];
    for (const r of resolvedRaw) if (r) resolved.push(r);

    // Classify and prep pixel coords / leaf descriptors.
    type TileItem = {
      res: Resolved;
      tid: bigint;
      pxRow: number;
      pxCol: number;
      entry: DirEntry | null;
      needsLeaf: boolean;
      leafOff: number;
      leafLen: number;
    };
    type RawItem = { res: Resolved };
    const tileItems: TileItem[] = [];
    const rawItems: RawItem[] = [];
    const zOutOfRange = z < this._header.minZoom || z > this._header.maxZoom;
    for (const r of resolved) {
      if (r.cb.rawGrid) {
        rawItems.push({ res: r });
        continue;
      }
      if (zOutOfRange || !r.cb.root) continue;
      const px = latLonToTilePixel(z, req.lat, req.lon, this._tileSize);
      if (!px) continue;
      const tid = encode3D(z, px.x, px.y);
      const entry = findTile(r.cb.root, tid);
      if (!entry) continue;
      tileItems.push({
        res: r,
        tid,
        pxRow: px.row,
        pxCol: px.col,
        entry,
        needsLeaf: entry.isLeaf,
        leafOff: entry.isLeaf ? entry.offset : 0,
        leafLen: entry.isLeaf ? entry.length : 0,
      });
    }

    // wave 2: dedup leaves per (block, leafOff)
    const leafJobs = new Map<string, Promise<Directory>>();
    for (const it of tileItems) {
      if (!it.needsLeaf) continue;
      const k = `${it.res.blk.blockOffset}:${it.leafOff}`;
      if (!leafJobs.has(k)) {
        leafJobs.set(
          k,
          this._loadBlockLeaf(
            it.res.blk.blockOffset,
            it.res.cb.header,
            it.leafOff,
            it.leafLen,
          ),
        );
      }
    }
    if (leafJobs.size > 0) {
      const keys = [...leafJobs.keys()];
      const dirs = await Promise.all(keys.map((k) => leafJobs.get(k)!));
      const dirByKey = new Map<string, Directory>();
      for (let k = 0; k < keys.length; k++) dirByKey.set(keys[k], dirs[k]);
      for (const it of tileItems) {
        if (!it.needsLeaf) continue;
        const k = `${it.res.blk.blockOffset}:${it.leafOff}`;
        const e = findTile(dirByKey.get(k)!, it.tid);
        it.entry = !e || e.isLeaf ? null : e;
      }
    }

    // wave 3: tile blobs + raw chunks in one microtask
    const ts = this._tileSize;
    const np = this._nPixels;
    const finalJobs: Promise<void>[] = [];
    for (const it of tileItems) {
      if (!it.entry || it.entry.isLeaf) continue;
      const blk = it.res.blk;
      const blkHdr = it.res.cb.header;
      const entry = it.entry;
      const series = values[it.res.tgt.name];
      const slot = it.res.tgt.slot;
      const pxRow = it.pxRow;
      const pxCol = it.pxCol;
      finalJobs.push(
        this._src
          .read(
            blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
            entry.length,
          )
          .then((blob) => {
            const decoded = decodeCodec(blob, blk.dtype, np);
            const pixels = dequantize(decoded, blk, np);
            const v = pixels[pxRow * ts + pxCol];
            if (!Number.isNaN(v)) series[slot] = v;
          }),
      );
    }
    for (const it of rawItems) {
      const series = values[it.res.tgt.name];
      const slot = it.res.tgt.slot;
      finalJobs.push(
        this._sampleRaw(it.res.blk, it.res.cb, req.lat, req.lon).then((v) => {
          if (!Number.isNaN(v)) series[slot] = v;
        }),
      );
    }
    await Promise.all(finalJobs);

    return { times, values };
  }

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

  createHeatmapLayer(options?: HeatmapLayerOptions): WMTHeatmapLayer {
    return new WMTLayerImpl<HeatmapRendererState>(this, "heatmap", options);
  }

  createParticlesLayer(options?: ParticlesLayerOptions): WMTParticlesLayer {
    return new WMTLayerImpl<ParticlesRendererState>(this, "particles", options);
  }

  createIsobarLayer(options?: IsobarLayerOptions): WMTIsobarLayer {
    return new WMTLayerImpl<IsobarRendererState>(this, "isobar", options);
  }

  createArrowsLayer(options?: ArrowsLayerOptions): WMTArrowsLayer {
    return new WMTLayerImpl<ArrowsRendererState>(this, "arrows", options);
  }

  createSymbolLayer(options?: SymbolLayerOptions): WMTSymbolLayer {
    return new WMTLayerImpl<SymbolRendererState>(this, "symbol", options);
  }

  createHatchLayer(options?: HatchLayerOptions): WMTHatchLayer {
    return new WMTLayerImpl<HatchRendererState>(this, "hatch", options);
  }

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
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    if (cb.rawGrid) {
      // Raw-grid blocks have no Hilbert directory; callers should use sample().
      return null;
    }
    const { header: blkHdr, root } = cb;
    if (!root) return null;

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
    const cbAll = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    if (cbAll.rawGrid) {
      for (let i = 0; i < coords.length; i++) {
        out[i] = nanArray(this._nPixels);
      }
      return out;
    }
    const { header: blkHdr, root } = cbAll;
    if (!root) {
      for (let i = 0; i < coords.length; i++) {
        out[i] = nanArray(this._nPixels);
      }
      return out;
    }

    type Need = { i: number; fileOff: number; length: number };
    const needs: Need[] = [];
    // Pass 1: resolve root entries, collect unique leaf descriptors. Avoids
    // serializing one HTTP round-trip per coord that lands in a leaf.
    type Pending = { i: number; tid: bigint; leafOff: number; leafLen: number };
    const pending: Pending[] = [];
    const leafJobs = new Map<number, Promise<Directory>>();
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
      const entry = findTile(root, tid);
      if (!entry) {
        out[i] = nanArray(this._nPixels);
        continue;
      }
      if (entry.isLeaf) {
        if (!leafJobs.has(entry.offset)) {
          leafJobs.set(
            entry.offset,
            this._loadBlockLeaf(
              blk.blockOffset,
              blkHdr,
              entry.offset,
              entry.length,
            ),
          );
        }
        pending.push({ i, tid, leafOff: entry.offset, leafLen: entry.length });
        continue;
      }
      needs.push({
        i,
        fileOff: blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
        length: entry.length,
      });
    }
    // Pass 2: await all leaves in one wave, then resolve their entries.
    if (leafJobs.size > 0) {
      const leaves = new Map<number, Directory>();
      const entries = [...leafJobs.entries()];
      const dirs = await Promise.all(entries.map(([, p]) => p));
      for (let k = 0; k < entries.length; k++) leaves.set(entries[k][0], dirs[k]);
      for (const p of pending) {
        const leaf = leaves.get(p.leafOff)!;
        const entry = findTile(leaf, p.tid);
        if (!entry || entry.isLeaf) {
          out[p.i] = nanArray(this._nPixels);
          continue;
        }
        needs.push({
          i: p.i,
          fileOff: blk.blockOffset + blkHdr.tileDataOffset + entry.offset,
          length: entry.length,
        });
      }
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
    // Dispatch every group concurrently. The global coalescer can then merge
    // disjoint groups into one multipart fetch instead of serializing waves.
    await Promise.all(
      groups.map(async (g) => {
        const tFetch0 = log ? performance.now() : 0;
        const buf = await this._src.read(g.start, g.end - g.start);
        const fetchMs = log ? performance.now() - tFetch0 : 0;
        bytesFetched += buf.length;
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
      }),
    );
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
    // Raw-grid blocks ignore z and bilinearly sample from the source grid.
    const blk = await this._lookupBlock(varId, t);
    if (!blk) return null;
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    if (cb.rawGrid) {
      return await this._sampleRaw(blk, cb, lat, lon);
    }
    if (z < this._header.minZoom || z > this._header.maxZoom) return null;
    const px = latLonToTilePixel(z, lat, lon, this._tileSize);
    if (!px) return null;
    const tile = await this._fetchTile(varId, t, z, px.x, px.y);
    if (!tile) return null;
    return tile[px.row * this._tileSize + px.col];
  }

  private async _isRawGrid(varId: number, t: number): Promise<boolean> {
    const blk = await this._lookupBlock(varId, t);
    if (!blk) return false;
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    return cb.rawGrid !== null;
  }

  private async _rawGridSectionOf(
    varId: number,
    t: number,
  ): Promise<RawGridSection | null> {
    const blk = await this._lookupBlock(varId, t);
    if (!blk) return null;
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    return cb.rawGrid;
  }

  private async _sampleDetail(
    varId: number,
    t: number,
    lat: number,
    lon: number,
  ): Promise<SampleDetail | null> {
    const blk = await this._lookupBlock(varId, t);
    if (!blk) return null;
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    if (!cb.rawGrid) return null;
    const g = cb.rawGrid;

    const cs = 1 << g.chunkSizeLog2;
    const gx = g.dx === 0 ? NaN : (lon - g.lon0) / g.dx;
    const gy = g.dy === 0 ? NaN : (lat - g.lat0) / g.dy;
    const inGrid =
      Number.isFinite(gx) &&
      Number.isFinite(gy) &&
      gx >= 0 &&
      gy >= 0 &&
      gx <= g.nx - 1 &&
      gy <= g.ny - 1;

    const empty: SampleDetail = {
      bilinear: NaN,
      nearest: NaN,
      gx,
      gy,
      neighbours: [
        { x: 0, y: 0, value: NaN },
        { x: 0, y: 0, value: NaN },
        { x: 0, y: 0, value: NaN },
        { x: 0, y: 0, value: NaN },
      ],
      chunks: [],
    };
    if (!inGrid) return empty;

    const x0 = Math.floor(gx);
    const y0 = Math.floor(gy);
    const x1 = x0 + 1 > g.nx - 1 ? x0 : x0 + 1;
    const y1 = y0 + 1 > g.ny - 1 ? y0 : y0 + 1;

    const need = new Set<number>();
    for (const x of [x0, x1]) {
      for (const y of [y0, y1]) {
        const cx = Math.floor(x / cs);
        const cy = Math.floor(y / cs);
        need.add(cy * g.chunkCountX + cx);
      }
    }
    await this._ensureRawChunks(blk, cb, need);

    const cache = cb.chunkCache!;
    const pixelAt = (x: number, y: number): number => {
      const cx = Math.floor(x / cs);
      const cy = Math.floor(y / cs);
      const pixels = cache.get(cy * g.chunkCountX + cx);
      if (!pixels) return NaN;
      const w = rawGridChunkWidth(g, cx);
      return pixels[(y - cy * cs) * w + (x - cx * cs)];
    };

    const v00 = pixelAt(x0, y0);
    const v10 = pixelAt(x1, y0);
    const v01 = pixelAt(x0, y1);
    const v11 = pixelAt(x1, y1);
    const wx = gx - x0;
    const wy = gy - y0;

    let bilinear: number;
    if (
      Number.isNaN(v00) ||
      Number.isNaN(v10) ||
      Number.isNaN(v01) ||
      Number.isNaN(v11)
    ) {
      bilinear = NaN;
    } else {
      const a = v00 * (1 - wx) + v10 * wx;
      const b = v01 * (1 - wx) + v11 * wx;
      bilinear = a * (1 - wy) + b * wy;
    }

    const nx = Math.min(g.nx - 1, Math.max(0, Math.round(gx)));
    const ny = Math.min(g.ny - 1, Math.max(0, Math.round(gy)));
    const nearest = pixelAt(nx, ny);

    const seenChunks = new Set<number>();
    const chunks: SampleDetailChunk[] = [];
    for (const [x, y] of [
      [x0, y0],
      [x1, y0],
      [x0, y1],
      [x1, y1],
    ]) {
      const cx = Math.floor(x / cs);
      const cy = Math.floor(y / cs);
      const idx = cy * g.chunkCountX + cx;
      if (seenChunks.has(idx)) continue;
      seenChunks.add(idx);
      const loc = this._chunkLocationCached(cb, cx, cy);
      const length = loc?.ln ?? 0;
      const offset = loc?.off ?? 0;
      chunks.push({
        cx,
        cy,
        index: idx,
        offset,
        length,
        absent: length === 0,
      });
    }

    return {
      bilinear,
      nearest,
      gx,
      gy,
      neighbours: [
        { x: x0, y: y0, value: v00 },
        { x: x1, y: y0, value: v10 },
        { x: x0, y: y1, value: v01 },
        { x: x1, y: y1, value: v11 },
      ],
      chunks,
    };
  }

  private async _sampleRaw(
    blk: BlockTableEntry,
    cb: CachedBlock,
    lat: number,
    lon: number,
  ): Promise<number> {
    const g = cb.rawGrid!;
    if (g.dx === 0 || g.dy === 0) return NaN;
    const gx = (lon - g.lon0) / g.dx;
    const gy = (lat - g.lat0) / g.dy;
    if (!Number.isFinite(gx) || !Number.isFinite(gy)) return NaN;
    if (gx < 0 || gy < 0) return NaN;
    if (gx > g.nx - 1 || gy > g.ny - 1) return NaN;

    let x0 = Math.floor(gx);
    let y0 = Math.floor(gy);
    let x1 = x0 + 1;
    let y1 = y0 + 1;
    if (x1 > g.nx - 1) x1 = x0;
    if (y1 > g.ny - 1) y1 = y0;
    const wx = gx - x0;
    const wy = gy - y0;

    const need = new Set<number>();
    const cs = 1 << g.chunkSizeLog2;
    for (const x of [x0, x1]) {
      for (const y of [y0, y1]) {
        const cx = Math.floor(x / cs);
        const cy = Math.floor(y / cs);
        need.add(cy * g.chunkCountX + cx);
      }
    }
    await this._ensureRawChunks(blk, cb, need);

    const pixel = (x: number, y: number): number => {
      const cx = Math.floor(x / cs);
      const cy = Math.floor(y / cs);
      const idx = cy * g.chunkCountX + cx;
      const pixels = cb.chunkCache!.get(idx)!;
      const w = rawGridChunkWidth(g, cx);
      const row = y - cy * cs;
      const col = x - cx * cs;
      return pixels[row * w + col];
    };
    const v00 = pixel(x0, y0);
    const v10 = pixel(x1, y0);
    const v01 = pixel(x0, y1);
    const v11 = pixel(x1, y1);
    if (Number.isNaN(v00) || Number.isNaN(v10) || Number.isNaN(v01) || Number.isNaN(v11)) {
      return NaN;
    }
    const a = v00 * (1 - wx) + v10 * wx;
    const b = v01 * (1 - wx) + v11 * wx;
    return a * (1 - wy) + b * wy;
  }

  private async _samplesBatch(
    varId: number,
    t: number,
    points: ReadonlyArray<{ lat: number; lon: number }>,
    z: number,
    opts: CoalesceOptions = {},
  ): Promise<Float32Array> {
    const blk = await this._lookupBlock(varId, t);
    if (!blk) return new Float32Array(points.length).fill(NaN);
    const cb = await this._loadBlockHeader(blk.blockOffset, blk.blockLength);
    if (cb.rawGrid) {
      return this._sampleManyRaw(blk, cb, points, opts);
    }
    // map points to tile pixels, dedup tile coords, one coalesced fetch, read locally.
    const out = new Float32Array(points.length);
    if (z < this._header.minZoom || z > this._header.maxZoom) {
      return out.fill(NaN);
    }
    type Px = { tileIdx: number; col: number; row: number };
    const pxs: (Px | null)[] = new Array(points.length);
    const coordIdx = new Map<string, number>();
    const coords: TileCoord[] = [];
    for (let i = 0; i < points.length; i++) {
      const p = latLonToTilePixel(z, points[i].lat, points[i].lon, this._tileSize);
      if (!p) { pxs[i] = null; continue; }
      const key = `${p.x},${p.y}`;
      let tileIdx = coordIdx.get(key);
      if (tileIdx === undefined) {
        tileIdx = coords.length;
        coordIdx.set(key, tileIdx);
        coords.push({ z, x: p.x, y: p.y });
      }
      pxs[i] = { tileIdx, col: p.col, row: p.row };
    }
    if (coords.length === 0) return out.fill(NaN);
    const tiles = await this._fetchTiles(varId, t, coords, opts);
    const ts = this._tileSize;
    for (let i = 0; i < points.length; i++) {
      const p = pxs[i];
      if (!p) { out[i] = NaN; continue; }
      out[i] = tiles[p.tileIdx][p.row * ts + p.col];
    }
    return out;
  }

  private async _sampleManyRaw(
    blk: BlockTableEntry,
    cb: CachedBlock,
    points: ReadonlyArray<{ lat: number; lon: number }>,
    opts: CoalesceOptions,
  ): Promise<Float32Array> {
    const g = cb.rawGrid!;
    const cs = 1 << g.chunkSizeLog2;
    const need = new Set<number>();
    for (const p of points) {
      const gx = (p.lon - g.lon0) / g.dx;
      const gy = (p.lat - g.lat0) / g.dy;
      if (!Number.isFinite(gx) || !Number.isFinite(gy)) continue;
      if (gx < 0 || gy < 0 || gx > g.nx - 1 || gy > g.ny - 1) continue;
      let x0 = Math.floor(gx);
      let y0 = Math.floor(gy);
      let x1 = x0 + 1;
      let y1 = y0 + 1;
      if (x1 > g.nx - 1) x1 = x0;
      if (y1 > g.ny - 1) y1 = y0;
      for (const x of [x0, x1]) {
        for (const y of [y0, y1]) {
          const cx = Math.floor(x / cs);
          const cy = Math.floor(y / cs);
          need.add(cy * g.chunkCountX + cx);
        }
      }
    }
    await this._ensureRawChunks(blk, cb, need, opts);

    const out = new Float32Array(points.length);
    for (let i = 0; i < points.length; i++) {
      out[i] = await this._sampleRaw(blk, cb, points[i].lat, points[i].lon);
    }
    return out;
  }

  // cached + inflight-deduped; reads from the block's leafDirectories slot.
  private async _loadFineIndex(
    blk: BlockTableEntry,
    cb: CachedBlock,
    coarseIdx: number,
  ): Promise<FineIndex> {
    if (!cb.fineCache) cb.fineCache = new Map();
    const cached = cb.fineCache.get(coarseIdx);
    if (cached) return cached;
    if (!cb.fineFetches) cb.fineFetches = new Map();
    let inflight = cb.fineFetches.get(coarseIdx);
    if (!inflight) {
      inflight = (async () => {
        const g = cb.rawGrid!;
        const cce = g.coarseTable[coarseIdx];
        const coarseCountX = rawGridCoarseCountX(g);
        const coarseCx = coarseIdx % coarseCountX;
        const coarseCy = Math.floor(coarseIdx / coarseCountX);
        const { w: cellW, h: cellH } = rawGridCoarseCellExtent(g, coarseCx, coarseCy);
        const expected = cellW * cellH;
        let fi: FineIndex;
        if (cce.length === 0) {
          fi = {
            chunkOffsets: new Float64Array(expected),
            chunkLengths: new Float64Array(expected),
            cellW,
            cellH,
          };
        } else {
          const buf = await this._src.read(
            blk.blockOffset + (cb.header.leafDirectoriesOffset as number) + cce.offset,
            cce.length,
          );
          const parsed = parseFineIndex(buf, expected);
          fi = {
            chunkOffsets: parsed.chunkOffsets,
            chunkLengths: parsed.chunkLengths,
            cellW,
            cellH,
          };
        }
        cb.fineCache!.set(coarseIdx, fi);
        return fi;
      })();
      cb.fineFetches.set(coarseIdx, inflight);
      inflight.finally(() => cb.fineFetches!.delete(coarseIdx));
    }
    return inflight;
  }

  // callers must await _loadFineIndex for the relevant coarse cell first.
  private _chunkLocationCached(
    cb: CachedBlock,
    cx: number,
    cy: number,
  ): { off: number; ln: number } | null {
    const g = cb.rawGrid!;
    const { coarseIdx, localIdx } = rawGridCoarseIndexOf(g, cx, cy);
    const fi = cb.fineCache?.get(coarseIdx);
    if (!fi) return null;
    if (localIdx < 0 || localIdx >= fi.chunkOffsets.length) return null;
    return { off: fi.chunkOffsets[localIdx], ln: fi.chunkLengths[localIdx] };
  }

  private async _ensureRawChunks(
    blk: BlockTableEntry,
    cb: CachedBlock,
    needIdx: Set<number>,
    opts: CoalesceOptions = {},
  ): Promise<void> {
    const g = cb.rawGrid!;
    if (!cb.chunkCache) cb.chunkCache = new Map();
    const cache = cb.chunkCache;
    const missing: number[] = [];
    for (const i of needIdx) {
      if (!cache.has(i)) missing.push(i);
    }
    if (missing.length === 0) return;

    // Wave A: load fine-indices for every coarse cell touched by the missing
    // chunks, in parallel. Without this the per-chunk lookup below would have
    // to serialize one fine-index fetch at a time.
    const coarseNeeded = new Set<number>();
    for (const idx of missing) {
      const cy = Math.floor(idx / g.chunkCountX);
      const cx = idx - cy * g.chunkCountX;
      const { coarseIdx } = rawGridCoarseIndexOf(g, cx, cy);
      coarseNeeded.add(coarseIdx);
    }
    await Promise.all(
      [...coarseNeeded].map((cidx) => this._loadFineIndex(blk, cb, cidx)),
    );

    const maxGap = opts.maxGapBytes ?? 32 * 1024;
    const maxReq = opts.maxRequestBytes ?? 1024 * 1024;

    type R = { idx: number; off: number; ln: number; cx: number; cy: number };
    const ranges: R[] = [];
    for (const idx of missing) {
      const cy = Math.floor(idx / g.chunkCountX);
      const cx = idx - cy * g.chunkCountX;
      const loc = this._chunkLocationCached(cb, cx, cy);
      if (!loc) {
        // Defensive: fine-index should be cached at this point.
        throw new FormatError(`raw-grid: missing fine-index for chunk (${cx},${cy})`);
      }
      ranges.push({ idx, off: loc.off, ln: loc.ln, cx, cy });
    }
    ranges.sort((a, b) => a.off - b.off);

    // runs go out in parallel so the global coalescer can multipart them
    // alongside every other block's runs in the same flush.
    const jobs: Promise<void>[] = [];
    let i = 0;
    while (i < ranges.length) {
      let j = i;
      const runStart = ranges[i].off;
      let runEnd = ranges[i].off + ranges[i].ln;
      while (j + 1 < ranges.length) {
        const next = ranges[j + 1];
        if (next.ln === 0) {
          j++;
          continue;
        }
        const gap = next.off - runEnd;
        if (gap > maxGap) break;
        if (next.off + next.ln - runStart > maxReq) break;
        runEnd = next.off + next.ln;
        j++;
      }
      if (runEnd > runStart) {
        const lo = i;
        const hi = j;
        const start = runStart;
        const length = runEnd - runStart;
        jobs.push(
          this._src
            .read(
              blk.blockOffset + (cb.header.tileDataOffset as number) + start,
              length,
            )
            .then((buf) => {
              for (let k = lo; k <= hi; k++) {
                const rg = ranges[k];
                const w = rawGridChunkWidth(g, rg.cx);
                const h = rawGridChunkHeight(g, rg.cy);
                if (rg.ln === 0) {
                  cache.set(rg.idx, new Float32Array(w * h).fill(NaN));
                  continue;
                }
                const startInRun = rg.off - start;
                const blob = buf.subarray(startInRun, startInRun + rg.ln);
                const decoded = decodeCodec(blob, blk.dtype, w * h);
                const pixels = dequantize(decoded, blk, w * h);
                cache.set(rg.idx, pixels);
              }
            }),
        );
      } else {
        // All ranges in this run had ln === 0 (absent chunks). Cache NaN.
        for (let k = i; k <= j; k++) {
          const rg = ranges[k];
          const w = rawGridChunkWidth(g, rg.cx);
          const h = rawGridChunkHeight(g, rg.cy);
          cache.set(rg.idx, new Float32Array(w * h).fill(NaN));
        }
      }
      i = j + 1;
    }
    if (jobs.length > 0) await Promise.all(jobs);
  }

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
    const varRaw = await decompressInternal(varBlob, comp, this._zstdOverride);
    this._catalog = parseVariableCatalog(varRaw, sh.numVariables);
    this._variables = this._catalog.map(
      (v) =>
        new WMTVariable(
          (ref) => this.timeIndexOf(ref),
          (varId, t, z, x, y) => this._fetchTile(varId, t, z, x, y),
          (varId, t, coords, opts) =>
            this._fetchTiles(varId, t, coords, opts),
          (varId, t, lat, lon, z) => this._sample(varId, t, lat, lon, z),
          (varId, t, points, z, opts) =>
            this._samplesBatch(varId, t, points, z, opts),
          (varId, t) => this._isRawGrid(varId, t),
          (varId, t) => this._rawGridSectionOf(varId, t),
          (varId, t, lat, lon) => this._sampleDetail(varId, t, lat, lon),
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
    if (!regular) tcBlob = await decompressInternal(tcBlob, comp, this._zstdOverride);
    this._timeCatalog = parseTimeCatalog(tcBlob, regular);
    this._timeAxis = null;

    const rootBlob = snapBuf.subarray(
      sh.blockTableRootOff,
      sh.blockTableRootOff + sh.blockTableRootLen,
    );
    const rootRaw = await decompressInternal(rootBlob, comp, this._zstdOverride);
    this._blockTableRoot = parseBlockTable(rootRaw);
    this._btLeavesAbs = off + sh.blockTableLeavesOff;

    // pre-parse all block-table leaves from snapBuf (zero extra I/O) so
    // forecast() doesn't pay an extra RTT before block headers.
    if (sh.blockTableLeavesLen > 0) {
      const leavesAbs = sh.blockTableLeavesOff;
      const leavesEnd = leavesAbs + sh.blockTableLeavesLen;
      if (leavesEnd <= snapBuf.length) {
        const leafJobs: Promise<void>[] = [];
        for (const row of this._blockTableRoot) {
          if (!row.isLeafPointer) continue;
          const leafOff = row.blockOffset;
          const leafLen = row.blockLength;
          if (this._btLeafCache.has(leafOff)) continue;
          const leafBlob = snapBuf.subarray(
            leavesAbs + leafOff,
            leavesAbs + leafOff + leafLen,
          );
          leafJobs.push(
            decompressInternal(leafBlob, comp, this._zstdOverride).then((buf) => {
              this._btLeafCache.set(leafOff, parseBlockTable(buf));
            }),
          );
        }
        if (leafJobs.length > 0) await Promise.all(leafJobs);
      }
    }
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
          this._zstdOverride,
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
        const want = Math.min(this._blockHeaderPrefetchBytes, blockLen);
        const prefix = await this._readAbs(blockOff, want);
        const header = parseBlockHeader(prefix.subarray(0, BLOCK_HEADER_SIZE));
        const rootEnd = header.rootDirectoryOffset + header.rootDirectoryLength;
        let rootRaw: Uint8Array;
        if (rootEnd <= prefix.length) {
          // Root fits entirely in prefix; zero extra I/O.
          rootRaw = prefix.subarray(header.rootDirectoryOffset, rootEnd);
        } else {
          const headInPrefix = prefix.length - header.rootDirectoryOffset;
          if (headInPrefix <= 0) {
            rootRaw = await this._src.read(
              blockOff + header.rootDirectoryOffset,
              header.rootDirectoryLength,
            );
          } else {
            const tailLen = header.rootDirectoryLength - headInPrefix;
            const tail = await this._src.read(blockOff + prefix.length, tailLen);
            rootRaw = new Uint8Array(header.rootDirectoryLength);
            rootRaw.set(
              prefix.subarray(header.rootDirectoryOffset, prefix.length),
              0,
            );
            rootRaw.set(tail, headInPrefix);
          }
        }
        const rootBuf = await decompressInternal(
          rootRaw,
          this._header.internalCompression,
          this._zstdOverride,
        );
        let out: CachedBlock;
        if ((header.blockFlags & BLOCK_FLAG_RAW_GRID) !== 0) {
          const rawGrid = parseRawGridSection(rootBuf);
          out = { header, root: null, rawGrid };
        } else {
          const root = parseDirectory(rootBuf);
          out = { header, root, rawGrid: null };
        }
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
    let inflight = this._leafFetches.get(cacheKey);
    if (!inflight) {
      inflight = (async () => {
        const raw = await this._src.read(
          blockOff + header.leafDirectoriesOffset + leafOff,
          leafLen,
        );
        const buf = await decompressInternal(
          raw,
          this._header.internalCompression,
          this._zstdOverride,
        );
        const dir = parseDirectory(buf);
        this._leafCache.set(cacheKey, dir);
        return dir;
      })();
      this._leafFetches.set(cacheKey, inflight);
      inflight.finally(() => this._leafFetches.delete(cacheKey));
    }
    return inflight;
  }
}

function nanArray(n: number): Float32Array {
  return new Float32Array(n).fill(NaN);
}
