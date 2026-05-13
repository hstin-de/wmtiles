# Leaflet Adapter

The Leaflet adapter in `wmtiles/leaflet` turns a `WMT` file into Leaflet
overlay layers. It provides WebGL-backed `L.Layer` implementations for heatmaps,
particles, arrows, and isobars.

The layers read visible tiles directly from the `WMT` instance, update on pan,
zoom, and resize, and clean up their WebGL resources when removed from the map.

## Installation

```sh
bun add wmtiles leaflet
# or
npm install wmtiles leaflet
```

In browser bundlers, also import Leaflet's CSS:

```ts
import "leaflet/dist/leaflet.css";
```

The adapter requires a browser environment with Leaflet. All layers require
WebGL2. `Particles`, `Arrows`, and `Isobar` also require
`EXT_color_buffer_float`.

## Quickstart

```ts
import L from "leaflet";
import "leaflet/dist/leaflet.css";
import { open } from "wmtiles";
import "wmtiles/leaflet";

const wmt = await open("/data/weather.wmt");

const map = L.map("map", {
  zoomControl: true,
}).fitBounds([
  [wmt.bbox.south, wmt.bbox.west],
  [wmt.bbox.north, wmt.bbox.east],
]);

L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
  attribution: "&copy; OpenStreetMap contributors",
}).addTo(map);

const heatmap = wmt.createHeatmapLayer({
  variable: "temperature_2m",
  t: 0,
  vmin: 260,
  vmax: 305,
  colormap: "viridis",
}).addTo(map);

const wind = wmt.createParticlesLayer({
  uVar: "10u",
  vVar: "10v",
  t: 0,
  particleCount: 4096,
  particleSize: 2,
  colormap: "white",
}).addTo(map);
```

The Leaflet container needs an explicit size:

```css
#map {
  width: 100%;
  height: 100vh;
}
```

## Imports

### WMT Instance Methods

Importing `wmtiles/leaflet` for its side effect adds four factory methods to
every `WMT` instance:

```ts
import { open } from "wmtiles";
import "wmtiles/leaflet";

const wmt = await open("/data/weather.wmt");

wmt.createHeatmapLayer({ variable: "temperature_2m" });
wmt.createParticlesLayer({ uVar: "10u", vVar: "10v" });
wmt.createArrowsLayer({ uVar: "10u", vVar: "10v" });
wmt.createIsobarLayer({ variable: "pressure_msl", spacing: 400 });
```

### Standalone Factories

You can also import the factory functions directly:

```ts
import { open } from "wmtiles";
import { createHeatmapLayer } from "wmtiles/leaflet";

const wmt = await open("/data/weather.wmt");
const layer = createHeatmapLayer(wmt, { variable: "temperature_2m" });
```

Both styles return normal Leaflet layers. Use them with `addTo(map)`,
`remove()`, `map.addLayer(layer)`, or `map.removeLayer(layer)`.

## Layers

### Heatmap

Heatmaps render one scalar variable as a color-mapped raster overlay. They are a
good fit for temperature, precipitation, radar reflectivity, probabilities, and
other continuous fields.

```ts
const heatmap = wmt.createHeatmapLayer({
  variable: "temperature_2m",
  t: 3.5,
  vmin: 260,
  vmax: 305,
  colormap: "plasma",
  alpha: 0.8,
}).addTo(map);
```

`t` is a numeric index on the WMT time axis. Fractional values are allowed and
are interpolated between neighboring time steps by default.

Update the layer at runtime with `setState()`:

```ts
heatmap.setState({
  t: 12,
  variable: wmt.variable("dewpoint_2m"),
  vmin: 250,
  vmax: 295,
});
```

In factory options, `variable` is a variable name. In `setState()`, `variable`
is the `Variable` handle returned by `wmt.variable(name)`.

### Particles

Particles render an animated flow field from two variables: `uVar` for the
east-west component and `vVar` for the north-south component.

```ts
const particles = wmt.createParticlesLayer({
  uVar: "10u",
  vVar: "10v",
  t: 0,
  particleCount: 8192,
  particleSize: 1.75,
  fadeOpacity: 0.96,
  speedFactor: 0.0005,
  speedRange: [0, 30],
  colormap: "viridis",
}).addTo(map);
```

The particle renderer owns its own `requestAnimationFrame` loop. The adapter
starts it when the layer is added to the map and stops it when the layer is
removed. During Leaflet zoom animations, particles are briefly paused and then
realigned.

```ts
particles.setState({
  t: 8,
  uVar: wmt.variable("u_component_of_wind"),
  vVar: wmt.variable("v_component_of_wind"),
});
```

### Arrows

Arrows render the same `u/v` field as a static, map-anchored arrow grid. Use
them when you want a calm, directly readable vector overlay.

```ts
const arrows = wmt.createArrowsLayer({
  uVar: "10u",
  vVar: "10v",
  t: 0,
  arrowsPerTile: 8,
  arrowSize: 16,
  outlineWidth: 1.5,
  outlineColor: [0, 0, 0],
  speedRange: [0, 30],
  colormap: "white",
}).addTo(map);
```

```ts
arrows.setState({
  t: 6,
  uVar: wmt.variable("10u"),
  vVar: wmt.variable("10v"),
});
```

### Isobar

Isobars render contour lines for one scalar variable. The layer can optionally
fill the regions between contours.

```ts
const isobars = wmt.createIsobarLayer({
  variable: "pressure_msl",
  t: 0,
  spacing: 400,
  lineColor: [1, 1, 1],
  lineWidth: 1,
  majorEvery: 5,
  smoothness: 4,
  fillEnabled: true,
  fillColormap: "hilow",
  fillAlpha: 0.35,
}).addTo(map);
```

The isobar layer also exposes convenience methods for common UI controls:

```ts
isobars.setState({ t: 4, variable: wmt.variable("pressure_msl") });
isobars.setSpacing(200);
isobars.setSmoothness(3);
isobars.setFillEnabled(true);
isobars.setFillRange([98000, 104000]);
isobars.setFillAlpha(0.4);

const spacingInUse = isobars.effectiveSpacing();
```

`effectiveSpacing()` returns the contour spacing currently used by the renderer.
It can differ from `spacing` when `referenceZoom` is set and the map is zoomed
out.

## State Updates

Every adapter layer has a read-only `state` property and a `setState()` method:

```ts
layer.state;
layer.setState({ t: 10 });
```

Do not mutate `state` directly. Use `setState()` so cache invalidation, tile
loading, and redraw scheduling happen correctly.

The layer state shapes are:

```ts
type HeatmapState = {
  variable: Variable;
  t: number;
  vmin: number;
  vmax: number;
};

type ParticlesState = {
  uVar: Variable;
  vVar: Variable;
  t: number;
};

type ArrowsState = {
  uVar: Variable;
  vVar: Variable;
  t: number;
};

type IsobarState = {
  variable: Variable;
  t: number;
};
```

If a layer is created without a variable option, the renderer initially uses
`wmt.variables[0]`. For `Particles` and `Arrows`, you should usually set both
`uVar` and `vVar` explicitly.

## Options

### Common Options

These options are accepted by all adapter layers:

| Option | Type | Default | Description |
|---|---:|---:|---|
| `t` | `number` | `0` | Initial time step. Fractional values enable time interpolation unless disabled. |
| `cacheSize` | `number` | `384` | Maximum number of cached tile textures per layer. |
| `parentFallbackLevels` | `number` | `6` | Number of coarser zoom levels to use as fallback while detailed tiles load. |
| `tileTextureFormat` | `"auto" \| "r32f" \| "r16f"` | `"auto"` | GPU texture format for tile data. `"auto"` probes `R32F` and falls back to `R16F` if needed. |
| `disableTimeLerp` | `boolean` | `false` | Disables interpolation between neighboring time steps. |
| `prefetchNext` | `boolean` | `true` | Prefetches the next time step. |
| `onFrame` | `(frameMs: number) => void` | - | Callback with CPU time per draw or tick. Useful for performance overlays. |

`onUpdate` and `shiftValuesByBaseline` come from the internal
`TileSourceOptions` type. For normal Leaflet usage, do not set them; the adapter
configures them where needed.

### Heatmap Options

| Option | Type | Default | Description |
|---|---:|---:|---|
| `variable` | `string` | first WMT variable | Scalar variable name. |
| `vmin` | `number` | `variable.range.min` when `variable` is set; otherwise `0` | Lower color scale bound. |
| `vmax` | `number` | `variable.range.max` when `variable` is set; otherwise `1` | Upper color scale bound. |
| `colormap` | `BuiltinColormapName \| Colormap` | `"viridis"` | Color map. |
| `alpha` | `number` | `0.85` | Heatmap opacity. |
| `childFallback` | `boolean` | `true` | Allows child-tile fallback when available. |

Unknown variable names throw during layer creation:

```ts
try {
  wmt.createHeatmapLayer({ variable: "unknown" });
} catch (error) {
  console.error(error);
}
```

### Particles Options

| Option | Type | Default | Description |
|---|---:|---:|---|
| `uVar` | `string` | first WMT variable | East-west component variable name. |
| `vVar` | `string` | first WMT variable | North-south component variable name. |
| `particleCount` | `number` | `4096` | Number of particles. |
| `particleSize` | `number` | `1.5` | Particle size in pixels. |
| `fadeOpacity` | `number` | `0.96` | Trail fade per frame. Lower values create shorter trails. |
| `speedFactor` | `number` | `0.0005` | Advection speed factor. |
| `maxAgeFrames` | `number` | `100` | Maximum particle lifetime in frames. |
| `colormap` | `BuiltinColormapName \| Colormap` | `"viridis"` | Particle color by speed. |
| `speedRange` | `[number, number]` | `[0, 30]` | Speed range used for color normalization. |

### Arrows Options

| Option | Type | Default | Description |
|---|---:|---:|---|
| `uVar` | `string` | first WMT variable | East-west component variable name. |
| `vVar` | `string` | first WMT variable | North-south component variable name. |
| `arrowsPerTile` | `number` | `8` | Arrows per tile side. `8` means 64 arrows per visible tile. |
| `arrowSize` | `number` | `16` | Arrow size in pixels. |
| `outlineWidth` | `number` | `1.5` | Outline width. `0` disables the outline. |
| `outlineColor` | `[number, number, number]` | `[0, 0, 0]` | Outline RGB color in the `0..1` range. |
| `colormap` | `BuiltinColormapName \| Colormap` | `"viridis"` | Arrow color by speed. |
| `speedRange` | `[number, number]` | `[0, 30]` | Speed range used for color normalization. |
| `alpha` | `number` | `0.95` | Arrow opacity. |

### Isobar Options

| Option | Type | Default | Description |
|---|---:|---:|---|
| `variable` | `string` | first WMT variable | Scalar variable name. |
| `spacing` | `number` | `4` | Contour interval in data units. |
| `lineColor` | `[number, number, number]` | `[1, 1, 1]` | Line RGB color in the `0..1` range. |
| `lineWidth` | `number` | `1` | Line width in logical pixels. |
| `alpha` | `number` | `0.9` | Contour line opacity. |
| `majorEvery` | `number` | `5` | Emphasizes every nth contour line. `0` disables emphasis. |
| `referenceZoom` | `number \| null` | `null` | Anchor zoom for increasing `spacing` while zooming out. |
| `smoothness` | `number` | `4` | Number of smoothing levels. Typical values are `3` to `5`. |
| `dataZoom` | `number \| "map" \| "max"` | `"map"` | Data zoom mode: follow the map, pin a zoom level, or force maximum resolution. |
| `dataZoomBoost` | `number` | `0` | Additional zoom levels above the selected `dataZoom`, capped at `wmt.zoomRange.max`. |
| `fillEnabled` | `boolean` | `false` | Enables filled regions between contours. |
| `fillColormap` | `BuiltinColormapName \| Colormap` | `"hilow"` | Fill color map. |
| `fillRange` | `[number, number]` | `variable.range` | Value range for fill coloring. |
| `fillAlpha` | `number` | `0.45` | Fill opacity. |

`spacing` must match the variable unit. For pressure data in pascals, `400`
means a 4 hPa contour interval.

## Colormaps

All colored layers accept a built-in color map name or a custom `Colormap`.

Built-in names:

```ts
"viridis" | "plasma" | "inferno" | "gray" | "white" | "rdbu" | "hilow"
```

Custom color map with RGB stops:

```ts
const temperature = {
  kind: "stops",
  stops: [
    [30, 70, 180],
    [255, 255, 255],
    [190, 30, 30],
  ],
} as const;

wmt.createHeatmapLayer({
  variable: "temperature_2m",
  colormap: temperature,
});
```

Custom color map as GLSL:

```ts
const binary = {
  kind: "glsl",
  body: `
vec3 colormap(float t) {
  return t < 0.5 ? vec3(0.1, 0.35, 0.9) : vec3(0.9, 0.2, 0.1);
}
`,
} as const;
```

RGB stops use values in the `0..255` range. `lineColor` and `outlineColor` use
normalized values in the `0..1` range.

## TypeScript

The main adapter types can be imported from `wmtiles/leaflet`:

```ts
import type {
  HeatmapLayerOptions,
  ParticlesLayerOptions,
  ArrowsLayerOptions,
  IsobarLayerOptions,
  WMTHeatmapLayer,
  WMTParticlesLayer,
  WMTArrowsLayer,
  WMTIsobarLayer,
} from "wmtiles/leaflet";
```

The adapter also re-exports the renderer option and state types:

```ts
import type {
  HeatmapRendererOptions,
  HeatmapRendererState,
  ParticlesRendererOptions,
  ParticlesRendererState,
  ArrowsRendererOptions,
  ArrowsRendererState,
  IsobarRendererOptions,
  IsobarRendererState,
} from "wmtiles/leaflet";
```

A typical time-control handler:

```ts
function setTime(
  layers: Array<WMTHeatmapLayer | WMTParticlesLayer | WMTArrowsLayer | WMTIsobarLayer>,
  t: number,
) {
  for (const layer of layers) {
    layer.setState({ t });
  }
}
```

## Combining Layers

Multiple WMTiles layers can be active in the same Leaflet map:

```ts
const heat = wmt.createHeatmapLayer({
  variable: "temperature_2m",
  colormap: "inferno",
  alpha: 0.65,
}).addTo(map);

const arrows = wmt.createArrowsLayer({
  uVar: "10u",
  vVar: "10v",
  colormap: "white",
  outlineWidth: 1,
}).addTo(map);

const pressure = wmt.createIsobarLayer({
  variable: "pressure_msl",
  spacing: 400,
  lineColor: [1, 1, 1],
  alpha: 0.9,
}).addTo(map);
```

The adapter inserts canvases into Leaflet's `overlayPane`. Use the order of
`addTo(map)` calls to control visual stacking. The canvases use
`pointer-events: none`, so normal Leaflet map interaction keeps working.

## Removing Layers

Leaflet calls `onRemove()` automatically when a layer is removed:

```ts
map.removeLayer(heatmap);
// or
heatmap.remove();
```

This removes event listeners, disposes the renderer, and removes the canvas from
the DOM. For particles, it also stops the animation loop.

## Errors and Notes

- Unknown names in `variable`, `uVar`, or `vVar` throw during layer creation.
- `setState()` expects `Variable` objects, not names. Use `wmt.variable("name")`.
- `t` is a numeric time index. If your UI works with dates, resolve the
  matching WMT time-axis index in application code.
- Browsers without WebGL2 cannot create adapter layers.
- Without `EXT_color_buffer_float`, `Particles`, `Arrows`, and `Isobar` cannot
  be created.
- For large datasets or many simultaneous layers, choose `cacheSize`
  deliberately. Each layer owns its own GPU tile cache.
