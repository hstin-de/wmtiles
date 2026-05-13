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

// Particles live in fractional tile coords at u_refZoom (file's max zoom),
// anchoring them to world positions instead of drifting with the atlas UV
// frame as the view changes.
const FS_PARTICLE_UPDATE = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_state;
uniform sampler2D u_atlas;
uniform float u_speed;
uniform float u_ageStep;
uniform float u_rand;
uniform float u_curZoom;
uniform float u_refZoom;
uniform vec2  u_atlasMinTile;  // atlas origin in current zoom tile coords
uniform vec2  u_atlasTileSize; // atlasTileW, atlasTileH at curZoom

${MISSING_GLSL_PREAMBLE}

float hash(vec2 p) {
  return fract(sin(dot(p, vec2(12.9898, 78.233))) * 43758.5453);
}

void main() {
  vec4 s = texture(u_state, v_uv);
  vec2 refPos = s.xy;
  float age = s.z;

  float zf = exp2(u_curZoom - u_refZoom);
  vec2 curPos = refPos * zf;
  vec2 atlasUV = (curPos - u_atlasMinTile) / u_atlasTileSize;
  bool inAtlas = atlasUV.x >= 0.0 && atlasUV.x <= 1.0
              && atlasUV.y >= 0.0 && atlasUV.y <= 1.0;
  vec2 wind = inAtlas ? texture(u_atlas, atlasUV).rg : vec2(0.0);
  bool noData = !inAtlas || isMissing(wind.x) || isMissing(wind.y);

  // Advect in refZoom tile units. Flip v (texture-up vs canvas-down), divide
  // by zf so screen-pixel motion stays zoom-invariant.
  vec2 step = noData ? vec2(0.0) : vec2(wind.x, -wind.y) * u_speed / zf;
  vec2 newRef = refPos + step;
  age += u_ageStep;
  bool dead = age >= 1.0 || noData;
  if (dead) {
    // respawn anywhere in atlas extent, convert back to refZoom
    vec2 randCur = vec2(
      u_atlasMinTile.x + hash(v_uv + vec2(u_rand))         * u_atlasTileSize.x,
      u_atlasMinTile.y + hash(v_uv + vec2(u_rand + 1.0))   * u_atlasTileSize.y
    );
    newRef = randCur / zf;
    age = hash(v_uv + vec2(u_rand + 2.0)) * 0.5;
  }
  outColor = vec4(newRef, age, s.w);
}`;

const VS_PARTICLE_DRAW = `#version 300 es
precision highp float;
uniform sampler2D u_state;
uniform sampler2D u_atlas;
uniform int u_texSize;
uniform vec4 u_atlasRect;
uniform vec2 u_screen;
uniform float u_pointSize;
uniform vec2 u_speedRange;
uniform float u_curZoom;
uniform float u_refZoom;
uniform vec2  u_atlasMinTile;
uniform vec2  u_atlasTileSize;
out float v_t;
out float v_age;
flat out int v_alive;

${MISSING_GLSL_PREAMBLE}

void main() {
  int i = gl_VertexID;
  int x = i % u_texSize;
  int y = i / u_texSize;
  vec4 s = texelFetch(u_state, ivec2(x, y), 0);
  vec2 refPos = s.xy;

  float zf = exp2(u_curZoom - u_refZoom);
  vec2 curPos = refPos * zf;
  vec2 atlasUV = (curPos - u_atlasMinTile) / u_atlasTileSize;
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

function buildParticleFS(colormap: Colormap): string {
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
  outColor = vec4(col, fade);
}`;
}

// Trail buffer is canvas-pixel space but the canvas stays pinned at (0,0).
// Sampling the previous trail at uv minus pan-delta keeps trails world-locked.
const FS_TRAIL_FADE = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_prev;
uniform float u_fade;
uniform vec2  u_panDelta;   // canvas-pixel space (Y down)
void main() {
  vec2 size = vec2(textureSize(u_prev, 0));
  // v_uv Y-up vs panDelta Y-down
  vec2 delta = vec2(u_panDelta.x, -u_panDelta.y);
  vec2 shifted = v_uv - delta / size;
  // out-of-buffer UV: treat as empty, otherwise CLAMP_TO_EDGE smears edges
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

export class ParticlesRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;

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

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly progParticleUpdate: WebGLProgram;
  private readonly progParticleDraw: WebGLProgram;
  private readonly progTrailFade: WebGLProgram;
  private readonly progComposite: WebGLProgram;

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
  private prevLayerOrigin: [number, number] | null = null;
  private panDelta: [number, number] = [0, 0];

  state: ParticlesRendererState;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    wmt: WMT,
    options: ParticlesRendererOptions = {},
    source?: TileSource,
  ) {
    const gl = canvas.getContext("webgl2", {
      premultipliedAlpha: false,
      antialias: false,
      preserveDrawingBuffer: true,
    }) as WebGL2RenderingContext | null;
    if (!gl) throw new Error("WebGL2 not supported");
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    // OES_texture_float_linear: not fatal, falls back to nearest on some GPUs
    gl.getExtension("OES_texture_float_linear");
    this.gl = gl;
    this.wmt = wmt;

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
      VS_PARTICLE_DRAW,
      buildParticleFS(this.colormap),
    );
    this.progTrailFade = linkProgram(gl, VS_FULLSCREEN, FS_TRAIL_FADE);
    this.progComposite = linkProgram(gl, VS_FULLSCREEN, FS_COMPOSITE);

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
    // Trails survive pans (Leaflet re-translates the canvas) but not zoom
    // changes since the canvas-px to world mapping scales.
    const newZoom = tiles[0]?.z ?? -1;
    if (newZoom !== this.prevViewZoom && this.prevViewZoom !== -1) {
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

  resize(widthDevPx: number, heightDevPx: number): void {
    if (this.disposed) return;
    const w = Math.max(1, widthDevPx | 0);
    const h = Math.max(1, heightDevPx | 0);
    if (this.canvas.width !== w || this.canvas.height !== h) {
      this.canvas.width = w;
      this.canvas.height = h;
    }
    this.canvasW = w;
    this.canvasH = h;
    this.gl.viewport(0, 0, w, h);
    this.ensureTrails(w, h);
  }

  start(): void {
    if (this.disposed || this.running) return;
    this.running = true;
    this.firstFrame = true;
    const loop = (): void => {
      if (!this.running || this.disposed) return;
      this.tick();
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
    gl.deleteProgram(this.progAtlas);
    gl.deleteProgram(this.progAtlasLerp);
    gl.deleteProgram(this.progParticleUpdate);
    gl.deleteProgram(this.progParticleDraw);
    gl.deleteProgram(this.progTrailFade);
    gl.deleteProgram(this.progComposite);
    if (this.ownsSource) this.source.dispose();
  }

  // --- frame ---

  private tick(): void {
    if (this.canvasW <= 1 || this.canvasH <= 1) return;
    if (this.view.length === 0) return;
    const tStart = performance.now();

    // Delta of layer-origin in canvas px since last tick, used by the trail
    // fade to keep trails world-locked.
    const ref = this.view[0];
    const tilePxW = ref.sx1 - ref.sx0;
    const tilePxH = ref.sy1 - ref.sy0;
    const layerOrigin: [number, number] = [
      ref.sx0 - ref.x * tilePxW,
      ref.sy0 - ref.y * tilePxH,
    ];
    if (this.prevLayerOrigin && !this.firstFrame) {
      this.panDelta = [
        layerOrigin[0] - this.prevLayerOrigin[0],
        layerOrigin[1] - this.prevLayerOrigin[1],
      ];
    } else {
      this.panDelta = [0, 0];
    }
    this.prevLayerOrigin = layerOrigin;

    const { tF, tC, frac, tP } = this.timeWindow();
    this.requestMissingTiles(tF, tC, tP);
    if (!this.buildAtlas(tF, tC, frac)) return;

    this.updateParticles();
    this.drawTrailAndParticles();
    this.compositeToCanvas();
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
    // spread across the whole refZoom grid; out-of-atlas particles respawn on first update
    const range = 1 << this.wmt.zoomRange.max;
    for (let i = 0; i < n * n; i++) {
      init[i * 4 + 0] = Math.random() * range;
      init[i * 4 + 1] = Math.random() * range;
      init[i * 4 + 2] = Math.random();
      init[i * 4 + 3] = Math.random();
    }
    const texOpts = {
      internalFormat: gl.RGBA32F,
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
      filter: "linear", // sub-pixel sampling in the fade pass

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

  // Atlas origin in current-zoom tile coords, used by the shaders to map
  // refZoom particle state into atlas UV and screen space.
  private viewAtlasMeta(): {
    curZoom: number;
    minTileX: number;
    minTileY: number;
    sizeW: number;
    sizeH: number;
  } | null {
    if (this.view.length === 0) return null;
    let minX = Infinity, minY = Infinity;
    for (const t of this.view) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
    }
    return {
      curZoom: this.view[0].z,
      minTileX: minX,
      minTileY: minY,
      sizeW: this.atlasTileW,
      sizeH: this.atlasTileH,
    };
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
      gl.getUniformLocation(this.progParticleUpdate, "u_speed"),
      this.opts.speedFactor,
    );
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_ageStep"),
      1 / this.opts.maxAgeFrames,
    );
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleUpdate, "u_rand"),
      Math.random(),
    );
    const meta = this.viewAtlasMeta();
    if (meta) {
      gl.uniform1f(
        gl.getUniformLocation(this.progParticleUpdate, "u_curZoom"),
        meta.curZoom,
      );
      gl.uniform1f(
        gl.getUniformLocation(this.progParticleUpdate, "u_refZoom"),
        this.wmt.zoomRange.max,
      );
      gl.uniform2f(
        gl.getUniformLocation(this.progParticleUpdate, "u_atlasMinTile"),
        meta.minTileX,
        meta.minTileY,
      );
      gl.uniform2f(
        gl.getUniformLocation(this.progParticleUpdate, "u_atlasTileSize"),
        meta.sizeW,
        meta.sizeH,
      );
    }
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    this.particles.i = dst;
  }

  private drawTrailAndParticles(): void {
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
    gl.uniform2f(
      gl.getUniformLocation(this.progTrailFade, "u_panDelta"),
      this.panDelta[0],
      this.panDelta[1],
    );
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);

    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
    gl.useProgram(this.progParticleDraw);
    gl.bindVertexArray(this.emptyVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.particles.texs[this.particles.i]);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, this.atlasTex);
    gl.uniform1i(gl.getUniformLocation(this.progParticleDraw, "u_state"), 0);
    gl.uniform1i(gl.getUniformLocation(this.progParticleDraw, "u_atlas"), 1);
    gl.uniform1i(
      gl.getUniformLocation(this.progParticleDraw, "u_texSize"),
      this.texSize,
    );
    gl.uniform4f(
      gl.getUniformLocation(this.progParticleDraw, "u_atlasRect"),
      ...this.atlasRectDevPx,
    );
    gl.uniform2f(
      gl.getUniformLocation(this.progParticleDraw, "u_screen"),
      this.canvasW,
      this.canvasH,
    );
    gl.uniform1f(
      gl.getUniformLocation(this.progParticleDraw, "u_pointSize"),
      this.opts.particleSize *
        (Math.min(window.devicePixelRatio || 1, 2)),
    );
    gl.uniform2f(
      gl.getUniformLocation(this.progParticleDraw, "u_speedRange"),
      this.opts.speedRange[0],
      this.opts.speedRange[1],
    );
    const meta = this.viewAtlasMeta();
    if (meta) {
      gl.uniform1f(
        gl.getUniformLocation(this.progParticleDraw, "u_curZoom"),
        meta.curZoom,
      );
      gl.uniform1f(
        gl.getUniformLocation(this.progParticleDraw, "u_refZoom"),
        this.wmt.zoomRange.max,
      );
      gl.uniform2f(
        gl.getUniformLocation(this.progParticleDraw, "u_atlasMinTile"),
        meta.minTileX,
        meta.minTileY,
      );
      gl.uniform2f(
        gl.getUniformLocation(this.progParticleDraw, "u_atlasTileSize"),
        meta.sizeW,
        meta.sizeH,
      );
    }
    gl.drawArrays(gl.POINTS, 0, this.texSize * this.texSize);
    this.trails.i = dst;
  }

  private compositeToCanvas(): void {
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.viewport(0, 0, this.canvasW, this.canvasH);
    gl.disable(gl.BLEND);
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
    gl.useProgram(this.progComposite);
    gl.bindVertexArray(this.quadVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.trails.texs[this.trails.i]);
    gl.uniform1i(gl.getUniformLocation(this.progComposite, "u_tex"), 0);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
  }
}
