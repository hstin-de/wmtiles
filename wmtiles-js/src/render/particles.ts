import type { TileCoord, Variable, WMT } from "../reader.js";
import {
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
import { colormapToGLSL } from "./colormap.js";
import { TileSource, type TileSourceOptions } from "./source.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
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
  setProjectionUniforms,
  getProjectionUniformLocs,
  type GlobeProjectionData,
  type GlobeShaderData,
  type ProjectionUniformLocs,
} from "./globe.js";
import { beginHostFrame, VariantProgramCache } from "./backend.js";

const GLOBE_SEGMENTS = 16; // subdivision per composite step, for globe curvature

// screen mode: fullscreen blit. matrix mode: reprojects the trail buffer via
// projectTile()
interface CompositeProgram {
  program: WebGLProgram;
  proj: ProjectionUniformLocs; // all-null in screen mode
}

export type { TileDrawRect } from "./heatmap.js";

export interface ParticlesRendererOptions extends TileSourceOptions {
  particleCount?: number;
  particleSize?: number;
  fadeOpacity?: number;
  speedFactor?: number;
  maxAgeFrames?: number;
  colormap?: Colormap | BuiltinColormapName;
  speedRange?: [number, number];
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
  // cpu time per tick, skipped on early-exit frames
  onFrame?: (frameMs: number) => void;
  // When true: caller drives drawing via draw(matrix, ...), tile rects are
  // mercator (0..1), and no internal rAF loop runs.
  matrixMode?: boolean;
  onRedraw?: () => void;
}

export interface ParticlesRendererState {
  uVar: Variable;
  vVar: Variable;
  t: number;
}

const DEFAULTS = {
  particleCount: 4096,
  particleSize: 1.5,
  fadeOpacity: 0.96,
  speedFactor: 0.0005,
  maxAgeFrames: 100,
  speedRange: [0, 30] as [number, number],
  disableTimeLerp: false,
  prefetchNext: true,
} as const;

// RG wind atlas: one VS_ATLAS_SLOT pass per tile, samples u and v tiles into RG.
const FS_ATLAS = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_texU;
uniform sampler2D u_texV;
uniform vec4 u_uvU;   // ox,oy,sx,sy
uniform vec4 u_uvV;
${MISSING_GLSL_PREAMBLE}
void main() {
  float u = texture(u_texU, u_uvU.xy + v_uv * u_uvU.zw).r;
  float v = texture(u_texV, u_uvV.xy + v_uv * u_uvV.zw).r;
  outColor = packRG(u, v);
}`;

// Time-lerp: build atlas as mix(A, B) per slot.
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

const FS_PARTICLE_UPDATE = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_state;
uniform sampler2D u_atlas;
uniform float u_stepFactor;     // tile-units / frame / unit wind
uniform float u_ageStep;
uniform float u_rand;
uniform vec2  u_atlasTileSize;  // atlas size in tile units (e.g. 1..4)
uniform float u_atlasZoomScale; // 2^(curZoom - prevCurZoom), 1 if unchanged
uniform vec2  u_atlasDelta;     // newMin - oldMin * scale, in tile units

${MISSING_GLSL_PREAMBLE}

// "Hash without sine" by David Hoskins; FP16-safe (every intermediate is in
// low single-digit magnitude where FP16 has 4-5 digits of resolution).
vec3 hash33(vec2 p) {
  vec3 p3 = fract(vec3(p.xyx) * vec3(0.1031, 0.1030, 0.0973));
  p3 += dot(p3, p3.yxz + 33.33);
  return fract((p3.xxy + p3.yxx) * p3.zyx);
}

void main() {
  vec4 s = texture(u_state, v_uv);
  // Rebase atlas-relative position from previous frame's atlas to current
  // frame's atlas. For an unchanged atlas, scale=1 and delta=0 → pos = s.xy.
  vec2 pos = s.xy * u_atlasZoomScale - u_atlasDelta;
  float age = s.z;

  vec2 atlasUV = pos / u_atlasTileSize;
  bool inAtlas = atlasUV.x >= 0.0 && atlasUV.x <= 1.0
              && atlasUV.y >= 0.0 && atlasUV.y <= 1.0;
  vec2 wind = inAtlas ? texture(u_atlas, atlasUV).rg : vec2(0.0);
  bool noData = !inAtlas || isMissing(wind.x) || isMissing(wind.y);

  // advect in tile units. Flip v: wind is north-positive, mercator y is
  // south-positive
  vec2 step = noData ? vec2(0.0) : vec2(wind.x, -wind.y) * u_stepFactor;
  vec2 newPos = pos + step;
  age += u_ageStep;
  bool dead = age >= 1.0 || noData;
  if (dead) {
    vec3 r = hash33(v_uv * 17.0 + vec2(u_rand + s.w * 7.13, u_rand * 3.7));
    newPos = r.xy * u_atlasTileSize;
    age = r.z * 0.5;
  }
  outColor = vec4(newPos, age, s.w);
}`;

// Screen-mode draw. Particle state is in atlas-relative tile coords.
// atlasUV is just pos / atlasTileSize. Map to canvas pixels via u_atlasRect.
const VS_PARTICLE_DRAW_SCREEN = `#version 300 es
precision highp float;
uniform sampler2D u_state;
uniform sampler2D u_atlas;
uniform int u_texSize;
uniform vec4 u_atlasRect;
uniform vec2 u_screen;
uniform float u_pointSize;
uniform vec2 u_speedRange;
uniform vec2 u_atlasTileSize;
out float v_t;
out float v_age;
flat out int v_alive;

${MISSING_GLSL_PREAMBLE}

void main() {
  int i = gl_VertexID;
  int x = i % u_texSize;
  int y = i / u_texSize;
  vec4 s = texelFetch(u_state, ivec2(x, y), 0);
  vec2 pos = s.xy;

  vec2 atlasUV = pos / u_atlasTileSize;
  bool outOfAtlas = atlasUV.x < 0.0 || atlasUV.x > 1.0
                 || atlasUV.y < 0.0 || atlasUV.y > 1.0;
  vec2 wind = outOfAtlas ? vec2(0.0) : texture(u_atlas, atlasUV).rg;
  bool noData = outOfAtlas || isMissing(wind.x) || isMissing(wind.y);
  v_alive = noData ? 0 : 1;
  float speed = length(wind);
  v_t = clamp((speed - u_speedRange.x) / max(u_speedRange.y - u_speedRange.x, 1e-30), 0.0, 1.0);
  v_age = s.z;
  if (noData) {
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0);
    gl_PointSize = 0.0;
    return;
  }
  vec2 px = mix(u_atlasRect.xy, u_atlasRect.zw, atlasUV);
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  gl_Position = vec4(ndc, 0.0, 1.0);
  gl_PointSize = u_pointSize;
}`;

const VS_PARTICLE_DRAW_MATRIX = `#version 300 es
precision highp float;
uniform sampler2D u_state;
uniform sampler2D u_atlas;
uniform int u_texSize;
uniform float u_pointSize;
uniform vec2 u_speedRange;
uniform vec2 u_atlasTileSize;
out float v_t;
out float v_age;
flat out int v_alive;

${MISSING_GLSL_PREAMBLE}

void main() {
  int i = gl_VertexID;
  int x = i % u_texSize;
  int y = i / u_texSize;
  vec4 s = texelFetch(u_state, ivec2(x, y), 0);
  vec2 pos = s.xy;

  vec2 atlasUV = pos / u_atlasTileSize;
  bool outOfAtlas = atlasUV.x < 0.0 || atlasUV.x > 1.0
                 || atlasUV.y < 0.0 || atlasUV.y > 1.0;
  vec2 wind = outOfAtlas ? vec2(0.0) : texture(u_atlas, atlasUV).rg;
  bool noData = outOfAtlas || isMissing(wind.x) || isMissing(wind.y);
  v_alive = noData ? 0 : 1;
  float speed = length(wind);
  v_t = clamp((speed - u_speedRange.x) / max(u_speedRange.y - u_speedRange.x, 1e-30), 0.0, 1.0);
  v_age = s.z;
  if (noData) {
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0);
    gl_PointSize = 0.0;
    return;
  }
  gl_Position = vec4(atlasUV * 2.0 - 1.0, 0.0, 1.0);
  gl_PointSize = u_pointSize;
}`;

// matrix-mode composite: draw a subdivided quad over the trail buffer's
// mercator extent and let projectTile() do the screen projection
function buildMatrixCompositeVS(shaderData: GlobeShaderData): string {
  return `#version 300 es
precision highp float;
${shaderData.vertexShaderPrelude}
${shaderData.define}
layout(location=0) in vec2 a_pos;
uniform vec4 u_mercExtent;
out vec2 v_uv;
void main() {
  v_uv = a_pos;
  vec2 merc = mix(u_mercExtent.xy, u_mercExtent.zw, a_pos);
  gl_Position = projectTile(merc);
}`;
}

function buildParticleFS(colormap: Colormap, premultiply: boolean): string {
  return `#version 300 es
precision highp float;
in float v_t;
in float v_age;
flat in int v_alive;
out vec4 outColor;

${colormapToGLSL(colormap)}

void main() {
  if (v_alive == 0) discard;
  vec2 c = gl_PointCoord - vec2(0.5);
  float d = dot(c, c);
  if (d > 0.25) discard;
  float fade = smoothstep(0.25, 0.08, d);
  vec3 col = colormap(v_t);
  ${premultiply
    ? "outColor = vec4(col * fade, fade);"
    : "outColor = vec4(col, fade);"}
}`;
}

const FS_TRAIL_FADE = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_prev;
uniform float u_fade;
uniform mat3 u_warp;
void main() {
  vec3 h = u_warp * vec3(v_uv, 1.0);
  // points behind the camera (or projecting through the camera origin) have
  // non-positive w after the warp; treat as no previous content.
  if (h.z <= 0.0) {
    outColor = vec4(0.0);
    return;
  }
  vec2 shifted = h.xy / h.z;
  // out-of-buffer: treat as empty so we don't smear with CLAMP_TO_EDGE.
  if (shifted.x < 0.0 || shifted.x > 1.0 || shifted.y < 0.0 || shifted.y > 1.0) {
    outColor = vec4(0.0);
    return;
  }
  vec4 c = texture(u_prev, shifted);
  outColor = vec4(c.rgb * u_fade, c.a * u_fade);
}`;

const FS_COMPOSITE = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_tex;
void main() {
  outColor = texture(u_tex, v_uv);
}`;

interface PingPong {
  fbos: [WebGLFramebuffer, WebGLFramebuffer];
  texs: [WebGLTexture, WebGLTexture];
  i: 0 | 1;
}

function probeRgba32fFbo(gl: WebGL2RenderingContext): boolean {
  const tex = gl.createTexture();
  const fbo = gl.createFramebuffer();
  if (!tex || !fbo) {
    if (tex) gl.deleteTexture(tex);
    if (fbo) gl.deleteFramebuffer(fbo);
    return false;
  }
  const prevTexBinding = gl.getParameter(gl.TEXTURE_BINDING_2D);
  const prevFbo = gl.getParameter(gl.FRAMEBUFFER_BINDING);
  let ok = false;
  try {
    gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.texImage2D(
      gl.TEXTURE_2D, 0, gl.RGBA32F, 4, 4, 0, gl.RGBA, gl.FLOAT, null,
    );
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
    gl.bindFramebuffer(gl.FRAMEBUFFER, fbo);
    gl.framebufferTexture2D(
      gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, tex, 0,
    );
    ok = gl.checkFramebufferStatus(gl.FRAMEBUFFER) === gl.FRAMEBUFFER_COMPLETE;
  } finally {
    gl.bindFramebuffer(gl.FRAMEBUFFER, prevFbo);
    gl.bindTexture(gl.TEXTURE_2D, prevTexBinding);
    gl.deleteFramebuffer(fbo);
    gl.deleteTexture(tex);
  }
  return ok;
}

// column-major 3x3, the shape the trail-fade shader expects for u_warp
type Mat3 = Float32Array;
const IDENTITY3: Mat3 = new Float32Array([1, 0, 0, 0, 1, 0, 0, 0, 1]);

export class ParticlesRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;
  private readonly matrixMode: boolean;
  private readonly onRedraw?: () => void;

  private readonly opts: Required<
    Pick<
      ParticlesRendererOptions,
      | "particleCount"
      | "particleSize"
      | "fadeOpacity"
      | "speedFactor"
      | "maxAgeFrames"
      | "speedRange"
      | "disableTimeLerp"
      | "prefetchNext"
    >
  >;
  private readonly colormap: Colormap;
  private readonly texSize: number;
  private readonly onFrame?: (frameMs: number) => void;
  // GPU can't reliably render FP32 particle state, so storage falls back to
  // RGBA16F (see probeRgba32fFbo)
  private readonly fp16State: boolean;

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly progParticleUpdate: WebGLProgram;
  // screen-space NDC output (screen) vs mercator-space trail buffer (matrix)
  private readonly progParticleDraw: WebGLProgram;
  private readonly progTrailFade: WebGLProgram;
  private readonly compositePrograms: VariantProgramCache<CompositeProgram>;
  // set once in screen mode, refreshed per draw() in matrix mode
  private currentComposite: CompositeProgram | null = null;
  // subdivided quad so the matrix composite curves on globe
  private readonly compositeMatrixVAO: WebGLVertexArrayObject | null;
  private readonly compositeMatrixIndexCount: number;

  private readonly quadVAO: WebGLVertexArrayObject;
  private readonly emptyVAO: WebGLVertexArrayObject;

  private particles: PingPong;
  private trails!: PingPong;
  private atlasTex!: WebGLTexture;
  private atlasFBO!: WebGLFramebuffer;
  private atlasTileW = 0;
  private atlasTileH = 0;
  private canvasW = 1;
  private canvasH = 1;
  private trailsAllocatedFor = "0x0";

  private view: TileDrawRect[] = [];
  private atlasRectDevPx: [number, number, number, number] = [0, 0, 1, 1];
  private raf = 0;
  private running = false;
  private disposed = false;
  private firstFrame = true;
  private prevViewZoom = -1;
  // see computeWarp
  private warp: Mat3 = new Float32Array(IDENTITY3);
  private prevLayerOrigin: [number, number] | null = null;
  private prevMercExtent: [number, number, number, number] | null = null;
  // previous frame's atlas min-tile / zoom, for the rebase uniforms
  private prevAtlasMinTile: [number, number] | null = null;
  private prevAtlasCurZoom: number | null = null;

  state: ParticlesRendererState;

  constructor(
    gl: WebGL2RenderingContext,
    wmt: WMT,
    options: ParticlesRendererOptions = {},
    source?: TileSource,
  ) {
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    // OES_texture_float_linear: not fatal, falls back to nearest on some GPUs
    gl.getExtension("OES_texture_float_linear");
    this.gl = gl;
    this.wmt = wmt;
    this.matrixMode = options.matrixMode ?? false;
    this.onRedraw = options.onRedraw;

    this.opts = {
      particleCount: options.particleCount ?? DEFAULTS.particleCount,
      particleSize: options.particleSize ?? DEFAULTS.particleSize,
      fadeOpacity: options.fadeOpacity ?? DEFAULTS.fadeOpacity,
      speedFactor: options.speedFactor ?? DEFAULTS.speedFactor,
      maxAgeFrames: options.maxAgeFrames ?? DEFAULTS.maxAgeFrames,
      speedRange: options.speedRange ?? DEFAULTS.speedRange,
      disableTimeLerp:
        options.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
      prefetchNext: options.prefetchNext ?? DEFAULTS.prefetchNext,
    };
    this.onFrame = options.onFrame;
    this.colormap = resolveColormap(options.colormap);
    this.fp16State = !probeRgba32fFbo(gl);

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, options);
      this.ownsSource = true;
    }

    this.texSize = Math.max(2, Math.ceil(Math.sqrt(this.opts.particleCount)));

    this.progAtlas = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS);
    this.progAtlasLerp = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS_LERP);
    this.progParticleUpdate = linkProgram(gl, VS_FULLSCREEN, FS_PARTICLE_UPDATE);
    this.progParticleDraw = linkProgram(
      gl,
      this.matrixMode ? VS_PARTICLE_DRAW_MATRIX : VS_PARTICLE_DRAW_SCREEN,
      buildParticleFS(this.colormap, false),
    );
    this.progTrailFade = linkProgram(gl, VS_FULLSCREEN, FS_TRAIL_FADE);
    this.compositePrograms = new VariantProgramCache<CompositeProgram>(
      gl,
      this.matrixMode,
      {
        buildScreen: (g) => {
          const program = linkProgram(g, VS_FULLSCREEN, FS_COMPOSITE);
          return { program, proj: getProjectionUniformLocs(g, program) };
        },
        buildMatrix: (g, sd) => {
          const program = linkProgram(g, buildMatrixCompositeVS(sd), FS_COMPOSITE);
          return { program, proj: getProjectionUniformLocs(g, program) };
        },
        destroy: (g, p) => g.deleteProgram(p.program),
      },
    );
    if (!this.matrixMode) this.currentComposite = this.compositePrograms.get(null);
    if (this.matrixMode) {
      const sub = buildSubdividedQuadVAO(gl, GLOBE_SEGMENTS);
      this.compositeMatrixVAO = sub.vao;
      this.compositeMatrixIndexCount = sub.indexCount;
    } else {
      this.compositeMatrixVAO = null;
      this.compositeMatrixIndexCount = 0;
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

    const ev = gl.createVertexArray();
    if (!ev) throw new Error("createVertexArray failed");
    this.emptyVAO = ev;

    this.particles = this.allocParticles();

    this.state = {
      uVar: wmt.variables[0],
      vVar: wmt.variables[0],
      t: 0,
    };

    this.ensureTrails(1, 1);
  }

  setState(patch: Partial<ParticlesRendererState>): void {
    if (this.disposed) return;
    Object.assign(this.state, patch);
    this.source.invalidate();
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    // screen mode trails don't survive a zoom change (canvas-px to world
    // mapping scales); matrix mode's mercator warp handles zoom, so no clear
    const newZoom = tiles[0]?.z ?? -1;
    if (
      !this.matrixMode &&
      newZoom !== this.prevViewZoom &&
      this.prevViewZoom !== -1
    ) {
      this.firstFrame = true;
    }
    this.prevViewZoom = newZoom;
    if (tiles.length === 0) {
      this.view = tiles;
      this.atlasRectDevPx = [0, 0, 1, 1];
      return;
    }
    // canonical world only (worldX === x). Drop worldCopyJump wrap copies so
    // the atlas stays one contiguous grid and doesn't jump between copies.
    const uniq = tiles.filter((t) => t.worldX === t.x);
    if (uniq.length === 0) {
      this.view = tiles;
      this.atlasRectDevPx = [0, 0, 1, 1];
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
    this.atlasRectDevPx = [sx0, sy0, sx1, sy1];
    this.ensureAtlas(uniq);
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
    }
    this.ensureTrails(w, h);
    // Resizing invalidates the trail buffer; clear next frame.
    this.firstFrame = true;
  }

  // Matrix-mode entry point. Call from the host's render() hook each frame.
  draw(
    projData: GlobeProjectionData,
    shaderData: GlobeShaderData,
    viewportWPx: number,
    viewportHPx: number,
  ): void {
    if (this.disposed || !this.matrixMode) return;
    const w = Math.max(1, viewportWPx | 0);
    const h = Math.max(1, viewportHPx | 0);
    if (w !== this.canvasW || h !== this.canvasH) {
      this.canvasW = w;
      this.canvasH = h;
      this.ensureTrails(w, h);
      this.firstFrame = true;
    }
    // composite is the only pass that needs the projection prelude
    this.currentComposite = this.compositePrograms.get(shaderData);
    this.tick(projData);
    // Keep the animation going. Adapter wires onRedraw to map.triggerRepaint.
    if (this.running) this.onRedraw?.();
  }

  start(): void {
    if (this.disposed || this.running) return;
    this.running = true;
    this.firstFrame = true;
    if (this.matrixMode) {
      // Host drives draw() per frame; just kick the first repaint.
      this.onRedraw?.();
      return;
    }
    const loop = (): void => {
      if (!this.running || this.disposed) return;
      this.tick(null);
      this.raf = requestAnimationFrame(loop);
    };
    this.raf = requestAnimationFrame(loop);
  }

  stop(): void {
    this.running = false;
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.stop();
    const gl = this.gl;
    for (const t of this.particles.texs) gl.deleteTexture(t);
    for (const f of this.particles.fbos) gl.deleteFramebuffer(f);
    if (this.trails) {
      for (const t of this.trails.texs) gl.deleteTexture(t);
      for (const f of this.trails.fbos) gl.deleteFramebuffer(f);
    }
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    gl.deleteVertexArray(this.quadVAO);
    gl.deleteVertexArray(this.emptyVAO);
    if (this.compositeMatrixVAO) gl.deleteVertexArray(this.compositeMatrixVAO);
    gl.deleteProgram(this.progAtlas);
    gl.deleteProgram(this.progAtlasLerp);
    gl.deleteProgram(this.progParticleUpdate);
    gl.deleteProgram(this.progParticleDraw);
    gl.deleteProgram(this.progTrailFade);
    this.compositePrograms.dispose();
    if (this.ownsSource) this.source.dispose();
  }

  private computeWarp(): void {
    if (this.firstFrame) {
      this.warp.set(IDENTITY3);
      return;
    }
    if (this.matrixMode && this.prevMercExtent) {
      const c = this.atlasRectDevPx; // current mercExtent (in matrix mode)
      const p = this.prevMercExtent;
      const cw = c[2] - c[0];
      const ch = c[3] - c[1];
      const pw = p[2] - p[0];
      const ph = p[3] - p[1];
      if (cw <= 0 || ch <= 0 || pw <= 0 || ph <= 0) {
        this.warp.set(IDENTITY3);
        return;
      }
      // old_uv = a * new_uv + b, per axis
      const ax = cw / pw;
      const bx = (c[0] - p[0]) / pw;
      const ay = ch / ph;
      const by = (c[1] - p[1]) / ph;
      this.warp.set([ax, 0, 0, 0, ay, 0, bx, by, 1]);
      return;
    }
    if (!this.matrixMode && this.prevLayerOrigin && this.view.length > 0) {
      const ref = this.view[0];
      const tilePxW = ref.sx1 - ref.sx0;
      const tilePxH = ref.sy1 - ref.sy0;
      const lx = ref.sx0 - ref.x * tilePxW;
      const ly = ref.sy0 - ref.y * tilePxH;
      const dx = -(lx - this.prevLayerOrigin[0]) / this.canvasW;
      const dy = (ly - this.prevLayerOrigin[1]) / this.canvasH;
      this.warp.set([1, 0, 0, 0, 1, 0, dx, dy, 1]);
      return;
    }
    this.warp.set(IDENTITY3);
  }

  private tick(projData: GlobeProjectionData | null): void {
    if (this.canvasW <= 1 || this.canvasH <= 1) return;
    if (this.view.length === 0) return;
    const tStart = performance.now();

    this.computeWarp();

    // Stash this frame's mercExtent / layer origin for next frame's warp.
    if (this.matrixMode) {
      const c = this.atlasRectDevPx;
      this.prevMercExtent = [c[0], c[1], c[2], c[3]];
    } else if (this.view.length > 0) {
      const ref = this.view[0];
      const tilePxW = ref.sx1 - ref.sx0;
      const tilePxH = ref.sy1 - ref.sy0;
      this.prevLayerOrigin = [
        ref.sx0 - ref.x * tilePxW,
        ref.sy0 - ref.y * tilePxH,
      ];
    }

    const { tF, tC, frac, tP } = this.timeWindow();
    this.requestMissingTiles(tF, tC, tP);
    if (!this.buildAtlas(tF, tC, frac)) return;

    this.updateParticles();
    this.drawTrailAndParticles(projData);
    this.compositeToHost(projData);
    this.firstFrame = false;
    this.onFrame?.(performance.now() - tStart);
  }

  private timeWindow(): TimeWindow {
    return computeTimeWindow(this.wmt, this.state.t, this.opts);
  }

  private requestMissingTiles(tF: number, tC: number, tP: number): void {
    const { uVar, vVar } = this.state;
    const missing = (t: number, vari: Variable): TileCoord[] => {
      const out: TileCoord[] = [];
      const seen = new Set<string>();
      for (const r of this.view) {
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

  private allocParticles(): PingPong {
    const gl = this.gl;
    const n = this.texSize;
    const init = new Float32Array(n * n * 4);
    // age near 1.0: particles die on the first update and respawn inside the
    // real atlas, since atlasTileSize isn't known yet. s.w seeds the respawn
    for (let i = 0; i < n * n; i++) {
      init[i * 4 + 0] = Math.random();
      init[i * 4 + 1] = Math.random();
      init[i * 4 + 2] = 0.99 + Math.random() * 0.01;
      init[i * 4 + 3] = Math.random();
    }
    const internalFormat = this.fp16State ? gl.RGBA16F : gl.RGBA32F;
    const texOpts = {
      internalFormat,
      format: gl.RGBA,
      type: gl.FLOAT,
    } as const;
    const t0 = createTexture(gl, n, n, { ...texOpts, pixels: init });
    const t1 = createTexture(gl, n, n, texOpts);
    return { fbos: [createFBO(gl, t0), createFBO(gl, t1)], texs: [t0, t1], i: 0 };
  }

  private ensureTrails(w: number, h: number): void {
    const key = `${w}x${h}`;
    if (this.trails && this.trailsAllocatedFor === key) return;
    const gl = this.gl;
    if (this.trails) {
      for (const t of this.trails.texs) gl.deleteTexture(t);
      for (const f of this.trails.fbos) gl.deleteFramebuffer(f);
    }
    const trailOpts = {
      internalFormat: gl.RGBA8,
      format: gl.RGBA,
      type: gl.UNSIGNED_BYTE,
      // nearest: bilinear would compound into blur on each non-pixel-aligned
      // warp; snapping to whole pixels is fine for the fade effect
      filter: "nearest",
    } as const;
    const t0 = createTexture(gl, w, h, trailOpts);
    const t1 = createTexture(gl, w, h, trailOpts);
    this.trails = {
      fbos: [createFBO(gl, t0), createFBO(gl, t1)],
      texs: [t0, t1],
      i: 0,
    };
    this.trailsAllocatedFor = key;
    this.clearTrails();
  }

  private clearTrails(): void {
    const gl = this.gl;
    for (const f of this.trails.fbos) {
      gl.bindFramebuffer(gl.FRAMEBUFFER, f);
      gl.viewport(0, 0, this.canvasW, this.canvasH);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
    }
  }

  private ensureAtlas(tiles: TileDrawRect[]): void {
    if (tiles.length === 0) return;
    const z = tiles[0].z;
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const t of tiles) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
      if (t.x > maxX) maxX = t.x;
      if (t.y > maxY) maxY = t.y;
    }
    const tilesW = maxX - minX + 1;
    const tilesH = maxY - minY + 1;
    void z;
    if (tilesW === this.atlasTileW && tilesH === this.atlasTileH && this.atlasTex) {
      return;
    }
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    const ts = this.wmt.tileSize;
    this.atlasTex = createTexture(gl, tilesW * ts, tilesH * ts, {
      internalFormat: gl.RG32F,
      format: gl.RG,
      type: gl.FLOAT,
    });
    this.atlasFBO = createFBO(gl, this.atlasTex);
    this.atlasTileW = tilesW;
    this.atlasTileH = tilesH;
  }

  // false when too few tiles are ready to render
  private buildAtlas(tF: number, tC: number, frac: number): boolean {
    if (!this.atlasTex) return false;
    const gl = this.gl;
    const ts = this.wmt.tileSize;
    const aw = this.atlasTileW * ts;
    const ah = this.atlasTileH * ts;

    gl.bindFramebuffer(gl.FRAMEBUFFER, this.atlasFBO);
    gl.viewport(0, 0, aw, ah);
    gl.clearColor(NaN, NaN, 0, 1); // NaN = empty slot, shader skips/respawns

    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.disable(gl.BLEND);

    const tiles = this.view;
    let minX = Infinity, minY = Infinity;
    for (const t of tiles) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
    }

    const { uVar, vVar } = this.state;
    const lerp = frac > 0 && tF !== tC;
    const prog = lerp ? this.progAtlasLerp : this.progAtlas;
    gl.useProgram(prog);
    gl.bindVertexArray(this.quadVAO);

    let drawn = 0;
    for (const tile of tiles) {
      const refUA = this.source.findTex(uVar, tF, tile.z, tile.x, tile.y);
      const refVA = this.source.findTex(vVar, tF, tile.z, tile.x, tile.y);
      if (!refUA || !refVA) continue;
      const refUB = lerp
        ? this.source.findTex(uVar, tC, tile.z, tile.x, tile.y) ?? refUA
        : null;
      const refVB = lerp
        ? this.source.findTex(vVar, tC, tile.z, tile.x, tile.y) ?? refVA
        : null;

      const cx = tile.x - minX;
      const cy = tile.y - minY;
      const x0 = (cx / this.atlasTileW) * 2 - 1;
      const y0 = (cy / this.atlasTileH) * 2 - 1;
      const x1 = ((cx + 1) / this.atlasTileW) * 2 - 1;
      const y1 = ((cy + 1) / this.atlasTileH) * 2 - 1;
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

  // advection per unit wind, in atlas-relative tile coords per frame. Zoom-
  // independent: visual mercator speed still scales as tile-speed / 2^zoom.
  private tileStepFactor(): number {
    return this.opts.speedFactor;
  }

  private updateParticles(): void {
    const gl = this.gl;
    const src = this.particles.i;
    const dst = (src ^ 1) as 0 | 1;
    gl.bindFramebuffer(gl.FRAMEBUFFER, this.particles.fbos[dst]);
    gl.viewport(0, 0, this.texSize, this.texSize);
    gl.disable(gl.BLEND);
    gl.useProgram(this.progParticleUpdate);
    gl.bindVertexArray(this.quadVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.particles.texs[src]);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, this.atlasTex);
    gl.uniform1i(gl.getUniformLocation(this.progParticleUpdate, "u_state"), 0);
    gl.uniform1i(gl.getUniformLocation(this.progParticleUpdate, "u_atlas"), 1);
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_stepFactor"),
      this.tileStepFactor(),
    );
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_ageStep"),
      1 / this.opts.maxAgeFrames,
    );
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_rand"),
      Math.random(),
    );
    gl.uniform2f(
      gl.getUniformLocation(this.progParticleUpdate, "u_atlasTileSize"),
      this.atlasTileW,
      this.atlasTileH,
    );
    const rebase = this.computeAtlasRebase();
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_atlasZoomScale"),
      rebase.scale,
    );
    gl.uniform2f(
      gl.getUniformLocation(this.progParticleUpdate, "u_atlasDelta"),
      rebase.deltaX,
      rebase.deltaY,
    );
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    this.particles.i = dst;
  }

  // rebase from the previous frame's atlas coords to this frame's. Computed
  // in JS (FP64) so the shader only sees small pre-computed numbers.
  private computeAtlasRebase(): { scale: number; deltaX: number; deltaY: number } {
    if (this.view.length === 0) return { scale: 1, deltaX: 0, deltaY: 0 };
    const curZoom = this.view[0].z;
    let minX = Infinity, minY = Infinity;
    for (const t of this.view) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
    }
    if (this.prevAtlasMinTile === null || this.prevAtlasCurZoom === null) {
      this.prevAtlasMinTile = [minX, minY];
      this.prevAtlasCurZoom = curZoom;
      return { scale: 1, deltaX: 0, deltaY: 0 };
    }
    const scale = Math.pow(2, curZoom - this.prevAtlasCurZoom);
    const deltaX = minX - this.prevAtlasMinTile[0] * scale;
    const deltaY = minY - this.prevAtlasMinTile[1] * scale;
    this.prevAtlasMinTile = [minX, minY];
    this.prevAtlasCurZoom = curZoom;
    return { scale, deltaX, deltaY };
  }

  private drawTrailAndParticles(_projData: GlobeProjectionData | null): void {
    const gl = this.gl;
    const src = this.trails.i;
    const dst = (src ^ 1) as 0 | 1;
    gl.bindFramebuffer(gl.FRAMEBUFFER, this.trails.fbos[dst]);
    gl.viewport(0, 0, this.canvasW, this.canvasH);

    gl.disable(gl.BLEND);
    gl.useProgram(this.progTrailFade);
    gl.bindVertexArray(this.quadVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.trails.texs[src]);
    gl.uniform1i(gl.getUniformLocation(this.progTrailFade, "u_prev"), 0);
    gl.uniform1f(
      gl.getUniformLocation(this.progTrailFade, "u_fade"),
      this.firstFrame ? 0 : this.opts.fadeOpacity,
    );
    gl.uniformMatrix3fv(
      gl.getUniformLocation(this.progTrailFade, "u_warp"),
      false,
      this.warp,
    );
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);

    const drawProg = this.progParticleDraw;
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
    gl.useProgram(drawProg);
    gl.bindVertexArray(this.emptyVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.particles.texs[this.particles.i]);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, this.atlasTex);
    gl.uniform1i(gl.getUniformLocation(drawProg, "u_state"), 0);
    gl.uniform1i(gl.getUniformLocation(drawProg, "u_atlas"), 1);
    gl.uniform1i(
      gl.getUniformLocation(drawProg, "u_texSize"),
      this.texSize,
    );
    if (!this.matrixMode) {
      gl.uniform4f(
        gl.getUniformLocation(drawProg, "u_atlasRect"),
        ...this.atlasRectDevPx,
      );
      gl.uniform2f(
        gl.getUniformLocation(drawProg, "u_screen"),
        this.canvasW,
        this.canvasH,
      );
    }
    gl.uniform1f(
      gl.getUniformLocation(drawProg, "u_pointSize"),
      this.opts.particleSize *
        (Math.min(window.devicePixelRatio || 1, 2)),
    );
    gl.uniform2f(
      gl.getUniformLocation(drawProg, "u_speedRange"),
      this.opts.speedRange[0],
      this.opts.speedRange[1],
    );
    gl.uniform2f(
      gl.getUniformLocation(drawProg, "u_atlasTileSize"),
      this.atlasTileW,
      this.atlasTileH,
    );
    gl.drawArrays(gl.POINTS, 0, this.texSize * this.texSize);
    this.trails.i = dst;
  }

  private compositeToHost(projData: GlobeProjectionData | null): void {
    const gl = this.gl;
    const composite = this.currentComposite;
    if (!composite) return;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    if (this.matrixMode) {
      // trail buffer is non-premultiplied -> straight SRC_ALPHA
      beginHostFrame(gl, this.canvasW, this.canvasH, "straight");
    } else {
      gl.viewport(0, 0, this.canvasW, this.canvasH);
      gl.disable(gl.BLEND);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
    }
    if (this.matrixMode && projData && this.compositeMatrixVAO) {
      const prog = composite.program;
      gl.useProgram(prog);
      gl.bindVertexArray(this.compositeMatrixVAO);
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, this.trails.texs[this.trails.i]);
      gl.uniform1i(gl.getUniformLocation(prog, "u_tex"), 0);
      gl.uniform4f(
        gl.getUniformLocation(prog, "u_mercExtent"),
        ...this.atlasRectDevPx,
      );
      setProjectionUniforms(gl, composite.proj, projData);
      gl.drawElements(
        gl.TRIANGLES,
        this.compositeMatrixIndexCount,
        gl.UNSIGNED_SHORT,
        0,
      );
    } else {
      const prog = composite.program;
      gl.useProgram(prog);
      gl.bindVertexArray(this.quadVAO);
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, this.trails.texs[this.trails.i]);
      gl.uniform1i(gl.getUniformLocation(prog, "u_tex"), 0);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    }
  }
}
