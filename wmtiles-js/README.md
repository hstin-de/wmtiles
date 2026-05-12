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

// Resolve a variable handle once, reuse for many requests.
const t2m = wmt.variable("temperature_2m");

t2m.unit;              // "K"
t2m.range;             // { min, max }
t2m.colormap;          // "magma"

// Fetch one tile (Float32Array of tileSize² values; NaN where NoData).
const pixels = await t2m.tile({ time: 12, z: 5, x: 16, y: 11 });

// Or by absolute time — must match a step exactly.
const pixels2 = await t2m.tile({
  time: new Date("2026-05-06T12:00:00Z"),
  z: 5, x: 16, y: 11,
});

// Sample a single point (nearest pixel). Defaults z to maxZoom.
const valueK = await t2m.sample({ time: 12, lat: 52.52, lon: 13.405 });
```

### Batched tile fetch

For UIs that paint several tiles in one frame, `tiles()` issues 1–2 coalesced
range requests instead of one per tile (when all tiles share the same
variable + time block):

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

You can tune coalescing with the `coalesce` option:

```ts
await t2m.tiles({ time: 12, coords, coalesce: { maxGapBytes: 32_000 } });
```

### Forecast / time-series at a point

Sample one or more variables at a (lat, lon) point across the whole time axis:

```ts
const fc = await wmt.forecast({
  lat: 52.52,
  lon: 13.405,
  variables: ["dbzh", "temperature_2m"],
});

fc.times;            // Date[] — one per step, aligned with all series
fc.values.dbzh;      // Float32Array — fc.values.dbzh[i] is at fc.times[i]
fc.values.dbzh[0];   // NaN if missing/NoData
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

`forecast()` fans out one parallel request per `(variable, time step)` — there is
no cross-step coalescing, because different time steps live in different blocks.
Units for each series stay on the variable handle (`wmt.variable("dbzh").unit`).

### Loading from a buffer

```ts
import { readFileSync } from "node:fs";
import { open } from "wmtiles";

const wmt = await open(
  new Uint8Array(readFileSync("./data.wmt")),
);
```

### Custom byte source

Implement the `ByteSource` interface — one method, async byte-range reads:

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
| `UnknownVariableError` | `wmt.variable("foo")` for an absent name. |
| `TimeOutOfRangeError` | `tile()` / `sample()` with a `Date` that doesn't align to a step, or an out-of-range index. |

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
