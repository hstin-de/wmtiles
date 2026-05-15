import type { TileCoord, Variable, WMT } from "../reader.js";
import {
  colormapToGLSL,
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
import { TileSource, type TileSourceOptions } from "./source.js";
import { BILINEAR_R_GLSL, MISSING_GLSL_PREAMBLE } from "./missing.js";
import { computeTimeWindow, type TimeWindow } from "./time.js";
import {
  createFBO,
  createTexture,
  linkProgram,
  VS_ATLAS_SLOT,
  VS_FULLSCREEN,
} from "./gl.js";
import type { TileDrawRect } from "./heatmap.js";
import {
  buildSubdividedQuadVAO,
  getProjectionUniformLocs,
  setProjectionUniforms,
  type GlobeProjectionData,
  type GlobeShaderData,
  type ProjectionUniformLocs,
} from "./globe.js";
import {
  beginHostFrame,
  RedrawScheduler,
  VariantProgramCache,
} from "./backend.js";

const GLOBE_SEGMENTS = 16;

interface IsobarProgram {
  program: WebGLProgram;
  proj: ProjectionUniformLocs; // all-null in screen mode, those uniforms don't exist
}

function makeIsobarProgram(
  gl: WebGL2RenderingContext,
  vs: string,
  fs: string,
): IsobarProgram {
  const program = linkProgram(gl, vs, fs);
  return { program, proj: getProjectionUniformLocs(gl, program) };
}

export type { TileDrawRect } from "./heatmap.js";

export interface IsobarRendererOptions extends TileSourceOptions {
  spacing?: number; // contour interval in data units (e.g. 4 for 4 hPa)
  lineColor?: [number, number, number]; // rgb 0..1, default white
  lineWidth?: number; // logical px, scaled by dpr
  alpha?: number;
  majorEvery?: number; // every Nth line at 1.5x opacity, 0 disables
  referenceZoom?: number | null;
  // Pyramid downsample levels (NaN-aware 2x2 box). 3-5 is broadcast-style.
  // Internally capped so the smallest mip stays >= 16 texels.
  smoothness?: number;
  // 'map' follows the map zoom, 'max' pulls highest-res (heavy on global
  // data at low zoom), number pins it.
  dataZoom?: number | "map" | "max";
  // Bounded alternative to 'max': extra levels on top of the chosen base,
  // capped at the file's max zoom.
  dataZoomBoost?: number;
  // Colormap fill between contours. Pair with a diverging map (e.g. "rdbu")
  // for high/low pressure regions.
  fillEnabled?: boolean;
  fillColormap?: Colormap | BuiltinColormapName;
  fillRange?: [number, number]; // defaults to variable.range from WMT header
  fillAlpha?: number;
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
  onFrame?: (frameMs: number) => void; // cpu time per draw call
  // When true: caller drives drawing via draw(matrix, ...) into a shared gl
  // context; setView tile rects are mercator (0..1); no internal rAF runs.
  matrixMode?: boolean;
  // Matrix mode only. Fires when the next draw() would produce a different
  // result (state, view, tile arrival). Wire to map.triggerRepaint().
  onRedraw?: () => void;
}

export interface IsobarRendererState {
  variable: Variable;
  t: number;
}

const DEFAULTS = {
  spacing: 4,
  lineColor: [1, 1, 1] as [number, number, number],
  lineWidth: 1.0,
  alpha: 0.9,
  majorEvery: 5,
  referenceZoom: null as number | null,
  smoothness: 4,
  fillEnabled: false,
  fillAlpha: 0.45,
  disableTimeLerp: false,
  prefetchNext: true,
} as const;

const MIN_MIP_TEXEL = 16;
const MAX_MIP_LEVELS = 8;

const FS_ATLAS = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_tex;
uniform vec4 u_uv;
${MISSING_GLSL_PREAMBLE}
void main() {
  float v = texture(u_tex, u_uv.xy + v_uv * u_uv.zw).r;
  outColor = packR(v);
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

// Missing-aware 2x2 box downsample. Output is missing only when all 4 taps
// are, so the data region shrinks by at most one source texel per level.
const FS_DOWN = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_src;
${MISSING_GLSL_PREAMBLE}
void main() {
  vec2 srcSz = vec2(textureSize(u_src, 0));
  vec2 q = (0.5 / srcSz);
  float v00 = texture(u_src, v_uv + vec2(-q.x, -q.y)).r;
  float v10 = texture(u_src, v_uv + vec2( q.x, -q.y)).r;
  float v01 = texture(u_src, v_uv + vec2(-q.x,  q.y)).r;
  float v11 = texture(u_src, v_uv + vec2( q.x,  q.y)).r;
  float sum = 0.0;
  float n = 0.0;
  if (!isMissing(v00)) { sum += v00; n += 1.0; }
  if (!isMissing(v10)) { sum += v10; n += 1.0; }
  if (!isMissing(v01)) { sum += v01; n += 1.0; }
  if (!isMissing(v11)) { sum += v11; n += 1.0; }
  outColor = packR(n > 0.0 ? sum / n : MISSING_SENTINEL);
}`;

const VS_CONTOUR_SCREEN = `#version 300 es
precision highp float;
layout(location=0) in vec2 a_pos;
uniform vec2 u_screen;
uniform vec4 u_atlasRect;
out vec2 v_uv;
void main() {
  v_uv = a_pos;
  vec2 px = mix(u_atlasRect.xy, u_atlasRect.zw, a_pos);
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  gl_Position = vec4(ndc, 0.0, 1.0);
}`;

// subdivided quad over the atlas mercator extent, projected via projectTile();
// the subdivision is what gives globe curvature
function buildContourMatrixVS(shaderData: GlobeShaderData): string {
  return `#version 300 es
precision highp float;
${shaderData.vertexShaderPrelude}
${shaderData.define}
layout(location=0) in vec2 a_pos;
uniform vec4  u_mercRect;
out vec2 v_uv;
void main() {
  v_uv = a_pos;
  vec2 mercator = mix(u_mercRect.xy, u_mercRect.zw, a_pos);
  gl_Position = projectTile(mercator);
}`;
}

function buildContourFS(premultiply: boolean): string {
  return `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_field;
uniform float u_spacing;
uniform float u_phase;  // baseline mod spacing, in [0, spacing)
uniform float u_kMod;   // floor(baseline/spacing) mod majorEvery, 0 when majorEvery=0
uniform float u_lineWidth;
uniform vec3  u_lineColor;
uniform float u_alpha;
uniform float u_majorEvery;

${MISSING_GLSL_PREAMBLE}
${BILINEAR_R_GLSL}

void main() {
  float v = bilinearR(u_field, v_uv);
  if (isMissing(v)) discard;
  float shifted = v + u_phase;
  float n_local = floor(shifted / u_spacing + 0.5);
  float fw = fwidth(v);
  if (fw <= 0.0) discard;
  if (fw > u_spacing * 5.0) discard;
  float distPx = abs(shifted - n_local * u_spacing) / fw;
  float halfW = u_lineWidth * 0.5;
  if (distPx > halfW + 0.5) discard;
  float aa = clamp(halfW + 0.5 - distPx, 0.0, 1.0);
  float emphasis = 1.0;
  if (u_majorEvery > 0.0) {
    float m = mod(u_kMod + n_local, u_majorEvery);
    if (m < 0.5 || m > u_majorEvery - 0.5) emphasis = 1.5;
  }
  float a = aa * u_alpha * emphasis;
  ${premultiply
    ? "outColor = vec4(u_lineColor * a, a);"
    : "outColor = vec4(u_lineColor, a);"}
}`;
}

// Fills between contours, runs before the contour pass so lines sit on top.
// Same manual bilinear as FS_CONTOUR since R32F isn't filterable everywhere.
function buildFillFS(colormap: Colormap, premultiply: boolean): string {
  return `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_field;
uniform vec2 u_range;
uniform float u_alpha;

${MISSING_GLSL_PREAMBLE}
${BILINEAR_R_GLSL}
${colormapToGLSL(colormap)}

void main() {
  float v = bilinearR(u_field, v_uv);
  if (isMissing(v)) discard;
  float t = clamp((v - u_range.x) / max(u_range.y - u_range.x, 1e-30), 0.0, 1.0);
  vec3 rgb = colormap(t);
  ${premultiply
    ? "outColor = vec4(rgb * u_alpha, u_alpha);"
    : "outColor = vec4(rgb, u_alpha);"}
}`;
}


interface MipLevel {
  tex: WebGLTexture;
  fbo: WebGLFramebuffer;
  w: number;
  h: number;
}

export class IsobarRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;

  private readonly opts: Required<
    Pick<
      IsobarRendererOptions,
      | "spacing"
      | "lineColor"
      | "lineWidth"
      | "alpha"
      | "majorEvery"
      | "smoothness"
      | "fillEnabled"
      | "fillAlpha"
      | "disableTimeLerp"
      | "prefetchNext"
    >
  > & {
    referenceZoom: number | null;
    fillRange: [number, number] | null;
  };
  private readonly onFrame?: (frameMs: number) => void;

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly progDown: WebGLProgram;
  private readonly fillPrograms: VariantProgramCache<IsobarProgram>;
  private readonly contourPrograms: VariantProgramCache<IsobarProgram>;
  private readonly scheduler: RedrawScheduler;
  private readonly fillColormap: Colormap;
  private readonly quadVAO: WebGLVertexArrayObject;
  // Subdivided VAO for matrix-mode contour/fill so the quad curves on globe.
  private readonly contourMatrixVAO: WebGLVertexArrayObject | null;
  private readonly contourMatrixIndexCount: number;

  private atlasTex!: WebGLTexture;
  private atlasFBO!: WebGLFramebuffer;
  private atlasTileW = 0;
  private atlasTileH = 0;
  private mips: MipLevel[] = [];
  private builtMipCount = 0;

  private readonly matrixMode: boolean;
  private canvasW = 1;
  private canvasH = 1;
  private view: TileDrawRect[] = [];
  // Screen mode: bounding box of the visible tile set in canvas device px.
  // Matrix mode: same set, but in mercator (0..1) units.
  private atlasRectDevPx: [number, number, number, number] = [0, 0, 1, 1];
  private mercRect: [number, number, number, number] = [0, 0, 1, 1];
  private disposed = false;

  state: IsobarRendererState;

  constructor(
    gl: WebGL2RenderingContext,
    wmt: WMT,
    options?: IsobarRendererOptions,
    source?: TileSource,
  ) {
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    this.gl = gl;
    this.wmt = wmt;
    this.matrixMode = options?.matrixMode ?? false;

    this.opts = {
      spacing: options?.spacing ?? DEFAULTS.spacing,
      lineColor: options?.lineColor ?? DEFAULTS.lineColor,
      lineWidth: options?.lineWidth ?? DEFAULTS.lineWidth,
      alpha: options?.alpha ?? DEFAULTS.alpha,
      majorEvery: options?.majorEvery ?? DEFAULTS.majorEvery,
      referenceZoom:
        options?.referenceZoom === undefined
          ? DEFAULTS.referenceZoom
          : options.referenceZoom,
      smoothness: options?.smoothness ?? DEFAULTS.smoothness,
      fillEnabled: options?.fillEnabled ?? DEFAULTS.fillEnabled,
      fillAlpha: options?.fillAlpha ?? DEFAULTS.fillAlpha,
      fillRange: options?.fillRange ?? null,
      disableTimeLerp:
        options?.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
      prefetchNext: options?.prefetchNext ?? DEFAULTS.prefetchNext,
    };
    this.onFrame = options?.onFrame;
    this.fillColormap = resolveColormap(options?.fillColormap ?? "hilow");

    this.scheduler = new RedrawScheduler(
      this.matrixMode,
      () => this.drawInternal(null, null),
      options?.onRedraw,
    );

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, {
        ...options,
        onUpdate: () => this.scheduler.schedule(),
        // mobile highp demotion mangles (v - vmin)/range and (v - n*spacing) on absolute values
        shiftValuesByBaseline: true,
      });
      this.ownsSource = true;
    }

    this.progAtlas = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS);
    this.progAtlasLerp = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS_LERP);
    this.progDown = linkProgram(gl, VS_FULLSCREEN, FS_DOWN);
    // matrix mode composites over MapLibre with ONE/ONE_MINUS_SRC_ALPHA, so
    // the FS must output premultiplied alpha; screen mode uses straight
    this.fillPrograms = new VariantProgramCache<IsobarProgram>(
      gl,
      this.matrixMode,
      {
        buildScreen: (g) =>
          makeIsobarProgram(g, VS_CONTOUR_SCREEN, buildFillFS(this.fillColormap, false)),
        buildMatrix: (g, sd) =>
          makeIsobarProgram(g, buildContourMatrixVS(sd), buildFillFS(this.fillColormap, true)),
        destroy: (g, p) => g.deleteProgram(p.program),
      },
    );
    this.contourPrograms = new VariantProgramCache<IsobarProgram>(
      gl,
      this.matrixMode,
      {
        buildScreen: (g) =>
          makeIsobarProgram(g, VS_CONTOUR_SCREEN, buildContourFS(false)),
        buildMatrix: (g, sd) =>
          makeIsobarProgram(g, buildContourMatrixVS(sd), buildContourFS(true)),
        destroy: (g, p) => g.deleteProgram(p.program),
      },
    );
    if (this.matrixMode) {
      const sub = buildSubdividedQuadVAO(gl, GLOBE_SEGMENTS);
      this.contourMatrixVAO = sub.vao;
      this.contourMatrixIndexCount = sub.indexCount;
    } else {
      this.contourMatrixVAO = null;
      this.contourMatrixIndexCount = 0;
    }

    const vbo = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, vbo);
    gl.bufferData(
      gl.ARRAY_BUFFER,
      new Float32Array([0, 0, 1, 0, 0, 1, 1, 1]),
      gl.STATIC_DRAW,
    );
    const vao = gl.createVertexArray();
    if (!vao) throw new Error("createVertexArray failed");
    gl.bindVertexArray(vao);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    this.quadVAO = vao;

    this.state = { variable: wmt.variables[0], t: 0 };
  }

  setState(patch: Partial<IsobarRendererState>): void {
    if (this.disposed) return;
    Object.assign(this.state, patch);
    this.source.invalidate();
    this.scheduler.schedule();
  }

  setSpacing(spacing: number): void {
    this.opts.spacing = spacing;
    this.scheduler.schedule();
  }

  setSmoothness(smoothness: number): void {
    this.opts.smoothness = Math.max(0, Math.round(smoothness));
    this.scheduler.schedule();
  }

  setFillEnabled(enabled: boolean): void {
    this.opts.fillEnabled = enabled;
    this.scheduler.schedule();
  }

  setFillRange(range: [number, number] | null): void {
    this.opts.fillRange = range;
    this.scheduler.schedule();
  }

  setFillAlpha(alpha: number): void {
    this.opts.fillAlpha = Math.max(0, Math.min(1, alpha));
    this.scheduler.schedule();
  }

  effectiveSpacing(): number {
    const refZ = this.opts.referenceZoom;
    if (refZ == null) return this.opts.spacing;
    const z = this.view[0]?.z ?? refZ;
    const steps = Math.max(0, refZ - z);
    return this.opts.spacing * Math.pow(2, steps);
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    const uniq = tiles.filter((t) => t.worldX === t.x);
    if (uniq.length === 0) {
      this.view = tiles;
      this.atlasRectDevPx = [0, 0, 1, 1];
      this.mercRect = [0, 0, 1, 1];
      this.scheduler.schedule();
      return;
    }
    this.view = uniq;
    let sx0 = Infinity, sy0 = Infinity, sx1 = -Infinity, sy1 = -Infinity;
    for (const t of uniq) {
      if (t.sx0 < sx0) sx0 = t.sx0;
      if (t.sy0 < sy0) sy0 = t.sy0;
      if (t.sx1 > sx1) sx1 = t.sx1;
      if (t.sy1 > sy1) sy1 = t.sy1;
    }
    if (this.matrixMode) {
      // sx/sy are mercator (0..1) in matrix mode
      this.mercRect = [sx0, sy0, sx1, sy1];
    } else {
      this.atlasRectDevPx = [sx0, sy0, sx1, sy1];
    }
    this.ensureAtlas(uniq);
    this.scheduler.schedule();
  }

  setViewport(widthDevPx: number, heightDevPx: number): void {
    if (this.disposed) return;
    const w = Math.max(1, widthDevPx | 0);
    const h = Math.max(1, heightDevPx | 0);
    if (this.canvasW === w && this.canvasH === h) return;
    this.canvasW = w;
    this.canvasH = h;
    if (!this.matrixMode) {
      this.gl.viewport(0, 0, w, h);
      this.scheduler.schedule();
    }
  }

  // Matrix-mode entry point. Call from the host's render() hook each frame.
  draw(
    projData: GlobeProjectionData,
    shaderData: GlobeShaderData,
    viewportWPx: number,
    viewportHPx: number,
  ): void {
    if (this.disposed || !this.matrixMode) return;
    this.canvasW = Math.max(1, viewportWPx | 0);
    this.canvasH = Math.max(1, viewportHPx | 0);
    this.drawInternal(projData, shaderData);
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.scheduler.dispose();
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    for (const m of this.mips) {
      gl.deleteTexture(m.tex);
      gl.deleteFramebuffer(m.fbo);
    }
    this.mips = [];
    gl.deleteVertexArray(this.quadVAO);
    if (this.contourMatrixVAO) gl.deleteVertexArray(this.contourMatrixVAO);
    gl.deleteProgram(this.progAtlas);
    gl.deleteProgram(this.progAtlasLerp);
    gl.deleteProgram(this.progDown);
    this.fillPrograms.dispose();
    this.contourPrograms.dispose();
    if (this.ownsSource) this.source.dispose();
  }

  private ensureAtlas(tiles: TileDrawRect[]): void {
    if (tiles.length === 0) return;
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const t of tiles) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
      if (t.x > maxX) maxX = t.x;
      if (t.y > maxY) maxY = t.y;
    }
    const tilesW = maxX - minX + 1;
    const tilesH = maxY - minY + 1;
    if (tilesW === this.atlasTileW && tilesH === this.atlasTileH && this.atlasTex) {
      return;
    }
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    // mip sizes track the atlas, so reallocate next frame
    for (const m of this.mips) {
      gl.deleteTexture(m.tex);
      gl.deleteFramebuffer(m.fbo);
    }
    this.mips = [];
    const ts = this.wmt.tileSize;
    this.atlasTex = createTexture(gl, tilesW * ts, tilesH * ts);
    this.atlasFBO = createFBO(gl, this.atlasTex);
    this.atlasTileW = tilesW;
    this.atlasTileH = tilesH;
  }

  private timeWindow(): TimeWindow {
    return computeTimeWindow(this.wmt, this.state.t, this.opts);
  }

  private requestMissingTiles(tF: number, tC: number, tP: number): void {
    const variable = this.state.variable;
    const missing = (t: number): TileCoord[] => {
      const out: TileCoord[] = [];
      const seen = new Set<string>();
      for (const r of this.view) {
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

  private buildAtlas(tF: number, tC: number, frac: number): boolean {
    if (!this.atlasTex) return false;
    const gl = this.gl;
    const ts = this.wmt.tileSize;
    const aw = this.atlasTileW * ts;
    const ah = this.atlasTileH * ts;

    gl.bindFramebuffer(gl.FRAMEBUFFER, this.atlasFBO);
    gl.viewport(0, 0, aw, ah);
    gl.clearColor(NaN, 0, 0, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.disable(gl.BLEND);

    const tiles = this.view;
    let minX = Infinity, minY = Infinity;
    for (const t of tiles) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
    }

    const variable = this.state.variable;
    const lerp = frac > 0 && tF !== tC;
    const prog = lerp ? this.progAtlasLerp : this.progAtlas;
    gl.useProgram(prog);
    gl.bindVertexArray(this.quadVAO);

    let drawn = 0;
    for (const tile of tiles) {
      const refA = this.source.findTex(variable, tF, tile.z, tile.x, tile.y);
      if (!refA) continue;
      const refB = lerp
        ? this.source.findTex(variable, tC, tile.z, tile.x, tile.y) ?? refA
        : null;

      const cx = tile.x - minX;
      const cy = tile.y - minY;
      const x0 = (cx / this.atlasTileW) * 2 - 1;
      const y0 = (cy / this.atlasTileH) * 2 - 1;
      const x1 = ((cx + 1) / this.atlasTileW) * 2 - 1;
      const y1 = ((cy + 1) / this.atlasTileH) * 2 - 1;
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

  // Up to targetLevels of 2x2 missing-aware downsample, stops once a level
  // would drop below MIN_MIP_TEXEL on either axis.
  private buildPyramid(targetLevels: number): number {
    const gl = this.gl;
    const ts = this.wmt.tileSize;
    let w = this.atlasTileW * ts;
    let h = this.atlasTileH * ts;
    const cap = Math.min(targetLevels, MAX_MIP_LEVELS);

    gl.useProgram(this.progDown);
    gl.bindVertexArray(this.quadVAO);
    gl.disable(gl.BLEND);

    let prevTex = this.atlasTex;
    let levelsBuilt = 0;
    for (let i = 0; i < cap; i++) {
      const nw = w >> 1;
      const nh = h >> 1;
      if (nw < MIN_MIP_TEXEL || nh < MIN_MIP_TEXEL) break;

      let mip = this.mips[i];
      if (!mip || mip.w !== nw || mip.h !== nh) {
        if (mip) {
          gl.deleteTexture(mip.tex);
          gl.deleteFramebuffer(mip.fbo);
        }
        const tex = createTexture(gl, nw, nh);
        mip = { tex, fbo: createFBO(gl, tex), w: nw, h: nh };
        this.mips[i] = mip;
      }

      gl.bindFramebuffer(gl.FRAMEBUFFER, mip.fbo);
      gl.viewport(0, 0, nw, nh);
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, prevTex);
      gl.uniform1i(gl.getUniformLocation(this.progDown, "u_src"), 0);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);

      prevTex = mip.tex;
      w = nw;
      h = nh;
      levelsBuilt++;
    }
    return levelsBuilt;
  }

  private setHostFramebufferState(): void {
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    if (this.matrixMode) {
      beginHostFrame(gl, this.canvasW, this.canvasH, "premultiplied");
    } else {
      gl.viewport(0, 0, this.canvasW, this.canvasH);
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
    }
  }

  private setQuadProjection(
    prog: IsobarProgram,
    projData: GlobeProjectionData | null,
  ): void {
    const gl = this.gl;
    if (this.matrixMode && projData) {
      setProjectionUniforms(gl, prog.proj, projData);
      gl.uniform4f(
        gl.getUniformLocation(prog.program, "u_mercRect"),
        ...this.mercRect,
      );
    } else {
      gl.uniform2f(
        gl.getUniformLocation(prog.program, "u_screen"),
        this.canvasW,
        this.canvasH,
      );
      gl.uniform4f(
        gl.getUniformLocation(prog.program, "u_atlasRect"),
        ...this.atlasRectDevPx,
      );
    }
  }

  private drawQuad(): void {
    const gl = this.gl;
    if (this.matrixMode && this.contourMatrixVAO) {
      gl.bindVertexArray(this.contourMatrixVAO);
      gl.drawElements(
        gl.TRIANGLES,
        this.contourMatrixIndexCount,
        gl.UNSIGNED_SHORT,
        0,
      );
    } else {
      gl.bindVertexArray(this.quadVAO);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    }
  }

  private drawFill(
    sourceTex: WebGLTexture,
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    const gl = this.gl;
    this.setHostFramebufferState();

    const range = this.opts.fillRange ?? [
      Number.isFinite(this.state.variable.range.min)
        ? this.state.variable.range.min
        : 0,
      Number.isFinite(this.state.variable.range.max)
        ? this.state.variable.range.max
        : 1,
    ];
    // texels are stored as (real - baseline), so the range uniform shifts too
    const baseline = this.source.getBaseline(this.state.variable);

    const prog = this.fillPrograms.get(shaderData);
    gl.useProgram(prog.program);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, sourceTex);
    gl.uniform1i(gl.getUniformLocation(prog.program, "u_field"), 0);
    this.setQuadProjection(prog, projData);
    gl.uniform2f(
      gl.getUniformLocation(prog.program, "u_range"),
      range[0] - baseline,
      range[1] - baseline,
    );
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_alpha"),
      this.opts.fillAlpha,
    );
    this.drawQuad();
  }

  private drawContours(
    sourceTex: WebGLTexture,
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    const gl = this.gl;
    this.setHostFramebufferState();

    const spacing = this.effectiveSpacing();
    // baseline = K*spacing + phase decomposed in JS, shader sees small operands only
    const baseline = this.source.getBaseline(this.state.variable);
    const K = Math.floor(baseline / spacing);
    const phase = baseline - K * spacing;
    const majorEvery = this.opts.majorEvery;
    const kMod = majorEvery > 0 ? ((K % majorEvery) + majorEvery) % majorEvery : 0;

    const prog = this.contourPrograms.get(shaderData);
    gl.useProgram(prog.program);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, sourceTex);
    gl.uniform1i(gl.getUniformLocation(prog.program, "u_field"), 0);
    this.setQuadProjection(prog, projData);
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_spacing"),
      spacing,
    );
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_phase"),
      phase,
    );
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_kMod"),
      kMod,
    );
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_lineWidth"),
      this.opts.lineWidth *
        Math.min(window.devicePixelRatio || 1, 2),
    );
    gl.uniform3f(
      gl.getUniformLocation(prog.program, "u_lineColor"),
      this.opts.lineColor[0],
      this.opts.lineColor[1],
      this.opts.lineColor[2],
    );
    gl.uniform1f(gl.getUniformLocation(prog.program, "u_alpha"), this.opts.alpha);
    gl.uniform1f(
      gl.getUniformLocation(prog.program, "u_majorEvery"),
      this.opts.majorEvery,
    );
    this.drawQuad();
  }

  private clearCanvas(): void {
    if (this.matrixMode) return; // host owns the framebuffer
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.viewport(0, 0, this.canvasW, this.canvasH);
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
  }

  private drawInternal(
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    if (this.disposed) return;
    if (this.canvasW <= 1 || this.canvasH <= 1) return;
    const tStart = performance.now();
    if (this.view.length === 0) {
      this.clearCanvas();
      this.onFrame?.(performance.now() - tStart);
      return;
    }
    const { tF, tC, frac, tP } = this.timeWindow();
    this.requestMissingTiles(tF, tC, tP);
    if (!this.buildAtlas(tF, tC, frac)) {
      this.clearCanvas();
      this.onFrame?.(performance.now() - tStart);
      return;
    }

    const z = this.view[0]?.z ?? this.wmt.zoomRange.max;
    const cutLevels = Math.max(0, this.wmt.zoomRange.max - z);
    const target = Math.max(0, (this.opts.smoothness | 0) - cutLevels);
    this.builtMipCount = target > 0 ? this.buildPyramid(target) : 0;
    const sourceTex =
      this.builtMipCount === 0
        ? this.atlasTex
        : this.mips[this.builtMipCount - 1].tex;

    this.clearCanvas();
    if (this.opts.fillEnabled) this.drawFill(sourceTex, projData, shaderData);
    this.drawContours(sourceTex, projData, shaderData);
    this.onFrame?.(performance.now() - tStart);
  }
}
