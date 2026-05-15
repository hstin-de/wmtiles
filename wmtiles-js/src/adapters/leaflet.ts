import type * as leaflet from "leaflet";
import L from "leaflet";
import type { WMT } from "../reader.js";
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

interface CanvasRig {
  canvas: HTMLCanvasElement;
  getDPR(): number;
}

function attachCanvas(
  map: leaflet.Map,
): CanvasRig {
  const canvas = L.DomUtil.create(
    "canvas",
    "wmt-webgl-layer leaflet-zoom-animated",
  ) as HTMLCanvasElement;
  canvas.style.position = "absolute";
  canvas.style.pointerEvents = "none";
  map.getPanes().overlayPane.appendChild(canvas);
  return {
    canvas,
    getDPR: () => Math.min(window.devicePixelRatio || 1, 2),
  };
}

function resetCanvas(
  map: leaflet.Map,
  canvas: HTMLCanvasElement,
  onResize: (devW: number, devH: number) => void,
): number {
  const size = map.getSize();
  const dpr = Math.min(window.devicePixelRatio || 1, 2);

  const wpx = size.x + "px";
  const hpx = size.y + "px";
  if (canvas.style.width !== wpx) canvas.style.width = wpx;
  if (canvas.style.height !== hpx) canvas.style.height = hpx;

  const bw = Math.max(1, (size.x * dpr) | 0);
  const bh = Math.max(1, (size.y * dpr) | 0);
  onResize(bw, bh);

  const topLeft = map.containerPointToLayerPoint([0, 0]);
  L.DomUtil.setPosition(canvas, topLeft);
  return dpr;
}

function computeTileView(
  map: leaflet.Map,
  wmt: WMT,
  dpr: number,
  forceZoom?: number,
): TileDrawRect[] {
  const tileSize = wmt.tileSize;
  const rawZ = forceZoom !== undefined ? forceZoom : map.getZoom() | 0;
  const z = Math.min(
    Math.max(rawZ, wmt.zoomRange.min),
    wmt.zoomRange.max,
  );
  const n = 1 << z;
  const bounds = map.getBounds();
  const nw = map.project(bounds.getNorthWest(), z);
  const se = map.project(bounds.getSouthEast(), z);
  const x0 = Math.floor(nw.x / tileSize);
  const x1 = Math.floor(se.x / tileSize);
  const y0 = Math.max(0, Math.floor(nw.y / tileSize));
  const y1 = Math.min(n - 1, Math.floor(se.y / tileSize));

  const tiles: TileDrawRect[] = [];
  for (let xi = x0; xi <= x1; xi++) {
    const wrapped = ((xi % n) + n) % n;
    for (let yi = y0; yi <= y1; yi++) {
      const nwLL = map.unproject([xi * tileSize, yi * tileSize], z);
      const seLL = map.unproject(
        [(xi + 1) * tileSize, (yi + 1) * tileSize],
        z,
      );
      const p0 = map.latLngToContainerPoint(nwLL);
      const p1 = map.latLngToContainerPoint(seLL);
      tiles.push({
        z,
        x: wrapped,
        worldX: xi,
        y: yi,
        sx0: p0.x * dpr,
        sy0: p0.y * dpr,
        sx1: p1.x * dpr,
        sy1: p1.y * dpr,
      });
    }
  }
  return tiles;
}

function zoomAnimTransform(
  map: leaflet.Map,
  canvas: HTMLCanvasElement,
  e: leaflet.ZoomAnimEvent,
): void {
  const scale = map.getZoomScale(e.zoom, map.getZoom());
  const bounds = (map as unknown as {
    _latLngBoundsToNewLayerBounds(
      b: leaflet.LatLngBounds,
      z: number,
      c: leaflet.LatLng,
    ): leaflet.Bounds;
  })._latLngBoundsToNewLayerBounds(map.getBounds(), e.zoom, e.center);
  const offset = bounds.min ?? L.point(0, 0);
  L.DomUtil.setTransform(canvas, offset, scale);
}

function createHeatmapLayer(
  wmt: WMT,
  options?: HeatmapLayerOptions,
): BackendLayer {
  const variable = options?.variable !== undefined
    ? wmt.variables.find((v) => v.name === options.variable)
    : undefined;
  if (options?.variable !== undefined && !variable) {
    throw new Error(`wmtiles: unknown variable "${options.variable}"`);
  }

  class WMTHeatmapLayerImpl extends L.Layer {
    private renderer!: HeatmapRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };

    get state(): HeatmapRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<HeatmapRendererState>): void {
      this.renderer.setState(patch);
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: false,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new HeatmapRenderer(gl, wmt, options);

      const init: Partial<HeatmapRendererState> = {};
      if (variable) {
        init.variable = variable;
        if (Number.isFinite(variable.range.min)) init.vmin = variable.range.min;
        if (Number.isFinite(variable.range.max)) init.vmax = variable.range.max;
      }
      if (options?.vmin !== undefined) init.vmin = options.vmin;
      if (options?.vmax !== undefined) init.vmax = options.vmax;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("zoomend viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);

      this.refresh();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("zoomend viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      this.renderer.setView(computeTileView(this.map, wmt, this.dpr));
    }
  }

  return new WMTHeatmapLayerImpl();
}

function createHatchLayer(
  wmt: WMT,
  options?: HatchLayerOptions,
): BackendLayer {
  const variable = options?.variable !== undefined
    ? wmt.variables.find((v) => v.name === options.variable)
    : undefined;
  if (options?.variable !== undefined && !variable) {
    throw new Error(`wmtiles: unknown variable "${options.variable}"`);
  }

  class WMTHatchLayerImpl extends L.Layer {
    private renderer!: HatchRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };

    get state(): HatchRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<HatchRendererState>): void {
      this.renderer.setState(patch);
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: false,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new HatchRenderer(gl, wmt, options);

      const init: Partial<HatchRendererState> = {};
      if (variable) init.variable = variable;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("zoomend viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);

      this.refresh();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("zoomend viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      this.renderer.setPixelRatio(this.dpr);
      this.renderer.setView(computeTileView(this.map, wmt, this.dpr));
    }
  }

  return new WMTHatchLayerImpl();
}

function createParticlesLayer(
  wmt: WMT,
  options?: ParticlesLayerOptions,
): BackendLayer {
  const uVar = options?.uVar !== undefined
    ? wmt.variables.find((v) => v.name === options.uVar)
    : undefined;
  if (options?.uVar !== undefined && !uVar) {
    throw new Error(`wmtiles: unknown u variable "${options.uVar}"`);
  }
  const vVar = options?.vVar !== undefined
    ? wmt.variables.find((v) => v.name === options.vVar)
    : undefined;
  if (options?.vVar !== undefined && !vVar) {
    throw new Error(`wmtiles: unknown v variable "${options.vVar}"`);
  }

  class WMTParticlesLayerImpl extends L.Layer {
    private renderer!: ParticlesRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };
    // Pause + hide during zoom-anim: Leaflet CSS-scales the canvas, and new
    // particles landing at old atlas coords would cluster at the centre.
    private onZoomStart = (): void => {
      this.renderer.stop();
      this.canvas.style.visibility = "hidden";
    };
    private onZoomEnd = (): void => {
      this.canvas.style.visibility = "";
      this.refresh();
      this.renderer.start();
    };

    get state(): ParticlesRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<ParticlesRendererState>): void {
      this.renderer.setState(patch);
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: false,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new ParticlesRenderer(gl, wmt, options);

      const init: Partial<ParticlesRendererState> = {};
      if (uVar) init.uVar = uVar;
      if (vVar) init.vVar = vVar;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);
      map.on("zoomstart", this.onZoomStart);
      map.on("zoomend", this.onZoomEnd);

      this.refresh();
      this.renderer.start();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      map.off("zoomstart", this.onZoomStart);
      map.off("zoomend", this.onZoomEnd);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      this.renderer.setView(computeTileView(this.map, wmt, this.dpr));
    }
  }

  return new WMTParticlesLayerImpl();
}

function createIsobarLayer(
  wmt: WMT,
  options?: IsobarLayerOptions,
): BackendLayer {
  const variable = options?.variable !== undefined
    ? wmt.variables.find((v) => v.name === options.variable)
    : undefined;
  if (options?.variable !== undefined && !variable) {
    throw new Error(`wmtiles: unknown variable "${options.variable}"`);
  }

  class WMTIsobarLayerImpl extends L.Layer {
    private renderer!: IsobarRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };

    get state(): IsobarRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<IsobarRendererState>): void {
      this.renderer.setState(patch);
    }

    setSpacing(spacing: number): void {
      this.renderer.setSpacing(spacing);
    }

    setSmoothness(smoothness: number): void {
      this.renderer.setSmoothness(smoothness);
    }

    setFillEnabled(enabled: boolean): void {
      this.renderer.setFillEnabled(enabled);
    }

    setFillRange(range: [number, number] | null): void {
      this.renderer.setFillRange(range);
    }

    setFillAlpha(alpha: number): void {
      this.renderer.setFillAlpha(alpha);
    }

    effectiveSpacing(): number {
      return this.renderer.effectiveSpacing();
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: false,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new IsobarRenderer(gl, wmt, options);

      const init: Partial<IsobarRendererState> = {};
      if (variable) init.variable = variable;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("zoomend viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);

      this.refresh();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("zoomend viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      const dz = options?.dataZoom;
      const boost = options?.dataZoomBoost ?? 0;
      let baseZ: number;
      if (dz === "max") baseZ = wmt.zoomRange.max;
      else if (typeof dz === "number") baseZ = dz;
      else baseZ = this.map.getZoom() | 0;
      const forceZoom = Math.min(wmt.zoomRange.max, baseZ + boost);
      this.renderer.setView(
        computeTileView(this.map, wmt, this.dpr, forceZoom),
      );
    }
  }

  return new WMTIsobarLayerImpl();
}

function createArrowsLayer(
  wmt: WMT,
  options?: ArrowsLayerOptions,
): BackendLayer {
  const uVar = options?.uVar !== undefined
    ? wmt.variables.find((v) => v.name === options.uVar)
    : undefined;
  if (options?.uVar !== undefined && !uVar) {
    throw new Error(`wmtiles: unknown u variable "${options.uVar}"`);
  }
  const vVar = options?.vVar !== undefined
    ? wmt.variables.find((v) => v.name === options.vVar)
    : undefined;
  if (options?.vVar !== undefined && !vVar) {
    throw new Error(`wmtiles: unknown v variable "${options.vVar}"`);
  }

  class WMTArrowsLayerImpl extends L.Layer {
    private renderer!: ArrowsRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };

    get state(): ArrowsRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<ArrowsRendererState>): void {
      this.renderer.setState(patch);
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: true,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new ArrowsRenderer(gl, wmt, options);

      const init: Partial<ArrowsRendererState> = {};
      if (uVar) init.uVar = uVar;
      if (vVar) init.vVar = vVar;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("zoomend viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);

      this.refresh();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("zoomend viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      this.renderer.setView(computeTileView(this.map, wmt, this.dpr));
    }
  }

  return new WMTArrowsLayerImpl();
}

function createSymbolLayer(
  wmt: WMT,
  options?: SymbolLayerOptions,
): BackendLayer {
  const variable = options?.variable !== undefined
    ? wmt.variables.find((v) => v.name === options.variable)
    : undefined;
  if (options?.variable !== undefined && !variable) {
    throw new Error(`wmtiles: unknown variable "${options.variable}"`);
  }

  class WMTSymbolLayerImpl extends L.Layer {
    private renderer!: SymbolRenderer;
    private canvas!: HTMLCanvasElement;
    private map!: leaflet.Map;
    private dpr = 1;

    private onMove = (): void => this.refresh();
    private onResize = (): void => this.refresh();
    private onZoomAnim = (e: leaflet.ZoomAnimEvent): void => {
      zoomAnimTransform(this.map, this.canvas, e);
    };

    get state(): SymbolRendererState {
      return this.renderer.state;
    }

    setState(patch: Partial<SymbolRendererState>): void {
      this.renderer.setState(patch);
    }

    onAdd(map: leaflet.Map): this {
      this.map = map;
      const rig = attachCanvas(map);
      this.canvas = rig.canvas;
      const gl = this.canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: true,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this.renderer = new SymbolRenderer(gl, wmt, options);

      const init: Partial<SymbolRendererState> = {};
      if (variable) init.variable = variable;
      if (options?.t !== undefined) init.t = options.t;
      if (Object.keys(init).length > 0) this.renderer.setState(init);

      map.on("move", this.onMove);
      map.on("zoomend viewreset", this.onMove);
      map.on("resize", this.onResize);
      map.on("zoomanim", this.onZoomAnim);

      this.refresh();
      return this;
    }

    onRemove(map: leaflet.Map): this {
      map.off("move", this.onMove);
      map.off("zoomend viewreset", this.onMove);
      map.off("resize", this.onResize);
      map.off("zoomanim", this.onZoomAnim);
      this.renderer.dispose();
      this.canvas.remove();
      return this;
    }

    private refresh(): void {
      this.dpr = resetCanvas(this.map, this.canvas, (w, h) => {
        if (this.canvas.width !== w) this.canvas.width = w;
        if (this.canvas.height !== h) this.canvas.height = h;
        this.renderer.setViewport(w, h);
      });
      this.renderer.setView(computeTileView(this.map, wmt, this.dpr));
    }
  }

  return new WMTSymbolLayerImpl();
}

// Registered on import. detect() keys off getPanes, a Leaflet-only Map method,
// so it never matches a MapLibre map even when both adapters are imported.
const leafletBackend: LayerBackend = {
  name: "leaflet",
  detect: (map) =>
    typeof (map as { getPanes?: unknown }).getPanes === "function",
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

registerLayerBackend(leafletBackend);
