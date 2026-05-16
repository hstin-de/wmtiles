import type {
  CustomLayerInterface,
  CustomRenderMethodInput,
  Map as MapLibreMap,
} from "maplibre-gl";
import type { WMT, Variable } from "../reader.js";
import {
  type BackendLayer,
  type LayerBackend,
  registerLayerBackend,
} from "../layers.js";
import type {
  ArrowsLayerOptions,
  HatchLayerOptions,
  HeatmapLayerOptions,
  IsobarLayerOptions,
  ParticlesLayerOptions,
  SymbolLayerOptions,
} from "../layers.js";
import type { GlobeProjectionData, GlobeShaderData } from "../render/globe.js";
import {
  HeatmapRenderer,
  type HeatmapRendererState,
  type TileDrawRect,
} from "../render/heatmap.js";
import {
  ParticlesRenderer,
  type ParticlesRendererState,
} from "../render/particles.js";
import {
  IsobarRenderer,
  type IsobarRendererState,
} from "../render/isobar.js";
import {
  ArrowsRenderer,
  type ArrowsRendererState,
} from "../render/arrows.js";
import {
  SymbolRenderer,
  type SymbolRendererState,
} from "../render/symbols.js";
import {
  HatchRenderer,
  type HatchRendererState,
} from "../render/hatch.js";

const MAX_MERCATOR_LAT = 85.0511287798066;
let nextLayerId = 1;

function makeLayerId(kind: string, id?: string): string {
  return id ?? `wmtiles-maplibre-${kind}-${nextLayerId++}`;
}

function mapPixelRatio(map: MapLibreMap): number {
  const maybeMap = map as unknown as { getPixelRatio?: () => number };
  const fromMap = maybeMap.getPixelRatio?.();
  if (fromMap !== undefined && Number.isFinite(fromMap) && fromMap > 0) {
    return fromMap;
  }
  return typeof window === "undefined" ? 1 : window.devicePixelRatio || 1;
}

// css px spanned by the whole mercator world at the current zoom; renderers
// use it to size flat (map-plane) glyphs in mercator units.
function mapWorldSize(map: MapLibreMap): number {
  const transform = (map as unknown as {
    transform?: { worldSize?: number };
  }).transform;
  return transform?.worldSize ?? 512 * Math.pow(2, map.getZoom());
}

function clampLat(lat: number): number {
  return Math.max(-MAX_MERCATOR_LAT, Math.min(MAX_MERCATOR_LAT, lat));
}

function lonToTileX(lon: number, z: number): number {
  return ((lon + 180) / 360) * 2 ** z;
}

function latToTileY(lat: number, z: number): number {
  const rad = clampLat(lat) * Math.PI / 180;
  return (
    (1 - Math.log(Math.tan(rad) + 1 / Math.cos(rad)) / Math.PI) / 2
  ) * 2 ** z;
}

// structurally identical to MapLibre's types; cast so renderers don't have
// to depend on maplibre-gl
function projData(input: CustomRenderMethodInput): GlobeProjectionData {
  return input.defaultProjectionData as unknown as GlobeProjectionData;
}
function shaderData(input: CustomRenderMethodInput): GlobeShaderData {
  return input.shaderData as unknown as GlobeShaderData;
}

function pickWmtTileZoom(
  map: MapLibreMap,
  wmt: WMT,
  forceZoom: number | undefined,
  rounding: "nearest" | "floor" = "nearest",
): number {
  if (forceZoom !== undefined) {
    return Math.min(
      Math.max(Math.floor(forceZoom), wmt.zoomRange.min),
      wmt.zoomRange.max,
    );
  }
  const worldSize = mapWorldSize(map);
  const dpr = mapPixelRatio(map);
  const wmtZoom = Math.log2(worldSize * dpr / wmt.tileSize);
  const rounded = rounding === "floor"
    ? Math.floor(wmtZoom)
    : Math.round(wmtZoom);
  return Math.min(
    Math.max(rounded, wmt.zoomRange.min),
    wmt.zoomRange.max,
  );
}

function computeMercatorTileView(
  map: MapLibreMap,
  wmt: WMT,
  forceZoom?: number,
  padding = 0,
  rounding: "nearest" | "floor" = "nearest",
): TileDrawRect[] {
  const z = pickWmtTileZoom(map, wmt, forceZoom, rounding);
  const n = 2 ** z;
  const bounds = map.getBounds();

  let west = bounds.getWest();
  let east = bounds.getEast();
  if (east < west) east += 360;

  const bx0 = wmt.bbox.west;
  const bx1 = wmt.bbox.east >= wmt.bbox.west
    ? wmt.bbox.east
    : wmt.bbox.east + 360;
  west = Math.max(west, bx0);
  east = Math.min(east, bx1);
  const north = Math.min(
    Math.max(bounds.getNorth(), bounds.getSouth()),
    wmt.bbox.north,
  );
  const south = Math.max(
    Math.min(bounds.getNorth(), bounds.getSouth()),
    wmt.bbox.south,
  );
  if (east < west || north < south) return [];
  const x0 = Math.floor(lonToTileX(west, z)) - padding;
  const x1 = Math.floor(lonToTileX(east, z)) + padding;
  const y0 = Math.max(0, Math.floor(latToTileY(north, z)) - padding);
  const y1 = Math.min(n - 1, Math.floor(latToTileY(south, z)) + padding);
  if (x1 < x0 || y1 < y0) return [];

  const tiles: TileDrawRect[] = [];
  for (let xi = x0; xi <= x1; xi++) {
    const wrapped = ((xi % n) + n) % n;
    const mx0 = xi / n;
    const mx1 = (xi + 1) / n;
    for (let yi = y0; yi <= y1; yi++) {
      tiles.push({
        z,
        x: wrapped,
        worldX: xi,
        y: yi,
        sx0: mx0,
        sy0: yi / n,
        sx1: mx1,
        sy1: (yi + 1) / n,
      });
    }
  }
  return tiles;
}

function findVariable(
  wmt: WMT,
  name: string | undefined,
  label: string,
): Variable | undefined {
  if (name === undefined) return undefined;
  const variable = wmt.variables.find((v) => v.name === name);
  if (!variable) throw new Error(`wmtiles: unknown ${label} "${name}"`);
  return variable;
}

// true MapLibre custom layer: shares the GL2 context and renders inside
// MapLibre's frame, no overlay canvas
class MapLibreHeatmapLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: HeatmapRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: HeatmapRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: HeatmapLayerOptions | undefined,
    variable: Variable | undefined,
  ) {
    this.id = makeLayerId("heatmap", options?.id);
    const v = variable ?? wmt.variables[0];
    const vmin = options?.vmin
      ?? (variable && Number.isFinite(variable.range.min)
        ? variable.range.min
        : 0);
    const vmax = options?.vmax
      ?? (variable && Number.isFinite(variable.range.max)
        ? variable.range.max
        : 1);
    this.initialState = {
      variable: v,
      t: options?.t ?? 0,
      vmin,
      vmax,
    };
  }

  get state(): HeatmapRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<HeatmapRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: heatmap requires a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new HeatmapRenderer(gl, this.wmt, {
      ...this.options,
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    map.triggerRepaint();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      this.renderer.setView(computeMercatorTileView(this.map, this.wmt));
      this.viewDirty = false;
    }
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
    );
  }
}

function createHeatmapLayer(
  wmt: WMT,
  options?: HeatmapLayerOptions,
): BackendLayer {
  const variable = findVariable(wmt, options?.variable, "variable");
  return new MapLibreHeatmapLayer(wmt, options, variable);
}

// native custom layer; screen-space hatch patterns, same shape as the heatmap
// layer with a value-banded pattern fill instead of a colormap
class MapLibreHatchLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: HatchRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: HatchRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: HatchLayerOptions | undefined,
    variable: Variable | undefined,
  ) {
    this.id = makeLayerId("hatch", options?.id);
    this.initialState = {
      variable: variable ?? wmt.variables[0],
      t: options?.t ?? 0,
    };
  }

  get state(): HatchRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<HatchRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: hatch requires a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new HatchRenderer(gl, this.wmt, {
      ...this.options,
      pixelRatio: mapPixelRatio(map),
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    map.triggerRepaint();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      this.renderer.setView(computeMercatorTileView(this.map, this.wmt));
      this.viewDirty = false;
    }
    this.renderer.setPixelRatio(mapPixelRatio(this.map));
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
      mapWorldSize(this.map),
    );
  }
}

function createHatchLayer(
  wmt: WMT,
  options?: HatchLayerOptions,
): BackendLayer {
  const variable = findVariable(wmt, options?.variable, "variable");
  return new MapLibreHatchLayer(wmt, options, variable);
}

// native custom layer; the particle animation rides MapLibre's render loop
// via onRedraw -> map.triggerRepaint
class MapLibreParticlesLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: ParticlesRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: ParticlesRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: ParticlesLayerOptions | undefined,
    uVar: Variable | undefined,
    vVar: Variable | undefined,
  ) {
    this.id = makeLayerId("particles", options?.id);
    this.initialState = {
      uVar: uVar ?? wmt.variables[0],
      vVar: vVar ?? wmt.variables[0],
      t: options?.t ?? 0,
    };
  }

  get state(): ParticlesRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<ParticlesRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: particles require a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new ParticlesRenderer(gl, this.wmt, {
      ...this.options,
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    this.renderer.start();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.stop();
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      this.renderer.setView(
        computeMercatorTileView(this.map, this.wmt, undefined, 0, "floor"),
      );
      this.viewDirty = false;
    }
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
    );
  }
}

function createParticlesLayer(
  wmt: WMT,
  options?: ParticlesLayerOptions,
): BackendLayer {
  const uVar = findVariable(wmt, options?.uVar, "u variable");
  const vVar = findVariable(wmt, options?.vVar, "v variable");
  return new MapLibreParticlesLayer(wmt, options, uVar, vVar);
}

class MapLibreIsobarLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: IsobarRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: IsobarRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: IsobarLayerOptions | undefined,
    variable: Variable | undefined,
  ) {
    this.id = makeLayerId("isobar", options?.id);
    this.initialState = {
      variable: variable ?? wmt.variables[0],
      t: options?.t ?? 0,
    };
  }

  get state(): IsobarRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<IsobarRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  setSpacing(spacing: number): void {
    this.renderer?.setSpacing(spacing);
  }

  setSmoothness(smoothness: number): void {
    this.renderer?.setSmoothness(smoothness);
  }

  setFillEnabled(enabled: boolean): void {
    this.renderer?.setFillEnabled(enabled);
  }

  setFillRange(range: [number, number] | null): void {
    this.renderer?.setFillRange(range);
  }

  setFillAlpha(alpha: number): void {
    this.renderer?.setFillAlpha(alpha);
  }

  effectiveSpacing(): number {
    return this.renderer?.effectiveSpacing() ?? this.options?.spacing ?? 0;
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: isobar requires a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new IsobarRenderer(gl, this.wmt, {
      ...this.options,
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    map.triggerRepaint();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  private forceZoom(): number | undefined {
    const map = this.map;
    if (!map) return undefined;
    const dz = this.options?.dataZoom;
    const boost = this.options?.dataZoomBoost ?? 0;
    let baseZ: number;
    if (dz === "max") baseZ = this.wmt.zoomRange.max;
    else if (typeof dz === "number") baseZ = dz;
    else baseZ = Math.floor(map.getZoom());
    return Math.min(this.wmt.zoomRange.max, baseZ + boost);
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      this.renderer.setView(
        computeMercatorTileView(this.map, this.wmt, this.forceZoom()),
      );
      this.viewDirty = false;
    }
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
    );
  }
}

function createIsobarLayer(
  wmt: WMT,
  options?: IsobarLayerOptions,
): BackendLayer {
  const variable = findVariable(wmt, options?.variable, "variable");
  return new MapLibreIsobarLayer(wmt, options, variable);
}

class MapLibreArrowsLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: ArrowsRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: ArrowsRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: ArrowsLayerOptions | undefined,
    uVar: Variable | undefined,
    vVar: Variable | undefined,
  ) {
    this.id = makeLayerId("arrows", options?.id);
    this.initialState = {
      uVar: uVar ?? wmt.variables[0],
      vVar: vVar ?? wmt.variables[0],
      t: options?.t ?? 0,
    };
  }

  get state(): ArrowsRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<ArrowsRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: arrows require a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new ArrowsRenderer(gl, this.wmt, {
      ...this.options,
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    map.triggerRepaint();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      // 1-tile padding + "floor" rounding keep glyphs stable on pan/tilt; see
      // computeMercatorTileView and pickWmtTileZoom
      this.renderer.setView(
        computeMercatorTileView(this.map, this.wmt, undefined, 1, "floor"),
      );
      this.viewDirty = false;
    }
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
      (this.map.getBearing() * Math.PI) / 180,
      mapWorldSize(this.map),
    );
  }
}

function createArrowsLayer(
  wmt: WMT,
  options?: ArrowsLayerOptions,
): BackendLayer {
  const uVar = findVariable(wmt, options?.uVar, "u variable");
  const vVar = findVariable(wmt, options?.vVar, "v variable");
  return new MapLibreArrowsLayer(wmt, options, uVar, vVar);
}

class MapLibreSymbolLayer implements CustomLayerInterface {
  readonly type = "custom" as const;
  readonly renderingMode = "2d" as const;
  readonly id: string;

  private renderer: SymbolRenderer | null = null;
  private map: MapLibreMap | null = null;
  private initialState: SymbolRendererState;
  private viewDirty = true;

  private readonly onMove = (): void => {
    this.viewDirty = true;
    this.map?.triggerRepaint();
  };
  private readonly onResize = this.onMove;

  constructor(
    private readonly wmt: WMT,
    private readonly options: SymbolLayerOptions | undefined,
    variable: Variable | undefined,
  ) {
    this.id = makeLayerId("symbol", options?.id);
    this.initialState = {
      variable: variable ?? wmt.variables[0],
      t: options?.t ?? 0,
    };
  }

  get state(): SymbolRendererState {
    return this.renderer?.state ?? this.initialState;
  }

  setState(patch: Partial<SymbolRendererState>): void {
    if (this.renderer) {
      this.renderer.setState(patch);
    } else {
      Object.assign(this.initialState, patch);
    }
  }

  addTo(map: MapLibreMap, beforeId?: string): this {
    map.addLayer(this, beforeId);
    return this;
  }

  remove(): void {
    const map = this.map;
    if (map?.getLayer(this.id)) map.removeLayer(this.id);
  }

  onAdd(map: MapLibreMap, gl: WebGLRenderingContext | WebGL2RenderingContext): void {
    if (!(gl instanceof WebGL2RenderingContext)) {
      throw new Error("wmtiles: symbol layer requires a WebGL2 MapLibre context");
    }
    this.map = map;
    this.renderer = new SymbolRenderer(gl, this.wmt, {
      ...this.options,
      matrixMode: true,
      onRedraw: () => this.map?.triggerRepaint(),
    });
    this.renderer.setState(this.initialState);

    map.on("move", this.onMove);
    map.on("resize", this.onResize);
    this.viewDirty = true;
    map.triggerRepaint();
  }

  onRemove(map: MapLibreMap): void {
    map.off("move", this.onMove);
    map.off("resize", this.onResize);
    this.renderer?.dispose();
    this.renderer = null;
    if (this.map === map) this.map = null;
  }

  render(
    _gl: WebGLRenderingContext | WebGL2RenderingContext,
    options: CustomRenderMethodInput,
  ): void {
    if (!this.renderer || !this.map) return;
    if (this.viewDirty) {
      // 1-tile padding + "floor" rounding keep glyphs stable on pan/tilt; see
      // computeMercatorTileView and pickWmtTileZoom
      this.renderer.setView(
        computeMercatorTileView(this.map, this.wmt, undefined, 1, "floor"),
      );
      this.viewDirty = false;
    }
    const canvas = this.map.getCanvas();
    this.renderer.draw(
      projData(options),
      shaderData(options),
      canvas.width,
      canvas.height,
      (this.map.getBearing() * Math.PI) / 180,
      mapWorldSize(this.map),
    );
  }
}

function createSymbolLayer(
  wmt: WMT,
  options?: SymbolLayerOptions,
): BackendLayer {
  const variable = findVariable(wmt, options?.variable, "variable");
  return new MapLibreSymbolLayer(wmt, options, variable);
}

const maplibreBackend: LayerBackend = {
  name: "maplibre",
  detect: (map) =>
    typeof (map as { triggerRepaint?: unknown }).triggerRepaint === "function",
  build: (wmt, kind, options) => {
    switch (kind) {
      case "heatmap":
        return createHeatmapLayer(wmt, options as HeatmapLayerOptions);
      case "particles":
        return createParticlesLayer(wmt, options as ParticlesLayerOptions);
      case "isobar":
        return createIsobarLayer(wmt, options as IsobarLayerOptions);
      case "arrows":
        return createArrowsLayer(wmt, options as ArrowsLayerOptions);
      case "symbol":
        return createSymbolLayer(wmt, options as SymbolLayerOptions);
      case "hatch":
        return createHatchLayer(wmt, options as HatchLayerOptions);
    }
  },
};

registerLayerBackend(maplibreBackend);
