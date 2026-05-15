import type { WMT } from "./reader.js";
import type {
  HeatmapRendererOptions,
  HeatmapRendererState,
} from "./render/heatmap.js";
import type {
  ParticlesRendererOptions,
  ParticlesRendererState,
} from "./render/particles.js";
import type {
  IsobarRendererOptions,
  IsobarRendererState,
} from "./render/isobar.js";
import type {
  ArrowsRendererOptions,
  ArrowsRendererState,
} from "./render/arrows.js";
import type {
  GlyphSprite,
  SpriteIcons,
  SpriteSheet,
  SymbolRendererOptions,
  SymbolRendererState,
} from "./render/symbols.js";
import type {
  HatchBand,
  HatchPatternBand,
  HatchIconBand,
  HatchPattern,
  HatchRendererOptions,
  HatchRendererState,
} from "./render/hatch.js";

export type {
  HeatmapRendererOptions,
  HeatmapRendererState,
  ParticlesRendererOptions,
  ParticlesRendererState,
  IsobarRendererOptions,
  IsobarRendererState,
  ArrowsRendererOptions,
  ArrowsRendererState,
  SymbolRendererOptions,
  SymbolRendererState,
  HatchRendererOptions,
  HatchRendererState,
  HatchBand,
  HatchPatternBand,
  HatchIconBand,
  HatchPattern,
  GlyphSprite,
  SpriteSheet,
  SpriteIcons,
};

// Built-in backend names; any string is accepted so custom backends work too.
export type LayerBackendName = "leaflet" | "maplibre" | (string & {});

interface CommonLayerOptions {
  // MapLibre style layer id; the Leaflet backend ignores it.
  id?: string;
  backend?: LayerBackendName;
}

export type HeatmapLayerOptions = HeatmapRendererOptions & CommonLayerOptions & {
  variable?: string;
  vmin?: number;
  vmax?: number;
  t?: number;
};

export type ParticlesLayerOptions =
  & ParticlesRendererOptions
  & CommonLayerOptions
  & {
    uVar?: string;
    vVar?: string;
    t?: number;
  };

export type IsobarLayerOptions = IsobarRendererOptions & CommonLayerOptions & {
  variable?: string;
  t?: number;
};

export type ArrowsLayerOptions = ArrowsRendererOptions & CommonLayerOptions & {
  uVar?: string;
  vVar?: string;
  t?: number;
};

export type SymbolLayerOptions = SymbolRendererOptions & CommonLayerOptions & {
  variable?: string;
  t?: number;
};

export type HatchLayerOptions = HatchRendererOptions & CommonLayerOptions & {
  variable?: string;
  t?: number;
};

export type LayerOptions =
  | HeatmapLayerOptions
  | ParticlesLayerOptions
  | IsobarLayerOptions
  | ArrowsLayerOptions
  | SymbolLayerOptions
  | HatchLayerOptions;

export type LayerKind =
  | "heatmap"
  | "particles"
  | "isobar"
  | "arrows"
  | "symbol"
  | "hatch";

export interface BackendLayer {
  addTo(map: any, beforeId?: string): unknown;
  remove(): unknown;
  readonly state: object;
  setState(patch: any): unknown;
  setSpacing?(spacing: number): unknown;
  setSmoothness?(smoothness: number): unknown;
  setFillEnabled?(enabled: boolean): unknown;
  setFillRange?(range: [number, number] | null): unknown;
  setFillAlpha?(alpha: number): unknown;
  effectiveSpacing?(): number;
}

export interface LayerBackend {
  // Identifier for the `backend` layer option; the built-in adapters register
  // as "leaflet" and "maplibre".
  name: string;
  // True when this backend recognizes the given map object. Only consulted
  // when the layer's `backend` option is not set.
  detect(map: object): boolean;
  build(
    wmt: WMT,
    kind: LayerKind,
    options: LayerOptions | undefined,
  ): BackendLayer;
}

const backends: LayerBackend[] = [];

export function registerLayerBackend(backend: LayerBackend): void {
  backends.push(backend);
}

export interface WMTLayer<S extends object> {
  // Throws until addTo(map) has run.
  readonly state: S;
  setState(patch: Partial<S>): this;
  addTo(map: object, beforeId?: string): this;
  remove(): this;
}

export interface WMTIsobarLayer extends WMTLayer<IsobarRendererState> {
  setSpacing(spacing: number): this;
  setSmoothness(smoothness: number): this;
  setFillEnabled(enabled: boolean): this;
  setFillRange(range: [number, number] | null): this;
  setFillAlpha(alpha: number): this;
  // Spacing actually used at the current zoom; differs from the `spacing`
  // option when referenceZoom auto-scaling kicks in.
  effectiveSpacing(): number;
}

export type WMTHeatmapLayer = WMTLayer<HeatmapRendererState>;
export type WMTParticlesLayer = WMTLayer<ParticlesRendererState>;
export type WMTArrowsLayer = WMTLayer<ArrowsRendererState>;
export type WMTSymbolLayer = WMTLayer<SymbolRendererState>;
export type WMTHatchLayer = WMTLayer<HatchRendererState>;

// variable-name option keys validated eagerly so a typo throws at create time
const VARIABLE_KEYS: Record<LayerKind, readonly string[]> = {
  heatmap: ["variable"],
  isobar: ["variable"],
  particles: ["uVar", "vVar"],
  arrows: ["uVar", "vVar"],
  symbol: ["variable"],
  hatch: ["variable"],
};

export class WMTLayerImpl<S extends object> {
  private layer: BackendLayer | null = null;
  // setState / setSpacing / ... calls made before addTo are queued here and
  // replayed once the backend layer exists
  private readonly pending: Array<(l: BackendLayer) => void> = [];

  /** @internal */
  constructor(
    private readonly wmt: WMT,
    private readonly kind: LayerKind,
    private readonly options: LayerOptions | undefined,
  ) {
    const opts = options as Record<string, unknown> | undefined;
    for (const key of VARIABLE_KEYS[kind]) {
      const name = opts?.[key];
      if (typeof name === "string" && !wmt.findVariable(name)) {
        throw new Error(`wmtiles: unknown variable "${name}"`);
      }
    }
  }

  get state(): S {
    if (!this.layer) {
      throw new Error("wmtiles: layer.state is only available after addTo(map)");
    }
    return this.layer.state as S;
  }

  setState(patch: Partial<S>): this {
    return this.run((l) => l.setState(patch));
  }

  setSpacing(spacing: number): this {
    return this.run((l) => l.setSpacing?.(spacing));
  }

  setSmoothness(smoothness: number): this {
    return this.run((l) => l.setSmoothness?.(smoothness));
  }

  setFillEnabled(enabled: boolean): this {
    return this.run((l) => l.setFillEnabled?.(enabled));
  }

  setFillRange(range: [number, number] | null): this {
    return this.run((l) => l.setFillRange?.(range));
  }

  setFillAlpha(alpha: number): this {
    return this.run((l) => l.setFillAlpha?.(alpha));
  }

  effectiveSpacing(): number {
    if (this.layer?.effectiveSpacing) return this.layer.effectiveSpacing();
    return (this.options as { spacing?: number } | undefined)?.spacing ?? 0;
  }

  addTo(map: object, beforeId?: string): this {
    if (this.layer) {
      throw new Error("wmtiles: layer is already added to a map");
    }
    const wanted = (this.options as { backend?: string } | undefined)?.backend;
    const backend = wanted
      ? backends.find((b) => b.name === wanted)
      : backends.find((b) => b.detect(map));
    if (!backend) {
      throw new Error(
        wanted
          ? `wmtiles: no layer backend named "${wanted}" is registered. ` +
            `Import "wmtiles/${wanted}" or register one with ` +
            "registerLayerBackend()."
          : 'wmtiles: no layer backend matched this map. Import ' +
            '"wmtiles/leaflet" or "wmtiles/maplibre" so the matching adapter ' +
            "registers itself.",
      );
    }
    const layer = backend.build(this.wmt, this.kind, this.options);
    this.layer = layer;
    layer.addTo(map, beforeId);
    // Replay after addTo: the Leaflet layer builds its renderer in onAdd, so
    // setState before that point would hit a missing renderer.
    for (const op of this.pending) op(layer);
    this.pending.length = 0;
    return this;
  }

  remove(): this {
    this.layer?.remove();
    return this;
  }

  private run(op: (l: BackendLayer) => void): this {
    if (this.layer) op(this.layer);
    else this.pending.push(op);
    return this;
  }
}
