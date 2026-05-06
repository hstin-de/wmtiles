// High-level reader — what most users want.
export {
  WMT,
  open,
  httpSource,
  bytesSource,
  type ByteSource,
  type WMTSource,
  type OpenOptions,
  type BBox,
  type ZoomRange,
  type ValueRange,
  type Variable,
  type TimeRef,
  type TimeAxis,
  type TileCoord,
  type CoalesceOptions,
  type TileRequest,
  type TilesRequest,
  type SampleRequest,
} from "./reader.js";

// Typed errors.
export {
  WMTError,
  FormatError,
  SourceError,
  UnknownVariableError,
  TimeOutOfRangeError,
} from "./errors.js";

// Geo helper for point sampling UIs.
export { latLonToTilePixel, type TilePixel } from "./tileid.js";
