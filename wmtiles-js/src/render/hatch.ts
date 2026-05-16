import type { TileCoord, Variable, WMT } from "../reader.js";
import type { RGB } from "./colormap.js";
import { buildQuadVAO, compileShader } from "./gl.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
import { TileSource, type TexRef, type TileSourceOptions } from "./source.js";
import { computeTimeWindow } from "./time.js";
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

export type HatchPattern =
  | "forward"
  | "backward"
  | "cross"
  | "horizontal"
  | "vertical"
  | "dots";

export interface HatchPatternBand {
  range: [number, number];
  pattern: HatchPattern;
  color?: RGB; // RGB 0..255, matching the colormap stop convention
  // line gap / stipple pitch, in CSS pixels
  spacing?: number;
  // line width / dot radius, in CSS pixels
  thickness?: number;
  alpha?: number;
}

export interface HatchIconBand {
  range: [number, number];
  // a URL the renderer loads, or an already-decoded image
  icon: string | TexImageSource;
  // cell pitch (icon-to-icon gap), in CSS pixels
  spacing?: number;
  // drawn icon size within the cell, in CSS pixels; defaults to spacing
  iconSize?: number;
  alpha?: number;
}

export type HatchBand = HatchPatternBand | HatchIconBand;

function isIconBand(b: HatchBand): b is HatchIconBand {
  return "icon" in b;
}

export interface HatchRendererOptions extends TileSourceOptions {
  bands?: HatchBand[];
  childFallback?: boolean;
  prefetchNext?: boolean;
  disableTimeLerp?: boolean;
  // device-pixels-per-CSS-pixel, so screen-space patterns match on retina
  pixelRatio?: number;
  // when true, the pattern is anchored to map content (zoom-aware) so the same
  // lat/lon shows the same pattern phase regardless of pan. Default false
  // (screen-locked).
  lockToMap?: boolean;
  onFrame?: (frameMs: number) => void;
  matrixMode?: boolean;
  onRedraw?: () => void;
}

export interface HatchRendererState {
  variable: Variable;
  t: number;
}

export interface TileDrawRect {
  z: number;
  x: number;
  y: number;
  worldX: number;
  sx0: number;
  sy0: number;
  sx1: number;
  sy1: number;
}

const DEFAULTS = {
  childFallback: true,
  prefetchNext: true,
  disableTimeLerp: false,
  pixelRatio: 1,
} as const;

const PATTERN_DEFAULTS = {
  color: [40, 40, 40] as RGB,
  spacing: 8,
  thickness: 2,
  alpha: 1,
};

const ICON_DEFAULTS = {
  spacing: 28,
  alpha: 1,
};

const PATTERN_KIND: Record<HatchPattern, number> = {
  forward: 0,
  backward: 1,
  cross: 2,
  horizontal: 3,
  vertical: 4,
  dots: 5,
};

// GLSL has no infinity literal; clamp to a large finite value just in case.
function glslFloat(v: number): string {
  if (!Number.isFinite(v)) return v > 0 ? "1e30" : "-1e30";
  // toFixed keeps it a float literal even for integers
  return v.toFixed(6);
}

// 1x1 transparent stand-in so an icon band can be bound and drawn (as nothing)
// while its real image is still loading.
function createIconTexture(gl: WebGL2RenderingContext): WebGLTexture {
  const tex = gl.createTexture();
  if (!tex) throw new Error("createTexture failed");
  gl.bindTexture(gl.TEXTURE_2D, tex);
  gl.texImage2D(
    gl.TEXTURE_2D, 0, gl.RGBA, 1, 1, 0, gl.RGBA, gl.UNSIGNED_BYTE,
    new Uint8Array([0, 0, 0, 0]),
  );
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  return tex;
}

function uploadIconImage(
  gl: WebGL2RenderingContext,
  tex: WebGLTexture,
  image: TexImageSource,
): void {
  gl.bindTexture(gl.TEXTURE_2D, tex);
  // a shared host context may have left these on; icon UVs assume the image's
  // natural top-left origin and straight (un-premultiplied) alpha
  gl.pixelStorei(gl.UNPACK_FLIP_Y_WEBGL, false);
  gl.pixelStorei(gl.UNPACK_PREMULTIPLY_ALPHA_WEBGL, false);
  gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, image);
}

function buildScreenVS(lockToMap: boolean): string {
  return `#version 300 es
precision highp float;
layout(location=0) in vec2 a_pos;
uniform vec2 u_screen;
uniform vec4 u_rect;
${lockToMap ? "uniform vec4 u_mapAnchor;  // (anchorX, anchorY, sizeX, sizeY) in device px" : ""}
out vec2 v_uv;
${lockToMap ? "out vec2 v_mapPx;" : ""}
void main() {
  vec2 px = mix(u_rect.xy, u_rect.zw, a_pos);
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  v_uv = a_pos;
  ${lockToMap ? "v_mapPx = u_mapAnchor.xy + a_pos * u_mapAnchor.zw;" : ""}
  gl_Position = vec4(ndc, 0.0, 1.0);
}`;
}

function buildMatrixVS(
  shaderData: GlobeShaderData,
  lockToMap: boolean,
): string {
  return `#version 300 es
precision highp float;
${shaderData.vertexShaderPrelude}
${shaderData.define}
layout(location=0) in vec2 a_pos;
uniform vec4 u_rect;
${lockToMap ? "uniform float u_mapWorldPx;  // mercator-world span in device px at current zoom" : ""}
out vec2 v_uv;
${lockToMap ? "out vec2 v_mapPx;" : ""}
void main() {
  vec2 mercator = mix(u_rect.xy, u_rect.zw, a_pos);
  v_uv = a_pos;
  ${lockToMap ? "v_mapPx = mercator * u_mapWorldPx;" : ""}
  gl_Position = projectTile(mercator);
}`;
}

function buildPatternGlsl(lockToMap: boolean): string {
  const coord = lockToMap ? "v_mapPx" : "gl_FragCoord.xy";
  return `
// perpendicular distance to the nearest line center, AA'd to ~1px
float lineCov(float coord, float spacing, float thickness) {
  float d = abs(mod(coord, spacing) - spacing * 0.5);
  float half_t = thickness * 0.5;
  return 1.0 - smoothstep(half_t - 1.0, half_t + 1.0, d);
}
// diagonals are projected so spacing/thickness stay true perpendicular px
float patternCov(int kind, float spacing, float thickness) {
  vec2 fc = ${coord};
  if (kind == 0) return lineCov((fc.x + fc.y) * 0.70710678, spacing, thickness);
  if (kind == 1) return lineCov((fc.x - fc.y) * 0.70710678, spacing, thickness);
  if (kind == 2) return max(
    lineCov((fc.x + fc.y) * 0.70710678, spacing, thickness),
    lineCov((fc.x - fc.y) * 0.70710678, spacing, thickness));
  if (kind == 3) return lineCov(fc.y, spacing, thickness);
  if (kind == 4) return lineCov(fc.x, spacing, thickness);
  // dots: stipple grid, thickness is the dot radius
  vec2 cell = mod(fc, spacing) - spacing * 0.5;
  return 1.0 - smoothstep(thickness - 1.0, thickness + 1.0, length(cell));
}
`;
}

function buildFS(
  bands: HatchBand[],
  premultiply: boolean,
  lockToMap: boolean,
): string {
  // icon bands each get a sampler, on texture units 2, 3, ... (0/1 are data)
  let iconIndex = 0;
  const samplerDecls: string[] = [];
  const coord = lockToMap ? "v_mapPx" : "gl_FragCoord.xy";

  const bandCode = bands
    .map((b, i) => {
      if (isIconBand(b)) {
        const k = iconIndex++;
        samplerDecls.push(`uniform sampler2D u_icon${k};`);
        const spacing = b.spacing ?? ICON_DEFAULTS.spacing;
        const iconSize = b.iconSize ?? spacing;
        const alpha = b.alpha ?? ICON_DEFAULTS.alpha;
        return `  // band ${i} (icon)
  if (v >= u_bandMin[${i}] && v < u_bandMax[${i}]) {
    float pitch = ${glslFloat(spacing)} * u_dpr;
    float isz = ${glslFloat(iconSize)} * u_dpr;
    vec2 cell = mod(${coord}, pitch) - 0.5 * pitch;
    vec2 q = cell / isz + 0.5;
    if (q.x >= 0.0 && q.x <= 1.0 && q.y >= 0.0 && q.y <= 1.0) {
      vec4 ic = texture(u_icon${k}, vec2(q.x, 1.0 - q.y));
      float a = ic.a * ${glslFloat(alpha)};
      acc.rgb = mix(acc.rgb, ic.rgb, a);
      acc.a = acc.a + a * (1.0 - acc.a);
    }
  }`;
      }
      const color = b.color ?? PATTERN_DEFAULTS.color;
      const spacing = b.spacing ?? PATTERN_DEFAULTS.spacing;
      const thickness = b.thickness ?? PATTERN_DEFAULTS.thickness;
      const alpha = b.alpha ?? PATTERN_DEFAULTS.alpha;
      const kind = PATTERN_KIND[b.pattern];
      const rgb =
        `vec3(${(color[0] / 255).toFixed(5)}, ` +
        `${(color[1] / 255).toFixed(5)}, ${(color[2] / 255).toFixed(5)})`;
      return `  // band ${i}
  if (v >= u_bandMin[${i}] && v < u_bandMax[${i}]) {
    float c = patternCov(${kind}, ${glslFloat(spacing)} * u_dpr, ` +
        `${glslFloat(thickness)} * u_dpr) * ${glslFloat(alpha)};
    acc.rgb = mix(acc.rgb, ${rgb}, c);
    acc.a = acc.a + c * (1.0 - acc.a);
  }`;
    })
    .join("\n");

  // band edges live in uniforms, not baked literals: samples reach the shader
  // shifted by the source baseline, so the draw call shifts the edges to match
  return `#version 300 es
precision highp float;
in vec2 v_uv;
${lockToMap ? "in vec2 v_mapPx;" : ""}
out vec4 outColor;
uniform sampler2D u_texA;
uniform sampler2D u_texB;
${samplerDecls.join("\n")}
uniform vec2 u_uvOffA;
uniform vec2 u_uvScaleA;
uniform vec2 u_uvOffB;
uniform vec2 u_uvScaleB;
uniform float u_lerp;
uniform float u_dpr;
uniform float u_bandMin[${bands.length}];
uniform float u_bandMax[${bands.length}];

${MISSING_GLSL_PREAMBLE}
${buildPatternGlsl(lockToMap)}

void main() {
  float vA = texture(u_texA, u_uvOffA + v_uv * u_uvScaleA).r;
  float vB = texture(u_texB, u_uvOffB + v_uv * u_uvScaleB).r;
  if (isMissing(vA) || isMissing(vB)) discard;
  float v = mix(vA, vB, u_lerp);
  vec4 acc = vec4(0.0);
${bandCode}
  if (acc.a <= 0.0) discard;
  ${premultiply ? "outColor = vec4(acc.rgb * acc.a, acc.a);" : "outColor = acc;"}
}`;
}

interface ProgramHandles {
  program: WebGLProgram;
  uScreen: WebGLUniformLocation | null;
  uRect: WebGLUniformLocation | null;
  uTexA: WebGLUniformLocation | null;
  uTexB: WebGLUniformLocation | null;
  uUvOffA: WebGLUniformLocation | null;
  uUvScaleA: WebGLUniformLocation | null;
  uUvOffB: WebGLUniformLocation | null;
  uUvScaleB: WebGLUniformLocation | null;
  uLerp: WebGLUniformLocation | null;
  uDpr: WebGLUniformLocation | null;
  uBandMin: WebGLUniformLocation | null;
  uBandMax: WebGLUniformLocation | null;
  // one sampler location per icon band, in band order
  uIcons: (WebGLUniformLocation | null)[];
  // lockToMap only: screen-mode per-tile anchor, matrix-mode world span
  uMapAnchor: WebGLUniformLocation | null;
  uMapWorldPx: WebGLUniformLocation | null;
  proj: ProjectionUniformLocs;
}

function buildProgram(
  gl: WebGL2RenderingContext,
  vsSrc: string,
  fsSrc: string,
  iconCount: number,
): ProgramHandles {
  const vs = compileShader(gl, gl.VERTEX_SHADER, vsSrc);
  const fs = compileShader(gl, gl.FRAGMENT_SHADER, fsSrc);
  const program = gl.createProgram();
  if (!program) throw new Error("createProgram failed");
  gl.attachShader(program, vs);
  gl.attachShader(program, fs);
  gl.linkProgram(program);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
    const log = gl.getProgramInfoLog(program) ?? "";
    throw new Error("link: " + log);
  }
  return {
    program,
    uScreen: gl.getUniformLocation(program, "u_screen"),
    uRect: gl.getUniformLocation(program, "u_rect"),
    uTexA: gl.getUniformLocation(program, "u_texA"),
    uTexB: gl.getUniformLocation(program, "u_texB"),
    uUvOffA: gl.getUniformLocation(program, "u_uvOffA"),
    uUvScaleA: gl.getUniformLocation(program, "u_uvScaleA"),
    uUvOffB: gl.getUniformLocation(program, "u_uvOffB"),
    uUvScaleB: gl.getUniformLocation(program, "u_uvScaleB"),
    uLerp: gl.getUniformLocation(program, "u_lerp"),
    uDpr: gl.getUniformLocation(program, "u_dpr"),
    uBandMin: gl.getUniformLocation(program, "u_bandMin"),
    uBandMax: gl.getUniformLocation(program, "u_bandMax"),
    uIcons: Array.from({ length: iconCount }, (_, k) =>
      gl.getUniformLocation(program, `u_icon${k}`),
    ),
    uMapAnchor: gl.getUniformLocation(program, "u_mapAnchor"),
    uMapWorldPx: gl.getUniformLocation(program, "u_mapWorldPx"),
    proj: getProjectionUniformLocs(gl, program),
  };
}

export class HatchRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly matrixMode: boolean;
  private readonly lockToMap: boolean;
  private readonly programs: VariantProgramCache<ProgramHandles>;
  private readonly scheduler: RedrawScheduler;
  private readonly vao: WebGLVertexArrayObject;
  private readonly indexCount: number;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly bands: HatchBand[];
  // band edges shifted by the source baseline, refreshed per draw
  private readonly bandMin: Float32Array;
  private readonly bandMax: Float32Array;
  // band indices that are icon bands, in band order; iconTextures[k] is the
  // sprite for iconBands[k], bound to texture unit 2 + k
  private readonly iconBands: number[];
  private readonly iconTextures: WebGLTexture[];

  private readonly opts: Required<
    Pick<
      HatchRendererOptions,
      "childFallback" | "prefetchNext" | "disableTimeLerp"
    >
  >;
  private readonly onFrame?: (frameMs: number) => void;
  private view: TileDrawRect[] = [];
  private disposed = false;
  private viewportW = 1;
  private viewportH = 1;
  private dpr: number;
  // matrix-mode world size in CSS px; the adapter refreshes it per draw()
  private worldSize = 0;

  state: HatchRendererState;

  constructor(
    gl: WebGL2RenderingContext,
    private readonly wmt: WMT,
    options?: HatchRendererOptions,
    source?: TileSource,
  ) {
    this.gl = gl;
    this.matrixMode = options?.matrixMode ?? false;
    this.lockToMap = options?.lockToMap ?? false;

    // one open-ended forward hatch is the do-nothing-surprising default
    this.bands = options?.bands?.length
      ? options.bands
      : [{ range: [-Infinity, Infinity], pattern: "forward" }];
    this.bandMin = new Float32Array(this.bands.length);
    this.bandMax = new Float32Array(this.bands.length);
    this.iconBands = [];
    this.bands.forEach((b, i) => {
      if (isIconBand(b)) this.iconBands.push(i);
    });
    this.iconTextures = this.iconBands.map(() => createIconTexture(gl));
    const iconCount = this.iconBands.length;
    this.opts = {
      childFallback: options?.childFallback ?? DEFAULTS.childFallback,
      prefetchNext: options?.prefetchNext ?? DEFAULTS.prefetchNext,
      disableTimeLerp: options?.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
    };
    this.dpr = options?.pixelRatio ?? DEFAULTS.pixelRatio;
    this.onFrame = options?.onFrame;

    const lockToMap = this.lockToMap;
    this.programs = new VariantProgramCache<ProgramHandles>(
      gl,
      this.matrixMode,
      {
        buildScreen: (g) =>
          buildProgram(
            g,
            buildScreenVS(lockToMap),
            buildFS(this.bands, false, lockToMap),
            iconCount,
          ),
        buildMatrix: (g, sd) =>
          buildProgram(
            g,
            buildMatrixVS(sd, lockToMap),
            buildFS(this.bands, true, lockToMap),
            iconCount,
          ),
        destroy: (g, p) => g.deleteProgram(p.program),
      },
    );
    this.scheduler = new RedrawScheduler(
      this.matrixMode,
      () => this.drawInternal(null, null),
      options?.onRedraw,
    );

    if (this.matrixMode) {
      const sub = buildSubdividedQuadVAO(gl, GLOBE_SEGMENTS);
      this.vao = sub.vao;
      this.indexCount = sub.indexCount;
    } else {
      this.vao = buildQuadVAO(gl);
      this.indexCount = 0;
    }

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, {
        ...options,
        onUpdate: () => this.scheduler.schedule(),
        shiftValuesByBaseline: true,
      });
      this.ownsSource = true;
    }

    if (!this.matrixMode) {
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
    }

    this.state = {
      variable: wmt.variables[0],
      t: 0,
    };

    // start icon loads; until each resolves the band draws its transparent
    // placeholder texture, then reschedules
    this.iconBands.forEach((bandIdx, k) => {
      this.loadIcon(
        (this.bands[bandIdx] as HatchIconBand).icon,
        this.iconTextures[k],
      );
    });
  }

  private loadIcon(src: string | TexImageSource, tex: WebGLTexture): void {
    if (typeof src !== "string") {
      uploadIconImage(this.gl, tex, src);
      return;
    }
    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = (): void => {
      if (this.disposed) return;
      uploadIconImage(this.gl, tex, img);
      this.scheduler.schedule();
    };
    img.onerror = (): void => {
      console.error(`wmtiles: failed to load hatch icon "${src}"`);
    };
    img.src = src;
  }

  setState(patch: Partial<HatchRendererState>): void {
    if (this.disposed) return;
    Object.assign(this.state, patch);
    this.source.invalidate();
    this.scheduler.schedule();
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    this.view = tiles;
    this.scheduler.schedule();
  }

  setPixelRatio(ratio: number): void {
    if (this.disposed) return;
    const r = ratio > 0 ? ratio : 1;
    if (this.dpr === r) return;
    this.dpr = r;
    this.scheduler.schedule();
  }

  setViewport(widthDevPx: number, heightDevPx: number): void {
    if (this.disposed) return;
    const w = Math.max(1, widthDevPx | 0);
    const h = Math.max(1, heightDevPx | 0);
    if (this.viewportW === w && this.viewportH === h) return;
    this.viewportW = w;
    this.viewportH = h;
    if (!this.matrixMode) {
      this.gl.viewport(0, 0, w, h);
      this.scheduler.schedule();
    }
  }

  draw(
    projData: GlobeProjectionData,
    shaderData: GlobeShaderData,
    viewportWPx: number,
    viewportHPx: number,
    worldSize = 0,
  ): void {
    if (this.disposed || !this.matrixMode) return;
    this.viewportW = Math.max(1, viewportWPx | 0);
    this.viewportH = Math.max(1, viewportHPx | 0);
    this.worldSize = worldSize;
    this.drawInternal(projData, shaderData);
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.scheduler.dispose();
    this.programs.dispose();
    this.gl.deleteVertexArray(this.vao);
    for (const tex of this.iconTextures) this.gl.deleteTexture(tex);
    if (this.ownsSource) this.source.dispose();
  }

  private drawPair(
    prog: ProgramHandles,
    A: TexRef,
    B: TexRef,
    lerp: number,
    sx0: number,
    sy0: number,
    sx1: number,
    sy1: number,
    worldX: number,
    y: number,
  ): void {
    const gl = this.gl;
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, A.tex);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, B.tex);
    gl.uniform4f(prog.uRect, sx0, sy0, sx1, sy1);
    gl.uniform2f(prog.uUvOffA, A.ox, A.oy);
    gl.uniform2f(prog.uUvScaleA, A.s, A.s);
    gl.uniform2f(prog.uUvOffB, B.ox, B.oy);
    gl.uniform2f(prog.uUvScaleB, B.s, B.s);
    gl.uniform1f(prog.uLerp, lerp);
    if (this.lockToMap && !this.matrixMode && prog.uMapAnchor) {
      const sw = sx1 - sx0;
      const sh = sy1 - sy0;
      gl.uniform4f(prog.uMapAnchor, worldX * sw, y * sh, sw, sh);
    }
    if (this.matrixMode) {
      gl.drawElements(gl.TRIANGLES, this.indexCount, gl.UNSIGNED_SHORT, 0);
    } else {
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    }
  }

  private attempt(
    prog: ProgramHandles,
    variable: Variable,
    tF: number,
    tC: number,
    frac: number,
    z: number,
    x: number,
    y: number,
    sx0: number,
    sy0: number,
    sx1: number,
    sy1: number,
    worldX: number,
  ): boolean {
    const A = this.source.findTex(variable, tF, z, x, y);
    const B = frac > 0 ? this.source.findTex(variable, tC, z, x, y) : A;
    if (A && B) {
      this.drawPair(prog, A, B, frac, sx0, sy0, sx1, sy1, worldX, y);
      return true;
    }
    if (A) {
      this.drawPair(prog, A, A, 0, sx0, sy0, sx1, sy1, worldX, y);
      return true;
    }
    if (B) {
      this.drawPair(prog, B, B, 0, sx0, sy0, sx1, sy1, worldX, y);
      return true;
    }
    return false;
  }

  private drawInternal(
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    const gl = this.gl;
    if (this.viewportW <= 1 || this.viewportH <= 1) return;
    const prog = this.programs.get(shaderData);
    const tStart = performance.now();

    if (this.matrixMode) {
      beginHostFrame(gl, this.viewportW, this.viewportH, "premultiplied");
    } else {
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
    }

    const view = this.view;
    if (view.length === 0) {
      this.onFrame?.(performance.now() - tStart);
      return;
    }

    const { variable, t } = this.state;
    const { tF, tC, frac, tP } = computeTimeWindow(this.wmt, t, this.opts);

    // samples were shifted in source.ts; shift band edges to match. open ends
    // stay clamped far outside any real range.
    const baseline = this.source.getBaseline(variable);
    for (let i = 0; i < this.bands.length; i++) {
      const [lo, hi] = this.bands[i].range;
      this.bandMin[i] = Number.isFinite(lo) ? lo - baseline : -1e30;
      this.bandMax[i] = Number.isFinite(hi) ? hi - baseline : 1e30;
    }

    gl.useProgram(prog.program);
    gl.bindVertexArray(this.vao);
    if (this.matrixMode && projData) {
      setProjectionUniforms(gl, prog.proj, projData);
    } else {
      gl.uniform2f(prog.uScreen, this.viewportW, this.viewportH);
    }
    gl.uniform1f(prog.uDpr, this.dpr);
    gl.uniform1fv(prog.uBandMin, this.bandMin);
    gl.uniform1fv(prog.uBandMax, this.bandMax);
    gl.uniform1i(prog.uTexA, 0);
    gl.uniform1i(prog.uTexB, 1);
    if (this.lockToMap && this.matrixMode && prog.uMapWorldPx) {
      gl.uniform1f(prog.uMapWorldPx, this.worldSize * this.dpr);
    }
    // icon sprites stay bound for the whole tile loop on units 2, 3, ...;
    // drawPair only touches units 0/1
    for (let k = 0; k < this.iconTextures.length; k++) {
      gl.activeTexture(gl.TEXTURE0 + 2 + k);
      gl.bindTexture(gl.TEXTURE_2D, this.iconTextures[k]);
      gl.uniform1i(prog.uIcons[k], 2 + k);
    }

    const maxZ = this.wmt.zoomRange.max;
    const childOK = this.opts.childFallback;
    const missingF: TileCoord[] = [];
    const missingC: TileCoord[] = [];
    const missingP: TileCoord[] = [];
    const seenF = new Set<string>();
    const seenC = new Set<string>();
    const seenP = new Set<string>();

    for (const r of view) {
      const drew = this.attempt(
        prog,
        variable, tF, tC, frac,
        r.z, r.x, r.y,
        r.sx0, r.sy0, r.sx1, r.sy1,
        r.worldX,
      );

      if (!drew && childOK && r.z + 1 <= maxZ) {
        const cz = r.z + 1;
        const w = r.sx1 - r.sx0;
        const h = r.sy1 - r.sy0;
        for (let cy = 0; cy < 2; cy++) {
          for (let cx = 0; cx < 2; cx++) {
            this.attempt(
              prog,
              variable, tF, tC, frac, cz,
              r.x * 2 + cx, r.y * 2 + cy,
              r.sx0 + (cx * w) / 2,
              r.sy0 + (cy * h) / 2,
              r.sx0 + ((cx + 1) * w) / 2,
              r.sy0 + ((cy + 1) * h) / 2,
              r.worldX * 2 + cx,
            );
          }
        }
      }

      const baseKey = `${r.z}|${r.x}|${r.y}`;
      if (!this.source.hasExact(variable, tF, r.z, r.x, r.y) && !seenF.has(baseKey)) {
        seenF.add(baseKey);
        missingF.push({ z: r.z, x: r.x, y: r.y });
      }
      if (frac > 0 && !this.source.hasExact(variable, tC, r.z, r.x, r.y) && !seenC.has(baseKey)) {
        seenC.add(baseKey);
        missingC.push({ z: r.z, x: r.x, y: r.y });
      }
      if (tP >= 0 && !this.source.hasExact(variable, tP, r.z, r.x, r.y) && !seenP.has(baseKey)) {
        seenP.add(baseKey);
        missingP.push({ z: r.z, x: r.x, y: r.y });
      }
    }

    if (missingF.length > 0) this.source.requestTiles(variable, tF, missingF);
    if (missingC.length > 0) this.source.requestTiles(variable, tC, missingC);
    if (tP >= 0 && missingP.length > 0) {
      this.source.requestTiles(variable, tP, missingP);
    }
    this.onFrame?.(performance.now() - tStart);
  }
}
