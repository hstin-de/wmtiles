import type { TileCoord, Variable, WMT } from "../reader.js";
import {
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
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

export type { TileDrawRect } from "./heatmap.js";

export interface ArrowsRendererOptions extends TileSourceOptions {
  style?: "arrow" | "barb";
  // arrowsPerTile² total per visible tile, anchored to tile coords so they
  // stay locked to world position across pan/zoom
  arrowsPerTile?: number;
  arrowSize?: number; // px; default 16 for arrows, 52 for barbs
  // 0 disables. Outline pass redraws slightly larger in a dark colour so
  // arrows stay legible on bright backgrounds. Ignored for barbs.
  outlineWidth?: number;
  outlineColor?: [number, number, number];
  colormap?: Colormap | BuiltinColormapName;
  speedRange?: [number, number]; // m/s, normalises colormap; ignored for barbs
  alpha?: number;
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
  speedToKnots?: number;
  // Barb style only. CSS colour of the drawn barbs. Default "#202020".
  barbColor?: string;
  flat?: boolean;
  onFrame?: (frameMs: number) => void; // cpu time per draw
  // When true: caller drives drawing via draw(matrix, ...), tile rects are
  // mercator world units (0..1), and no internal rAF loop runs.
  matrixMode?: boolean;
  // Matrix mode only. Fires when the next draw() would produce a different
  // result (state change, new view, tile arrival). Wire to map.triggerRepaint().
  onRedraw?: () => void;
}

export interface ArrowsRendererState {
  uVar: Variable;
  vVar: Variable;
  t: number;
}

const DEFAULTS = {
  speedRange: [0, 30] as [number, number],
  disableTimeLerp: false,
  prefetchNext: true,
} as const;

// Local-space arrow pointing +x. Per-instance rotation aligns with flow.
// 6 stem + 3 head verts.
const ARROW_GEOM = new Float32Array([
  -0.6, -0.08,
   0.2, -0.08,
  -0.6,  0.08,
  -0.6,  0.08,
   0.2, -0.08,
   0.2,  0.08,
   0.15, -0.28,
   0.6,   0.0,
   0.15,  0.28,
]);

const WIND_RULE_GLSL = `
vec4 glyphRule(vec4 texel) {
  vec2 wind = texel.rg;
  bool dead = isMissing(wind.x) || isMissing(wind.y)
           || (wind.x == 0.0 && wind.y == 0.0);
  return vec4(atan(-wind.y, wind.x), length(wind), dead ? 0.0 : 1.0, 0.0);
}`;

function buildBarbRule(speedToKnots: number, perHemi: number): string {
  return `
vec4 glyphRule(vec4 texel) {
  vec2 wind = texel.rg;
  bool dead = isMissing(wind.x) || isMissing(wind.y);
  float knots = length(wind) * ${speedToKnots.toFixed(6)};
  float idx = clamp(floor(knots / 5.0 + 0.5), 0.0, ${(perHemi - 1).toFixed(1)});
  if (g_glyphLat < 0.0) idx += ${perHemi.toFixed(1)};
  return vec4(atan(wind.y, -wind.x), length(wind), dead ? 0.0 : 1.0, idx);
}`;
}

function buildWindBarbSheet(
  color: string,
  maxKnots = 150,
  cellPx = 80,
): {
  canvas: HTMLCanvasElement;
  cols: number;
  rows: number;
  count: number;
  perHemi: number;
} {
  const perHemi = Math.floor(maxKnots / 5) + 1;
  const count = perHemi * 2;
  const cols = Math.ceil(Math.sqrt(count));
  const rows = Math.ceil(count / cols);
  const canvas = document.createElement("canvas");
  canvas.width = cols * cellPx;
  canvas.height = rows * cellPx;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("2d canvas context unavailable");
  ctx.lineCap = "round";
  ctx.lineJoin = "round";

  const L = cellPx * 0.44; // staff length: plotting point -> tip
  const B = cellPx * 0.18; // full barb / pennant height (~0.4 of the staff)
  const slotBase = cellPx * 0.075; // nominal spacing of elements along the staff
  const leanXBase = cellPx * 0.045; // nominal element lean-back along the staff
  const lw = cellPx * 0.035;

  const drawGlyph = (
    ox: number,
    oy: number,
    knots: number,
    dir: number,
  ): void => {
    if (knots < 5) {
      ctx.beginPath();
      ctx.arc(ox, oy, cellPx * 0.07, 0, Math.PI * 2);
      ctx.stroke();
      return;
    }
    const tipX = ox + L;

    let rem = knots;
    const pennants = Math.floor(rem / 50);
    rem -= pennants * 50;
    const fullBarbs = Math.floor(rem / 10);
    rem -= fullBarbs * 10;
    const halfBarbs = Math.floor(rem / 5);
    // a lone half barb sits set in from the tip, per convention
    const loneHalf = halfBarbs > 0 && fullBarbs === 0 && pennants === 0;

    // total staff slots the elements occupy; on fast barbs the nominal spacing
    // would push elements past the plotting point, so shrink it to fit L
    const span =
      pennants + (pennants > 0 ? 0.5 : 0) + fullBarbs +
      (loneHalf ? 1 : 0) + halfBarbs;
    const slot = span * slotBase > L ? L / span : slotBase;
    const leanX = leanXBase * (slot / slotBase);
    // sink the barb/pennant roots half a staff-width into the staff so their
    // round caps stay buried under it instead of poking out the far side
    const rootY = oy - dir * (lw / 2);

    // place elements from the tip inward; barbs lean back (-x) and go off the
    // staff on the `dir` side
    let pos = tipX;
    const tick = (atX: number, len: number): void => {
      ctx.beginPath();
      ctx.moveTo(atX, rootY);
      ctx.lineTo(atX - leanX * (len / B), oy - dir * len);
      ctx.stroke();
    };
    for (let p = 0; p < pennants; p++) {
      ctx.beginPath();
      ctx.moveTo(pos, rootY);
      ctx.lineTo(pos - slot * 0.9, rootY);
      ctx.lineTo(pos - leanX, oy - dir * B);
      ctx.closePath();
      ctx.fill();
      ctx.stroke();
      pos -= slot;
    }
    if (pennants > 0) pos -= slot * 0.5; // gap after pennants
    for (let b = 0; b < fullBarbs; b++) {
      tick(pos, B);
      pos -= slot;
    }
    if (loneHalf) pos -= slot;
    for (let h = 0; h < halfBarbs; h++) {
      tick(pos, B * 0.5);
      pos -= slot;
    }

    // staff last so it sits over the sunk-in barb/pennant roots, hiding the
    // last sliver of their caps
    ctx.beginPath();
    ctx.moveTo(ox, oy);
    ctx.lineTo(tipX, oy);
    ctx.stroke();
  };

  for (let i = 0; i < count; i++) {
    const ox = (i % cols) * cellPx + cellPx / 2; // plotting point = cell centre
    const oy = ((i / cols) | 0) * cellPx + cellPx / 2;
    const knots = (i % perHemi) * 5;
    const dir = i < perHemi ? 1 : -1; // northern block first, then mirrored
    // light halo underneath, final colour on top
    ctx.strokeStyle = "rgba(255,255,255,0.9)";
    ctx.fillStyle = "rgba(255,255,255,0.9)";
    ctx.lineWidth = lw + cellPx * 0.04;
    drawGlyph(ox, oy, knots, dir);
    ctx.strokeStyle = color;
    ctx.fillStyle = color;
    ctx.lineWidth = lw;
    drawGlyph(ox, oy, knots, dir);
  }
  return { canvas, cols, rows, count, perHemi };
}

const FS_ATLAS = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_texU;
uniform sampler2D u_texV;
uniform vec4 u_uvU;
uniform vec4 u_uvV;
${MISSING_GLSL_PREAMBLE}
void main() {
  float u = texture(u_texU, u_uvU.xy + v_uv * u_uvU.zw).r;
  float v = texture(u_texV, u_uvV.xy + v_uv * u_uvV.zw).r;
  outColor = packRG(u, v);
}`;

const FS_ATLAS_LERP = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_texUA;
uniform sampler2D u_texUB;
uniform sampler2D u_texVA;
uniform sampler2D u_texVB;
uniform vec4 u_uvUA;
uniform vec4 u_uvUB;
uniform vec4 u_uvVA;
uniform vec4 u_uvVB;
uniform float u_lerp;
${MISSING_GLSL_PREAMBLE}
void main() {
  float uA = texture(u_texUA, u_uvUA.xy + v_uv * u_uvUA.zw).r;
  float uB = texture(u_texUB, u_uvUB.xy + v_uv * u_uvUB.zw).r;
  float vA = texture(u_texVA, u_uvVA.xy + v_uv * u_uvVA.zw).r;
  float vB = texture(u_texVB, u_uvVB.xy + v_uv * u_uvVB.zw).r;
  outColor = packRG(lerpR(uA, uB, u_lerp), lerpR(vA, vB, u_lerp));
}`;

export class ArrowsRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;
  private readonly glyphs: GlyphRenderer;

  private readonly opts: Required<
    Pick<
      ArrowsRendererOptions,
      "speedRange" | "disableTimeLerp" | "prefetchNext"
    >
  >;

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly quadVAO: WebGLVertexArrayObject;

  private atlasTex: WebGLTexture | null = null;
  private atlasFBO: WebGLFramebuffer | null = null;
  private atlasTileW = 0;
  private atlasTileH = 0;

  // atlas depends on vars/time/tile-set, not on pan/zoom/tilt. Keeping the
  // cached atlas through a pure pan is what keeps this layer feeling tight.
  private atlasDirty = true;
  private atlasReady = false;
  private disposed = false;

  state: ArrowsRendererState;

  constructor(
    gl: WebGL2RenderingContext,
    wmt: WMT,
    options: ArrowsRendererOptions = {},
    source?: TileSource,
  ) {
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    this.gl = gl;
    this.wmt = wmt;

    this.opts = {
      speedRange: options.speedRange ?? DEFAULTS.speedRange,
      disableTimeLerp: options.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
      prefetchNext: options.prefetchNext ?? DEFAULTS.prefetchNext,
    };

    const colormap = resolveColormap(options.colormap);

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

    // barb style swaps the colormap-shaded arrow geometry for a generated
    // sprite sheet of meteorological wind barbs, indexed by speed
    let rule = WIND_RULE_GLSL;
    let sprite: GlyphSprite | undefined;
    let glyphSize = options.arrowSize;
    if (options.style === "barb") {
      const sheet = buildWindBarbSheet(options.barbColor ?? "#202020");
      sprite = { image: sheet.canvas, cols: sheet.cols, rows: sheet.rows };
      rule = buildBarbRule(options.speedToKnots ?? 1.94384, sheet.perHemi);
      glyphSize = options.arrowSize ?? 52;
    }

    this.glyphs = new GlyphRenderer(gl, {
      geometry: ARROW_GEOM,
      rule,
      sprite,
      texelsPerTile: wmt.tileSize,
      // wind direction is geographic, so arrows turn with the map
      rotateWithMap: true,
      // a wind field reads naturally lying on the map surface
      flat: options.flat ?? true,
      glyphsPerTile: options.arrowsPerTile,
      glyphSize,
      outlineWidth: options.outlineWidth,
      outlineColor: options.outlineColor,
      colormap,
      valueRange: this.opts.speedRange,
      alpha: options.alpha,
      matrixMode: options.matrixMode,
      onRedraw: options.onRedraw,
      onFrame: options.onFrame,
      prepareAtlas: () => this.prepareAtlas(),
    });

    this.state = {
      uVar: wmt.variables[0],
      vVar: wmt.variables[0],
      t: 0,
    };
  }

  setState(patch: Partial<ArrowsRendererState>): void {
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

  // per-frame data handoff for GlyphRenderer: builds/reuses the RG atlas
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
      internalFormat: gl.RG32F,
      format: gl.RG,
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
    const { uVar, vVar } = this.state;
    const view = this.glyphs.getView();
    const missing = (t: number, vari: Variable): TileCoord[] => {
      const out: TileCoord[] = [];
      const seen = new Set<string>();
      for (const r of view) {
        if (this.source.hasExact(vari, t, r.z, r.x, r.y)) continue;
        const k = `${r.z}|${r.x}|${r.y}`;
        if (seen.has(k)) continue;
        seen.add(k);
        out.push({ z: r.z, x: r.x, y: r.y });
      }
      return out;
    };
    const muF = missing(tF, uVar);
    const mvF = missing(tF, vVar);
    if (muF.length > 0) this.source.requestTiles(uVar, tF, muF);
    if (mvF.length > 0) this.source.requestTiles(vVar, tF, mvF);
    if (tC !== tF) {
      const muC = missing(tC, uVar);
      const mvC = missing(tC, vVar);
      if (muC.length > 0) this.source.requestTiles(uVar, tC, muC);
      if (mvC.length > 0) this.source.requestTiles(vVar, tC, mvC);
    }
    if (tP >= 0) {
      const muP = missing(tP, uVar);
      const mvP = missing(tP, vVar);
      if (muP.length > 0) this.source.requestTiles(uVar, tP, muP);
      if (mvP.length > 0) this.source.requestTiles(vVar, tP, mvP);
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
    gl.clearColor(NaN, NaN, 0, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.disable(gl.BLEND);

    const { uVar, vVar } = this.state;
    const lerp = frac > 0 && tF !== tC;
    const prog = lerp ? this.progAtlasLerp : this.progAtlas;
    gl.useProgram(prog);
    gl.bindVertexArray(this.quadVAO);

    let drawn = 0;
    for (const tile of this.glyphs.getView()) {
      const refUA = this.source.findTex(uVar, tF, tile.z, tile.x, tile.y);
      const refVA = this.source.findTex(vVar, tF, tile.z, tile.x, tile.y);
      if (!refUA || !refVA) continue;
      const refUB = lerp
        ? this.source.findTex(uVar, tC, tile.z, tile.x, tile.y) ?? refUA
        : null;
      const refVB = lerp
        ? this.source.findTex(vVar, tC, tile.z, tile.x, tile.y) ?? refVA
        : null;

      const cx = tile.x - layout.minX;
      const cy = tile.y - layout.minY;
      const x0 = (cx / layout.tileW) * 2 - 1;
      const y0 = (cy / layout.tileH) * 2 - 1;
      const x1 = ((cx + 1) / layout.tileW) * 2 - 1;
      const y1 = ((cy + 1) / layout.tileH) * 2 - 1;
      gl.uniform4f(gl.getUniformLocation(prog, "u_slot"), x0, y0, x1, y1);

      if (lerp && refUB && refVB) {
        gl.activeTexture(gl.TEXTURE0);
        gl.bindTexture(gl.TEXTURE_2D, refUA.tex);
        gl.activeTexture(gl.TEXTURE1);
        gl.bindTexture(gl.TEXTURE_2D, refUB.tex);
        gl.activeTexture(gl.TEXTURE2);
        gl.bindTexture(gl.TEXTURE_2D, refVA.tex);
        gl.activeTexture(gl.TEXTURE3);
        gl.bindTexture(gl.TEXTURE_2D, refVB.tex);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texUA"), 0);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texUB"), 1);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texVA"), 2);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texVB"), 3);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvUA"), refUA.ox, refUA.oy, refUA.s, refUA.s);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvUB"), refUB.ox, refUB.oy, refUB.s, refUB.s);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvVA"), refVA.ox, refVA.oy, refVA.s, refVA.s);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvVB"), refVB.ox, refVB.oy, refVB.s, refVB.s);
        gl.uniform1f(gl.getUniformLocation(prog, "u_lerp"), frac);
      } else {
        gl.activeTexture(gl.TEXTURE0);
        gl.bindTexture(gl.TEXTURE_2D, refUA.tex);
        gl.activeTexture(gl.TEXTURE1);
        gl.bindTexture(gl.TEXTURE_2D, refVA.tex);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texU"), 0);
        gl.uniform1i(gl.getUniformLocation(prog, "u_texV"), 1);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvU"), refUA.ox, refUA.oy, refUA.s, refUA.s);
        gl.uniform4f(gl.getUniformLocation(prog, "u_uvV"), refVA.ox, refVA.oy, refVA.s, refVA.s);
      }
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
      drawn++;
    }
    return drawn > 0;
  }
}
