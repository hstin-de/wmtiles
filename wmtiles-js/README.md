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

| You want | Use | Returns |
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

## Map rendering

`wmtiles` ships four WebGL2 renderers that turn the tile data into something
visible on a map. They are framework-agnostic. First-party adapters are
available for Leaflet and MapLibre:
[`docs/leaflet.md`](./docs/leaflet.md) and
[`docs/maplibre.md`](./docs/maplibre.md).

| Renderer | Module | What it does |
|---|---|---|
| `HeatmapRenderer` | `wmtiles/render/heatmap` | Colormapped heatmap of one variable. Time-lerp + parent/child tile fallback. |
| `ParticlesRenderer` | `wmtiles/render/particles` | Animated particles advected through a u/v field. Map-anchored, with trails. |
| `ArrowsRenderer` | `wmtiles/render/arrows` | Static vector arrows on a per-tile grid. Map-anchored. |
| `IsobarRenderer` | `wmtiles/render/isobar` | Contour lines for any scalar field. Pyramid-smoothing, optional region fill. |

All four share a `TileSource` (`wmtiles/render/source`) that handles the WMT
tile cache + batched fetching, so multiple renderers backed by the same WMT
won't double-fetch.

### Quickstart with Leaflet

```sh
npm install wmtiles leaflet
```

```ts
import { open } from "wmtiles";
import "wmtiles/leaflet"; // side-effect: registers the Leaflet backend
import L from "leaflet";

const wmt = await open("/data.wmt");

const map = L.map("map").fitBounds([
  [wmt.bbox.south, wmt.bbox.west],
  [wmt.bbox.north, wmt.bbox.east],
]);

const heatmap = wmt.createHeatmapLayer({
  variable: "t2m",
  vmin: 260,
  vmax: 305,
  colormap: "viridis",
}).addTo(map);

const particles = wmt.createParticlesLayer({
  uVar: "10u",
  vVar: "10v",
  colormap: "white",
  particleSize: 2.5,
}).addTo(map);
```

The layer factories are adapter-neutral: `wmt.createHeatmapLayer(...)` returns a
handle, and `addTo(map)` picks the backend that matches the map. Import
`wmtiles/leaflet` or `wmtiles/maplibre` (or both) to register backends. To skip
auto-detection, pass `backend: "leaflet" | "maplibre"` in the layer options.

Full adapter API (all four layer factories, options, lifecycle):
[`docs/leaflet.md`](./docs/leaflet.md).

### Quickstart with MapLibre

```sh
npm install wmtiles maplibre-gl
```

```ts
import maplibregl from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
import { open } from "wmtiles";
import "wmtiles/maplibre"; // side-effect: registers the MapLibre backend

const wmt = await open("/data.wmt");

const map = new maplibregl.Map({
  container: "map",
  style: "https://demotiles.maplibre.org/style.json",
  bounds: [
    [wmt.bbox.west, wmt.bbox.south],
    [wmt.bbox.east, wmt.bbox.north],
  ],
});

map.on("load", () => {
  const heatmap = wmt.createHeatmapLayer({
    variable: "t2m",
    vmin: 260,
    vmax: 305,
    colormap: "viridis",
  }).addTo(map);

  const particles = wmt.createParticlesLayer({
    uVar: "10u",
    vVar: "10v",
    colormap: "white",
    particleSize: 2.5,
  }).addTo(map);
});
```

Same `wmt.create*Layer` calls as the Leaflet example: only the import and the
map object differ.

Full adapter API:
[`docs/maplibre.md`](./docs/maplibre.md).

### Renderer options at a glance

Pass these on a layer factory (`wmt.createHeatmapLayer(options)`) or on the raw
renderer
(`new HeatmapRenderer(canvas, wmt, options)`). The defaults are tuned for
"looks reasonable out of the box".

**Heatmap**
- `variable`: name (optional, defaults to first variable)
- `vmin`, `vmax`: colormap range (optional, default from `variable.range`)
- `t`: initial time step (default 0)
- `colormap`: `"viridis"` (default) / `"plasma"` / `"inferno"` / `"gray"` / `"white"` / `"rdbu"` / `"hilow"` / custom
- `alpha`: 0.85
- `cacheSize`: 384 tiles
- `parentFallbackLevels`: 6, `childFallback`: true
- `prefetchNext`: true, `disableTimeLerp`: false

**Particles**
- `uVar`, `vVar`: u/v variable names (optional, set via `setState` later if omitted)
- `t`: initial time step (default 0)
- `particleCount`: 4096, `particleSize`: 1.5 px, `fadeOpacity`: 0.96
- `speedFactor`: 0.0005, `maxAgeFrames`: 100
- `colormap` (for particle color by speed), `speedRange`: `[0, 30]` m/s

**Arrows**
- `uVar`, `vVar`: u/v variable names (optional)
- `t`: initial time step (default 0)
- `arrowsPerTile`: 8 (= 64 arrows per visible tile)
- `arrowSize`: 16 px, `outlineWidth`: 1.5 px, `outlineColor`: `[0, 0, 0]`
- `colormap`, `speedRange`: `[0, 30]` m/s

**Isobar**
- `variable`: name (optional)
- `t`: initial time step (default 0)
- `spacing`: contour interval in data units (e.g. 400 for 4 hPa pressure)
- `lineColor`: `[1, 1, 1]`, `lineWidth`: 1 px, `majorEvery`: 5
- `smoothness`: 4 (pyramid mip levels, auto-reduced at low zoom), `alpha`: 0.9
- `fillEnabled`: false, `fillColormap`: `"hilow"`, `fillRange`: `[min, max]` of variable, `fillAlpha`: 0.45

### Custom colormaps

Two flavours of `Colormap`:

```ts
// As RGB stops, evenly spaced, interpolated in shader
const myMap = { kind: "stops", stops: [[0, 0, 255], [255, 255, 255], [255, 0, 0]] };

// As raw GLSL, defining `vec3 colormap(float t)`
const myDiscrete = {
  kind: "glsl",
  body: `vec3 colormap(float t) { return t < 0.5 ? vec3(0.1,0.4,0.9) : vec3(0.9,0.2,0.2); }`,
};
```

Pass via the `colormap` option of any renderer. Builtins live in
`wmtiles/colormap`.

## API surface

| Layer | Exports | When to use |
|---|---|---|
| **Root** | `open`, `WMT`, `Variable`, `httpSource`, `bytesSource`, `ByteSource`, request types | What normal callers want. |
| **Geo helper** | `latLonToTilePixel` | Point sampling and custom map UIs. |
| **Errors** | `WMTError`, `SourceError`, `FormatError`, `UnknownVariableError`, `TimeOutOfRangeError` | `instanceof` checks. |
| **`wmtiles/leaflet`** | Side-effect import: registers the Leaflet backend so `wmt.create*Layer(...).addTo(leafletMap)` renders into Leaflet's `overlayPane`. See [`docs/leaflet.md`](./docs/leaflet.md). |
| **`wmtiles/maplibre`** | Side-effect import: registers the MapLibre backend so `wmt.create*Layer(...).addTo(maplibreMap)` renders as MapLibre custom layers. Import alongside `wmtiles/leaflet` if you use both. See [`docs/maplibre.md`](./docs/maplibre.md). |
| **`wmtiles/render/heatmap`** | `HeatmapRenderer`, options + state types | Build your own custom heatmap layer. |
| **`wmtiles/render/particles`** | `ParticlesRenderer` | Build your own animated particle flow layer. |
| **`wmtiles/render/arrows`** | `ArrowsRenderer` | Build your own vector arrows layer. |
| **`wmtiles/render/isobar`** | `IsobarRenderer` | Build your own contour layer. |
| **`wmtiles/render/source`** | `TileSource` | Shared tile cache/fetcher; pass into multiple renderers. |
| **`wmtiles/colormap`** | `builtinColormaps`, `resolveColormap`, `Colormap` types | Build / pass custom colormaps. |
| **`wmtiles/format`** | `parseHeader`, `parseBlockTable`, format constants and structs | Advanced: build your own caching layer. |
| **`wmtiles/codec`** | `decodeCodec`, `dequantize`, codec constants | Advanced: decode raw tile blobs. |
| **`wmtiles/tileid`** | `encode3D`, `hilbertXY2D`, `zoomOffset` | Advanced: precompute format tile IDs. |

`leaflet` and `maplibre-gl` are optional peer dependencies. The
framework-agnostic renderers expose everything needed to wire up other map
frameworks.

This library is a faithful port of the Go reader in `reader/reader.go`.

## License

MIT.
