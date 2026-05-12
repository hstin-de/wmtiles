# wmtiles

Pure-TypeScript reader for the [WMTiles](https://github.com/hstin-de/wmtiles)
single-file format. Parses headers, snapshots, block tables; decodes per-tile
codecs; dequantizes per-block. No DOM dependency: works in browsers, Node,
Bun, Cloudflare Workers, anywhere with `fetch`.

## Install

```sh
npm install wmtiles
# or
bun add wmtiles
```

## Usage

```ts
import { open } from "wmtiles";

const wmt = await open("/data.wmt");

// Inspect
wmt.bbox;              // { west, south, east, north }
wmt.zoomRange;         // { min, max }
wmt.tileSize;          // 256 (pixels per tile side)
wmt.referenceTime;     // Date
wmt.variables;         // ReadonlyArray<Variable>
wmt.timeStepCount;     // 81
wmt.timeAxis;          // { kind: "regular", start, intervalMs, count } | { kind: "irregular", times }
```

There are two ways to pull data out of a file:

| You want… | Use | Returns |
|---|---|---|
| The value of a few variables at one (lat, lon, time) | [`wmt.value()`](#point-snapshot-wmtvalue) | scalars per variable |
| The time series of a few variables at one (lat, lon) | [`wmt.forecast()`](#point-time-series-wmtforecast) | `Float32Array` per variable |
| Raster pixels of a tile to render on a map | [`variable.tile()` / `tiles()`](#tile-rendering-for-maps) | `Float32Array` of pixels |

Prefer the point APIs (`value`, `forecast`) when you only need scalars. They handle variable lookup, time resolution, and missing-data NaN-filling in one call. The tile API is for map renderers that need full raster pixels per tile.

### Point snapshot: `wmt.value()`

Many variables at one point at one time. Useful for map-click tooltips showing "temperature + wind + precip right here, right now":

```ts
const snap = await wmt.value({
  lat: 52.52,
  lon: 13.405,
  time: 0,                              // step index or Date
  variables: ["dbzh", "temperature_2m"],
});

snap.time;            // Date, the resolved absolute time
snap.values.dbzh;     // number, NaN if missing/NoData
snap.values.temperature_2m;
```

### Point time-series: `wmt.forecast()`

Many variables at one point across the time axis:

```ts
const fc = await wmt.forecast({
  lat: 52.52,
  lon: 13.405,
  variables: ["dbzh", "temperature_2m"],
});

fc.times;             // Date[], one per step, aligned with all series
fc.values.dbzh;       // Float32Array; fc.values.dbzh[i] is at fc.times[i]
fc.values.dbzh[0];    // NaN if missing/NoData
```

Optional `z` (defaults to `maxZoom`) and `timeRange` to restrict the window:

```ts
await wmt.forecast({
  lat: 52.52,
  lon: 13.405,
  variables: ["dbzh"],
  z: 7,
  timeRange: {
    start: new Date("2026-05-12T00:00:00Z"),
    end: new Date("2026-05-13T00:00:00Z"),
  },
});
```

`forecast()` fans out one parallel request per `(variable, time step)`. There is no cross-step coalescing, because different time steps live in different blocks. For per-variable metadata (`unit`, `colormap`, `range`) reach for the variable handle: `wmt.variable("dbzh").unit`.

### Tile rendering for maps

Map renderers need the actual raster pixels of a tile, not point samples. Resolve a `Variable` handle once and reuse it for every tile in every frame:

```ts
const t2m = wmt.variable("temperature_2m");

t2m.unit;              // "K"
t2m.range;             // { min, max }, feed into your colormap
t2m.colormap;          // "magma"

// Fetch one tile (Float32Array of tileSize² values; NaN where NoData).
const pixels = await t2m.tile({ time: 12, z: 5, x: 16, y: 11 });

// Or by absolute time, must match a step exactly.
const pixels2 = await t2m.tile({
  time: new Date("2026-05-06T12:00:00Z"),
  z: 5, x: 16, y: 11,
});
```

For UIs that paint several tiles in one frame, `tiles()` coalesces 1 or 2 range requests instead of one per tile (when all tiles share the same variable + time block):

```ts
const frame = await t2m.tiles({
  time: 12,
  coords: [
    { z: 5, x: 16, y: 11 },
    { z: 5, x: 17, y: 11 },
    { z: 5, x: 18, y: 11 },
  ],
});
// frame[i] is always a Float32Array (NaN-filled if missing/out-of-range).
```

Tune coalescing with the `coalesce` option:

```ts
await t2m.tiles({ time: 12, coords, coalesce: { maxGapBytes: 32_000 } });
```

If you only need one pixel and want a plain `number | null` (rather than the wrapped `wmt.value()` result), there is also:

```ts
const valueK = await t2m.sample({ time: 12, lat: 52.52, lon: 13.405 });
// number | null  (null = out-of-range zoom / invalid coords; NaN = NoData)
```

### Loading from a buffer

```ts
import { readFileSync } from "node:fs";
import { open } from "wmtiles";

const wmt = await open(
  new Uint8Array(readFileSync("./data.wmt")),
);
```

### Custom byte source

Implement the `ByteSource` interface with one method, async byte-range reads:

```ts
import { open, type ByteSource } from "wmtiles";

const s3: ByteSource = {
  async read(offset, length) {
    const resp = await s3Client.send(new GetObjectCommand({
      Bucket: "wx",
      Key: "data.wmt",
      Range: `bytes=${offset}-${offset + length - 1}`,
    }));
    return new Uint8Array(await resp.Body!.transformToByteArray());
  },
};

const wmt = await open(s3);
```

### Errors

All thrown errors derive from `WMTError`:

| Error | When |
|---|---|
| `SourceError` | Source/read failure, for example an HTTP server that ignores range requests. |
| `FormatError` | Malformed file: bad magic, bad CRC, truncated buffers, unsupported version. |
| `UnknownVariableError` | `wmt.variable("foo")`, `wmt.value({ variables: ["foo"] })`, etc. for an absent name. |
| `TimeOutOfRangeError` | A `Date` that doesn't align to a step, an out-of-range index, or `timeRange` where `start > end`. |

```ts
import { UnknownVariableError } from "wmtiles";

try {
  wmt.variable("nope");
} catch (e) {
  if (e instanceof UnknownVariableError) console.warn(e.variableName);
}
```

## API surface

| Layer | Exports | When to use |
|---|---|---|
| **Root** | `open`, `WMT`, `Variable`, `httpSource`, `bytesSource`, `ByteSource`, request types | What normal callers want. |
| **Geo helper** | `latLonToTilePixel` | Point sampling and custom map UIs. |
| **Errors** | `WMTError`, `SourceError`, `FormatError`, `UnknownVariableError`, `TimeOutOfRangeError` | `instanceof` checks. |
| **`wmtiles/format`** | `parseHeader`, `parseBlockTable`, format constants and structs | Advanced: build your own caching layer. |
| **`wmtiles/codec`** | `decodeCodec`, `dequantize`, codec constants | Advanced: decode raw tile blobs. |
| **`wmtiles/tileid`** | `encode3D`, `hilbertXY2D`, `zoomOffset` | Advanced: precompute format tile IDs. |

This library is a faithful port of the Go reader in `reader/reader.go`.

## License

MIT.
