// Opt-in instrumentation. Zero-cost when no sink is registered: the only
// runtime overhead is a null-check at the emit sites. Once a sink is wired
// up the reader records phase timings via performance.now() and feeds events
// here for the consumer to log, aggregate, or render.

export interface ReadEvent {
  kind: "read";
  offset: number;
  length: number;
  ms: number;
}

export interface TileEvent {
  kind: "tile";
  varId: number;
  t: number;
  z: number;
  x: number;
  y: number;
  codec: number;
  dtype: number;
  compressedBytes: number;
  /** wall time minus decode minus dequantize — fetch + lookup overhead */
  networkMs: number;
  decodeMs: number;
  dequantizeMs: number;
  totalMs: number;
}

export interface TilesEvent {
  kind: "tiles";
  varId: number;
  t: number;
  coordCount: number;
  /** coords that resolved to actual tile blobs (rest were missing/out-of-range) */
  hitCount: number;
  groupCount: number;
  bytesFetched: number;
  cpuMs: number;
  totalMs: number;
}

export interface OpenEvent {
  kind: "open";
  totalMs: number;
}

export type DebugEvent = ReadEvent | TileEvent | TilesEvent | OpenEvent;

export type DebugSink = (e: DebugEvent) => void;

let _sink: DebugSink | null = null;

/** Wire (or unwire) a consumer for debug events. Pass null to disable. */
export function setDebugSink(sink: DebugSink | null): void {
  _sink = sink;
}

/** @internal */
export function debugSink(): DebugSink | null {
  return _sink;
}

/** @internal */
export function emitDebug(e: DebugEvent): void {
  const s = _sink;
  if (s) s(e);
}
