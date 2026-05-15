import type { TileCoord, Variable, WMT } from "../reader.js";
import type { BuiltinColormapName, Colormap } from "./colormap.js";
import { TileSource, type TileSourceOptions } from "./source.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
import { computeTimeWindow, type TimeWindow } from "./time.js";
import {
  buildQuadVAO,
  createFBO,
  createTexture,
  linkProgram,
  VS_ATLAS_SLOT,
} from "./gl.js";
import type { TileDrawRect } from "./heatmap.js";
import type { GlobeProjectionData, GlobeShaderData } from "./globe.js";
import {
  GlyphRenderer,
  type AtlasLayout,
  type GlyphAtlas,
  type GlyphSprite,
} from "./glyphs.js";

export type { GlyphSprite, SpriteSheet, SpriteIcons } from "./glyphs.js";

export type { TileDrawRect } from "./heatmap.js";

export interface SymbolRendererOptions extends TileSourceOptions {
  // symbolsPerTile² glyphs per visible tile, anchored to tile coords so they
  // stay locked to world position across pan/zoom
  symbolsPerTile?: number;
  symbolSize?: number; // px
  // 0 disables. Outline pass redraws slightly larger in a dark colour so
  // glyphs stay legible on bright backgrounds.
  outlineWidth?: number;
  outlineColor?: [number, number, number];
  colormap?: Colormap | BuiltinColormapName;
  // normalises the rule's value output onto the colormap; defaults to [0, 1]
  valueRange?: [number, number];
  alpha?: number;
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
  geometry?: Float32Array;
  rule?: string;
  sprite?: GlyphSprite;
  // Matrix mode only. When true the rule's rotation is geographic: symbols
  // turn with the map's bearing instead of staying screen-upright.
  rotateWithMap?: boolean;
  flat?: boolean;
  onFrame?: (frameMs: number) => void; // cpu time per draw
  // When true: caller drives drawing via draw(matrix, ...), tile rects are
  // mercator world units (0..1), and no internal rAF loop runs.
  matrixMode?: boolean;
  // Matrix mode only. Fires when the next draw() would produce a different
  // result (state change, new view, tile arrival). Wire to map.triggerRepaint().
  onRedraw?: () => void;
}

export interface SymbolRendererState {
  variable: Variable;
  t: number;
}

const DEFAULTS = {
  disableTimeLerp: false,
  prefetchNext: true,
  valueRange: [0, 1] as [number, number],
} as const;

// Centered unit square. Rotation-agnostic default glyph; a custom `geometry`
// pointing +x can be rotated by the rule.
const SQUARE_GEOM = new Float32Array([
  -0.5, -0.5,
   0.5, -0.5,
  -0.5,  0.5,
  -0.5,  0.5,
   0.5, -0.5,
   0.5,  0.5,
]);

// No rotation, value is the raw sample, hidden where the tile has no data.
const DEFAULT_RULE_GLSL = `
vec4 glyphRule(vec4 texel) {
  return vec4(0.0, texel.r, isMissing(texel.r) ? 0.0 : 1.0, 0.0);
}`;

const FS_ATLAS = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_tex;
uniform vec4 u_uv;
${MISSING_GLSL_PREAMBLE}
void main() {
  outColor = packR(texture(u_tex, u_uv.xy + v_uv * u_uv.zw).r);
}`;

const FS_ATLAS_LERP = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_texA;
uniform sampler2D u_texB;
uniform vec4 u_uvA;
uniform vec4 u_uvB;
uniform float u_lerp;
${MISSING_GLSL_PREAMBLE}
void main() {
  float a = texture(u_texA, u_uvA.xy + v_uv * u_uvA.zw).r;
  float b = texture(u_texB, u_uvB.xy + v_uv * u_uvB.zw).r;
  outColor = packR(lerpR(a, b, u_lerp));
}`;

export class SymbolRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;
  private readonly glyphs: GlyphRenderer;

  private readonly opts: Required<
    Pick<SymbolRendererOptions, "disableTimeLerp" | "prefetchNext">
  >;

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly quadVAO: WebGLVertexArrayObject;

  private atlasTex: WebGLTexture | null = null;
  private atlasFBO: WebGLFramebuffer | null = null;
  private atlasTileW = 0;
  private atlasTileH = 0;

  // atlas depends on variable/time/tile-set, not on pan/zoom/tilt. Keeping the
  // cached atlas through a pure pan is what keeps this layer feeling tight.
  private atlasDirty = true;
  private atlasReady = false;
  private disposed = false;

  state: SymbolRendererState;

  constructor(
    gl: WebGL2RenderingContext,
    wmt: WMT,
    options: SymbolRendererOptions = {},
    source?: TileSource,
  ) {
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    this.gl = gl;
    this.wmt = wmt;

    this.opts = {
      disableTimeLerp: options.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
      prefetchNext: options.prefetchNext ?? DEFAULTS.prefetchNext,
    };

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, {
        ...options,
        onUpdate: () => {
          this.atlasDirty = true;
          this.glyphs.schedule();
        },
      });
      this.ownsSource = true;
    }

    this.progAtlas = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS);
    this.progAtlasLerp = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS_LERP);
    this.quadVAO = buildQuadVAO(gl);

    this.glyphs = new GlyphRenderer(gl, {
      geometry: options.geometry ?? SQUARE_GEOM,
      rule: options.rule ?? DEFAULT_RULE_GLSL,
      sprite: options.sprite,
      texelsPerTile: wmt.tileSize,
      rotateWithMap: options.rotateWithMap,
      flat: options.flat,
      glyphsPerTile: options.symbolsPerTile,
      glyphSize: options.symbolSize,
      outlineWidth: options.outlineWidth,
      outlineColor: options.outlineColor,
      colormap: options.colormap,
      valueRange: options.valueRange ?? DEFAULTS.valueRange,
      alpha: options.alpha,
      matrixMode: options.matrixMode,
      onRedraw: options.onRedraw,
      onFrame: options.onFrame,
      prepareAtlas: () => this.prepareAtlas(),
    });

    this.state = {
      variable: wmt.variables[0],
      t: 0,
    };
  }

  setState(patch: Partial<SymbolRendererState>): void {
    if (this.disposed) return;
    Object.assign(this.state, patch);
    this.source.invalidate();
    this.atlasDirty = true;
    this.glyphs.schedule();
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    // GlyphRenderer fingerprints the tile set; only rebuild the atlas when it
    // actually changed
    if (this.glyphs.setView(tiles)) this.atlasDirty = true;
  }

  setViewport(widthDevPx: number, heightDevPx: number): void {
    if (this.disposed) return;
    this.glyphs.setViewport(widthDevPx, heightDevPx);
  }

  draw(
    projData: GlobeProjectionData,
    shaderData: GlobeShaderData,
    viewportWPx: number,
    viewportHPx: number,
    bearingRad = 0,
    worldSize = 0,
  ): void {
    if (this.disposed) return;
    this.glyphs.draw(
      projData,
      shaderData,
      viewportWPx,
      viewportHPx,
      bearingRad,
      worldSize,
    );
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.glyphs.dispose();
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    gl.deleteVertexArray(this.quadVAO);
    gl.deleteProgram(this.progAtlas);
    gl.deleteProgram(this.progAtlasLerp);
    if (this.ownsSource) this.source.dispose();
  }

  // per-frame data handoff for GlyphRenderer: builds/reuses the R atlas
  private prepareAtlas(): GlyphAtlas | null {
    const layout = this.glyphs.getAtlasLayout();
    if (!layout) return null;
    this.ensureAtlas(layout);
    const { tF, tC, frac, tP } = this.timeWindow();
    this.requestMissingTiles(tF, tC, tP);
    if (this.atlasDirty) {
      this.atlasReady = this.buildAtlas(layout, tF, tC, frac);
      this.atlasDirty = false;
    }
    if (!this.atlasTex) return null;
    return { tex: this.atlasTex, ready: this.atlasReady };
  }

  private ensureAtlas(layout: AtlasLayout): void {
    if (
      layout.tileW === this.atlasTileW &&
      layout.tileH === this.atlasTileH &&
      this.atlasTex
    ) {
      return;
    }
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    const ts = this.wmt.tileSize;
    this.atlasTex = createTexture(gl, layout.tileW * ts, layout.tileH * ts, {
      internalFormat: gl.R32F,
      format: gl.RED,
      type: gl.FLOAT,
    });
    this.atlasFBO = createFBO(gl, this.atlasTex);
    this.atlasTileW = layout.tileW;
    this.atlasTileH = layout.tileH;
  }

  private timeWindow(): TimeWindow {
    return computeTimeWindow(this.wmt, this.state.t, this.opts);
  }

  private requestMissingTiles(tF: number, tC: number, tP: number): void {
    const { variable } = this.state;
    const view = this.glyphs.getView();
    const missing = (t: number): TileCoord[] => {
      const out: TileCoord[] = [];
      const seen = new Set<string>();
      for (const r of view) {
        if (this.source.hasExact(variable, t, r.z, r.x, r.y)) continue;
        const k = `${r.z}|${r.x}|${r.y}`;
        if (seen.has(k)) continue;
        seen.add(k);
        out.push({ z: r.z, x: r.x, y: r.y });
      }
      return out;
    };
    const mF = missing(tF);
    if (mF.length > 0) this.source.requestTiles(variable, tF, mF);
    if (tC !== tF) {
      const mC = missing(tC);
      if (mC.length > 0) this.source.requestTiles(variable, tC, mC);
    }
    if (tP >= 0) {
      const mP = missing(tP);
      if (mP.length > 0) this.source.requestTiles(variable, tP, mP);
    }
  }

  private buildAtlas(
    layout: AtlasLayout,
    tF: number,
    tC: number,
    frac: number,
  ): boolean {
    if (!this.atlasTex || !this.atlasFBO) return false;
    const gl = this.gl;
    const ts = this.wmt.tileSize;
    const aw = layout.tileW * ts;
    const ah = layout.tileH * ts;

    gl.bindFramebuffer(gl.FRAMEBUFFER, this.atlasFBO);
    gl.viewport(0, 0, aw, ah);
    gl.clearColor(NaN, 0, 0, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.disable(gl.BLEND);

    const { variable } = this.state;
    const lerp = frac > 0 && tF !== tC;
    const prog = lerp ? this.progAtlasLerp : this.progAtlas;
    gl.useProgram(prog);
    gl.bindVertexArray(this.quadVAO);

    let drawn = 0;
    for (const tile of this.glyphs.getView()) {
      const refA = this.source.findTex(variable, tF, tile.z, tile.x, tile.y);
      if (!refA) continue;
      const refB = lerp
        ? this.source.findTex(variable, tC, tile.z, tile.x, tile.y) ?? refA
        : null;

      const cx = tile.x - layout.minX;
      const cy = tile.y - layout.minY;
      const x0 = (cx / layout.tileW) * 2 - 1;
      const y0 = (cy / layout.tileH) * 2 - 1;
      const x1 = ((cx + 1) / layout.tileW) * 2 - 1;
      const y1 = ((cy + 1) / layout.tileH) * 2 - 1;
      gl.uniform4f(gl.getUniformLocation(prog, "u_slot"), x0, y0, x1, y1);

      if (lerp && refB) {
        gl.activeTexture(gl.TEXTURE0);
        gl.bindTexture(gl.TEXTURE_2D, refA.tex);
        gl.activeTexture(gl.TEXTURE1);
        gl.bindTexture(gl.TEXTURE_2D, refB.tex);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texA"), 0);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texB"), 1);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvA"), refA.ox, refA.oy, refA.s, refA.s);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvB"), refB.ox, refB.oy, refB.s, refB.s);
        gl.uniform1f(gl.getUniformLocation(prog, "u_lerp"), frac);
      } else {
        gl.activeTexture(gl.TEXTURE0);
        gl.bindTexture(gl.TEXTURE_2D, refA.tex);
        gl.uniform1i(gl.getUniformLocation(prog, "u_tex"), 0);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uv"), refA.ox, refA.oy, refA.s, refA.s);
      }
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
      drawn++;
    }
    return drawn > 0;
  }
}
