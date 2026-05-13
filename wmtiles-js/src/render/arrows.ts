import type { TileCoord, Variable, WMT } from "../reader.js";
import {
  colormapToGLSL,
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
import { TileSource, type TileSourceOptions } from "./source.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
import { computeTimeWindow, type TimeWindow } from "./time.js";
import {
  createFBO,
  createTexture,
  linkProgram,
  VS_ATLAS_SLOT,
} from "./gl.js";
import type { TileDrawRect } from "./heatmap.js";

export type { TileDrawRect } from "./heatmap.js";

export interface ArrowsRendererOptions extends TileSourceOptions {
  // arrowsPerTile² total per visible tile, anchored to tile coords so they
  // stay locked to world position across pan/zoom
  arrowsPerTile?: number;
  arrowSize?: number; // px
  // 0 disables. Outline pass redraws slightly larger in a dark colour so
  // arrows stay legible on bright backgrounds.
  outlineWidth?: number;
  outlineColor?: [number, number, number];
  colormap?: Colormap | BuiltinColormapName;
  speedRange?: [number, number]; // m/s, normalises colormap
  alpha?: number;
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
  onFrame?: (frameMs: number) => void; // cpu time per draw
}

export interface ArrowsRendererState {
  uVar: Variable;
  vVar: Variable;
  t: number;
}

const DEFAULTS = {
  arrowsPerTile: 8,
  arrowSize: 16,
  outlineWidth: 1.5,
  outlineColor: [0, 0, 0] as [number, number, number],
  speedRange: [0, 30] as [number, number],
  alpha: 0.95,
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
const VERTS_PER_ARROW = 9;

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

const VS_ARROW = `#version 300 es
precision highp float;

layout(location=0) in vec2 a_local;
layout(location=1) in vec4 a_inst;  // atlasU, atlasV, sx, sy

uniform sampler2D u_atlas;
uniform vec2  u_screen;       // canvas in device px
uniform float u_arrowSize;    // px
uniform float u_sizeBoost;    // 1 for fill, >1 for outline
uniform vec2  u_speedRange;

flat out int v_alive;
out float v_t;

${MISSING_GLSL_PREAMBLE}

void main() {
  vec2 atlasUV = a_inst.xy;
  vec2 screenCenter = a_inst.zw;

  vec2 wind = texture(u_atlas, atlasUV).rg;
  bool dead = isMissing(wind.x) || isMissing(wind.y)
           || (wind.x == 0.0 && wind.y == 0.0);
  if (dead) {
    v_alive = 0;
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0);
    return;
  }
  v_alive = 1;

  float speed = length(wind);
  v_t = clamp((speed - u_speedRange.x) / max(u_speedRange.y - u_speedRange.x, 1e-30), 0.0, 1.0);

  // u east -> +x, v north -> -y on a y-down canvas
  float angle = atan(-wind.y, wind.x);
  float c = cos(angle), s = sin(angle);

  vec2 local = a_local * u_arrowSize * u_sizeBoost;
  vec2 rotated = vec2(local.x * c - local.y * s, local.x * s + local.y * c);
  vec2 px = screenCenter + rotated;
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  gl_Position = vec4(ndc, 0.0, 1.0);
}`;

const FS_OUTLINE = `#version 300 es
precision highp float;
flat in int v_alive;
in float v_t;
out vec4 outColor;
uniform vec3 u_color;
uniform float u_alpha;
void main() {
  if (v_alive == 0) discard;
  outColor = vec4(u_color, u_alpha);
}`;

function buildFillFS(colormap: Colormap): string {
  return `#version 300 es
precision highp float;
flat in int v_alive;
in float v_t;
out vec4 outColor;

${colormapToGLSL(colormap)}

uniform float u_alpha;
void main() {
  if (v_alive == 0) discard;
  outColor = vec4(colormap(v_t), u_alpha);
}`;
}

export class ArrowsRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;
  private readonly wmt: WMT;

  private readonly opts: Required<
    Pick<
      ArrowsRendererOptions,
      | "arrowsPerTile"
      | "arrowSize"
      | "outlineWidth"
      | "outlineColor"
      | "speedRange"
      | "alpha"
      | "disableTimeLerp"
      | "prefetchNext"
    >
  >;
  private readonly onFrame?: (frameMs: number) => void;

  private readonly progAtlas: WebGLProgram;
  private readonly progAtlasLerp: WebGLProgram;
  private readonly progOutline: WebGLProgram;
  private readonly progFill: WebGLProgram;
  private readonly quadVAO: WebGLVertexArrayObject;
  private readonly arrowVAO: WebGLVertexArrayObject;
  private readonly instanceVBO: WebGLBuffer;
  private instanceCount = 0;

  private atlasTex!: WebGLTexture;
  private atlasFBO!: WebGLFramebuffer;
  private atlasTileW = 0;
  private atlasTileH = 0;
  private canvasW = 1;
  private canvasH = 1;

  private view: TileDrawRect[] = [];
  private raf = 0;
  private disposed = false;

  state: ArrowsRendererState;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    wmt: WMT,
    options: ArrowsRendererOptions = {},
    source?: TileSource,
  ) {
    const gl = canvas.getContext("webgl2", {
      premultipliedAlpha: false,
      antialias: true,
      preserveDrawingBuffer: true,
    }) as WebGL2RenderingContext | null;
    if (!gl) throw new Error("WebGL2 not supported");
    if (!gl.getExtension("EXT_color_buffer_float")) {
      throw new Error("EXT_color_buffer_float not supported");
    }
    this.gl = gl;
    this.wmt = wmt;

    this.opts = {
      arrowsPerTile: Math.max(1, options.arrowsPerTile ?? DEFAULTS.arrowsPerTile),
      arrowSize: options.arrowSize ?? DEFAULTS.arrowSize,
      outlineWidth: options.outlineWidth ?? DEFAULTS.outlineWidth,
      outlineColor: options.outlineColor ?? DEFAULTS.outlineColor,
      speedRange: options.speedRange ?? DEFAULTS.speedRange,
      alpha: options.alpha ?? DEFAULTS.alpha,
      disableTimeLerp:
        options.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
      prefetchNext: options.prefetchNext ?? DEFAULTS.prefetchNext,
    };
    this.onFrame = options.onFrame;

    const colormap = resolveColormap(options.colormap);

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, {
        ...options,
        onUpdate: () => this.scheduleDraw(),
      });
      this.ownsSource = true;
    }

    this.progAtlas = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS);
    this.progAtlasLerp = linkProgram(gl, VS_ATLAS_SLOT, FS_ATLAS_LERP);
    this.progOutline = linkProgram(gl, VS_ARROW, FS_OUTLINE);
    this.progFill = linkProgram(gl, VS_ARROW, buildFillFS(colormap));

    const qvbo = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, qvbo);
    gl.bufferData(
      gl.ARRAY_BUFFER,
      new Float32Array([0, 0, 1, 0, 0, 1, 1, 1]),
      gl.STATIC_DRAW,
    );
    const qvao = gl.createVertexArray();
    if (!qvao) throw new Error("createVertexArray failed");
    gl.bindVertexArray(qvao);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    this.quadVAO = qvao;

    const avbo = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, avbo);
    gl.bufferData(gl.ARRAY_BUFFER, ARROW_GEOM, gl.STATIC_DRAW);
    const ivbo = gl.createBuffer();
    if (!ivbo) throw new Error("createBuffer failed");
    this.instanceVBO = ivbo;
    const avao = gl.createVertexArray();
    if (!avao) throw new Error("createVertexArray failed");
    gl.bindVertexArray(avao);
    gl.bindBuffer(gl.ARRAY_BUFFER, avbo);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, ivbo);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 4, gl.FLOAT, false, 0, 0);
    gl.vertexAttribDivisor(1, 1);

    this.arrowVAO = avao;

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
    this.scheduleDraw();
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    // canonical world only, no wrap copies
    this.view = tiles.filter((t) => t.worldX === t.x);
    this.ensureAtlas(this.view);
    this.scheduleDraw();
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
    this.scheduleDraw();
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    const gl = this.gl;
    if (this.atlasTex) gl.deleteTexture(this.atlasTex);
    if (this.atlasFBO) gl.deleteFramebuffer(this.atlasFBO);
    gl.deleteVertexArray(this.quadVAO);
    gl.deleteVertexArray(this.arrowVAO);
    gl.deleteBuffer(this.instanceVBO);
    gl.deleteProgram(this.progAtlas);
    gl.deleteProgram(this.progAtlasLerp);
    gl.deleteProgram(this.progOutline);
    gl.deleteProgram(this.progFill);
    if (this.ownsSource) this.source.dispose();
  }

  private scheduleDraw(): void {
    if (this.raf || this.disposed) return;
    this.raf = requestAnimationFrame(() => {
      this.raf = 0;
      if (!this.disposed) this.draw();
    });
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

  private buildAtlas(tF: number, tC: number, frac: number): boolean {
    if (!this.atlasTex) return false;
    const gl = this.gl;
    const ts = this.wmt.tileSize;
    const aw = this.atlasTileW * ts;
    const ah = this.atlasTileH * ts;

    gl.bindFramebuffer(gl.FRAMEBUFFER, this.atlasFBO);
    gl.viewport(0, 0, aw, ah);
    gl.clearColor(NaN, NaN, 0, 1);
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

  // Per-instance buffer, anchored at fixed fractional tile coords so arrows pan with the map
  private uploadInstances(): number {
    const N = this.opts.arrowsPerTile;
    const view = this.view;
    if (view.length === 0) return 0;
    let minX = Infinity, minY = Infinity;
    for (const t of view) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
    }
    const total = view.length * N * N;
    const buf = new Float32Array(total * 4);
    let i = 0;
    for (const t of view) {
      const tx = t.x - minX;
      const ty = t.y - minY;
      const dx = t.sx1 - t.sx0;
      const dy = t.sy1 - t.sy0;
      for (let fy = 0; fy < N; fy++) {
        const fv = (fy + 0.5) / N;
        const sy = t.sy0 + fv * dy;
        for (let fx = 0; fx < N; fx++) {
          const fu = (fx + 0.5) / N;
          buf[i++] = (tx + fu) / this.atlasTileW;
          buf[i++] = (ty + fv) / this.atlasTileH;
          buf[i++] = t.sx0 + fu * dx;
          buf[i++] = sy;
        }
      }
    }
    const gl = this.gl;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.instanceVBO);
    gl.bufferData(gl.ARRAY_BUFFER, buf, gl.DYNAMIC_DRAW);
    return total;
  }

  private drawArrows(): void {
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.viewport(0, 0, this.canvasW, this.canvasH);
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);

    this.instanceCount = this.uploadInstances();
    if (this.instanceCount === 0) return;

    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const arrowSizeDev = this.opts.arrowSize * dpr;

    // outline first, fill on top
    if (this.opts.outlineWidth > 0) {
      const boost =
        1.0 + (this.opts.outlineWidth * 2) / Math.max(1, this.opts.arrowSize);
      gl.useProgram(this.progOutline);
      gl.bindVertexArray(this.arrowVAO);
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, this.atlasTex);
      gl.uniform1i(gl.getUniformLocation(this.progOutline, "u_atlas"), 0);
      gl.uniform2f(gl.getUniformLocation(this.progOutline, "u_screen"), this.canvasW, this.canvasH);
      gl.uniform1f(gl.getUniformLocation(this.progOutline, "u_arrowSize"), arrowSizeDev);
      gl.uniform1f(gl.getUniformLocation(this.progOutline, "u_sizeBoost"), boost);
      gl.uniform2f(
        gl.getUniformLocation(this.progOutline, "u_speedRange"),
        this.opts.speedRange[0],
        this.opts.speedRange[1],
      );
      gl.uniform3f(
        gl.getUniformLocation(this.progOutline, "u_color"),
        this.opts.outlineColor[0],
        this.opts.outlineColor[1],
        this.opts.outlineColor[2],
      );
      gl.uniform1f(gl.getUniformLocation(this.progOutline, "u_alpha"), this.opts.alpha);
      gl.drawArraysInstanced(gl.TRIANGLES, 0, VERTS_PER_ARROW, this.instanceCount);
    }

    gl.useProgram(this.progFill);
    gl.bindVertexArray(this.arrowVAO);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, this.atlasTex);
    gl.uniform1i(gl.getUniformLocation(this.progFill, "u_atlas"), 0);
    gl.uniform2f(gl.getUniformLocation(this.progFill, "u_screen"), this.canvasW, this.canvasH);
    gl.uniform1f(gl.getUniformLocation(this.progFill, "u_arrowSize"), arrowSizeDev);
    gl.uniform1f(gl.getUniformLocation(this.progFill, "u_sizeBoost"), 1.0);
    gl.uniform2f(
      gl.getUniformLocation(this.progFill, "u_speedRange"),
      this.opts.speedRange[0],
      this.opts.speedRange[1],
    );
    gl.uniform1f(gl.getUniformLocation(this.progFill, "u_alpha"), this.opts.alpha);
    gl.drawArraysInstanced(gl.TRIANGLES, 0, VERTS_PER_ARROW, this.instanceCount);
  }

  private clearCanvas(): void {
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.viewport(0, 0, this.canvasW, this.canvasH);
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
  }

  private draw(): void {
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
    this.drawArrows();
    this.onFrame?.(performance.now() - tStart);
  }
}
