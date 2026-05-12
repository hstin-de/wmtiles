import type { Variable, WMT, TileCoord } from "wmtiles";

declare const L: typeof import("leaflet");

export interface LayerState {
  variable: Variable;
  t: number;
  vmin: number;
  vmax: number;
  gen: number;
}

export interface WMTLayer extends L.Layer {
  state: LayerState;
  setState(patch: Partial<LayerState>): void;
}

const VS = `#version 300 es
precision highp float;
layout(location=0) in vec2 a_pos;
uniform vec2 u_screen;
uniform vec4 u_rect;
out vec2 v_uv;
void main() {
  vec2 px = mix(u_rect.xy, u_rect.zw, a_pos);
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  v_uv = a_pos;
  gl_Position = vec4(ndc, 0.0, 1.0);
}`;

const FS = `#version 300 es
precision highp float;
in vec2 v_uv;
out vec4 outColor;
uniform sampler2D u_texA;
uniform sampler2D u_texB;
uniform vec2 u_range;
uniform float u_alpha;
uniform vec2 u_uvOffA;
uniform vec2 u_uvScaleA;
uniform vec2 u_uvOffB;
uniform vec2 u_uvScaleB;
uniform float u_lerp;

const vec3 STOPS[9] = vec3[9](
  vec3( 68.0,   1.0,  84.0),
  vec3( 72.0,  35.0, 116.0),
  vec3( 64.0,  67.0, 135.0),
  vec3( 52.0,  94.0, 141.0),
  vec3( 41.0, 121.0, 142.0),
  vec3( 32.0, 144.0, 140.0),
  vec3( 34.0, 167.0, 132.0),
  vec3( 68.0, 190.0, 112.0),
  vec3(253.0, 231.0,  37.0)
);

vec3 viridis(float t) {
  t = clamp(t, 0.0, 1.0);
  float f = t * 8.0;
  int i = int(min(7.0, floor(f)));
  float frac = f - float(i);
  return mix(STOPS[i], STOPS[i + 1], frac) / 255.0;
}

void main() {
  float vA = texture(u_texA, u_uvOffA + v_uv * u_uvScaleA).r;
  float vB = texture(u_texB, u_uvOffB + v_uv * u_uvScaleB).r;
  if (vA != vA || vB != vB) discard;
  float v = mix(vA, vB, u_lerp);
  float t = (v - u_range.x) / max(u_range.y - u_range.x, 1e-30);
  outColor = vec4(viridis(t), u_alpha);
}`;

interface CacheEntry {
  tex: WebGLTexture;
  used: number;
}

const TEX_CACHE_MAX = 384;
const ALPHA = 0.85;

function compileShader(gl: WebGL2RenderingContext, type: number, src: string): WebGLShader {
  const sh = gl.createShader(type);
  if (!sh) throw new Error("createShader failed");
  gl.shaderSource(sh, src);
  gl.compileShader(sh);
  if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
    const log = gl.getShaderInfoLog(sh) ?? "";
    gl.deleteShader(sh);
    throw new Error("shader compile: " + log);
  }
  return sh;
}

function linkProgram(gl: WebGL2RenderingContext, vsSrc: string, fsSrc: string): WebGLProgram {
  const vs = compileShader(gl, gl.VERTEX_SHADER, vsSrc);
  const fs = compileShader(gl, gl.FRAGMENT_SHADER, fsSrc);
  const p = gl.createProgram();
  if (!p) throw new Error("createProgram failed");
  gl.attachShader(p, vs);
  gl.attachShader(p, fs);
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) {
    const log = gl.getProgramInfoLog(p) ?? "";
    throw new Error("link: " + log);
  }
  return p;
}

export function makeWMTLayer(wmt: WMT): WMTLayer {
  const tileSize = wmt.tileSize;

  const Layer = L.Layer.extend({
    state: {
      variable: wmt.variables[0],
      t: 0,
      vmin: 0,
      vmax: 1,
      gen: 0,
    } as LayerState,

    onAdd(this: WMTLayer & any, map: L.Map) {
      this._map = map;

      const canvas = L.DomUtil.create(
        "canvas",
        "wmt-webgl-layer leaflet-zoom-animated",
      );
      canvas.style.position = "absolute";
      canvas.style.pointerEvents = "none";
      map.getPanes().overlayPane.appendChild(canvas);
      this._canvas = canvas;

      const gl = canvas.getContext("webgl2", {
        premultipliedAlpha: false,
        antialias: false,
        preserveDrawingBuffer: true,
      }) as WebGL2RenderingContext | null;
      if (!gl) throw new Error("WebGL2 not supported");
      this._gl = gl;

      const prog = linkProgram(gl, VS, FS);
      this._prog = prog;
      this._uScreen = gl.getUniformLocation(prog, "u_screen");
      this._uRect = gl.getUniformLocation(prog, "u_rect");
      this._uRange = gl.getUniformLocation(prog, "u_range");
      this._uAlpha = gl.getUniformLocation(prog, "u_alpha");
      this._uTexA = gl.getUniformLocation(prog, "u_texA");
      this._uTexB = gl.getUniformLocation(prog, "u_texB");
      this._uUvOffA = gl.getUniformLocation(prog, "u_uvOffA");
      this._uUvScaleA = gl.getUniformLocation(prog, "u_uvScaleA");
      this._uUvOffB = gl.getUniformLocation(prog, "u_uvOffB");
      this._uUvScaleB = gl.getUniformLocation(prog, "u_uvScaleB");
      this._uLerp = gl.getUniformLocation(prog, "u_lerp");

      const vbo = gl.createBuffer();
      gl.bindBuffer(gl.ARRAY_BUFFER, vbo);
      gl.bufferData(
        gl.ARRAY_BUFFER,
        new Float32Array([0, 0, 1, 0, 0, 1, 1, 1]),
        gl.STATIC_DRAW,
      );
      const vao = gl.createVertexArray();
      gl.bindVertexArray(vao);
      gl.enableVertexAttribArray(0);
      gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
      this._vao = vao;

      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);

      this._cache = new Map<string, CacheEntry>();
      this._pending = new Map<string, Promise<void>>();
      this._frame = 0;
      this._raf = 0;

      const onMove = () => {
        this._reset();
        this._scheduleDraw();
      };
      const onResize = () => {
        this._reset();
        this._scheduleDraw();
      };
      const onZoomAnim = (e: L.ZoomAnimEvent) => {
        const m = this._map as L.Map;
        const scale = m.getZoomScale(e.zoom, m.getZoom());
        const offset = (m as any)
          ._latLngBoundsToNewLayerBounds(m.getBounds(), e.zoom, e.center)
          .min;
        L.DomUtil.setTransform(this._canvas, offset, scale);
      };
      this._handlers = { onMove, onResize, onZoomAnim };
      map.on("move", onMove);
      map.on("zoomend viewreset", onMove);
      map.on("resize", onResize);
      map.on("zoomanim", onZoomAnim);

      this._reset();
      this._scheduleDraw();
      return this;
    },

    onRemove(this: WMTLayer & any, map: L.Map) {
      const h = this._handlers;
      map.off("move", h.onMove);
      map.off("zoomend viewreset", h.onMove);
      map.off("resize", h.onResize);
      map.off("zoomanim", h.onZoomAnim);
      if (this._raf) cancelAnimationFrame(this._raf);

      const gl = this._gl as WebGL2RenderingContext;
      for (const e of this._cache.values()) gl.deleteTexture(e.tex);
      this._cache.clear();
      this._pending.clear();

      this._canvas.remove();
    },

    setState(this: WMTLayer & any, patch: Partial<LayerState>) {
      Object.assign(this.state, patch);
      this.state.gen++;
      this._scheduleDraw();
    },

    _reset(this: WMTLayer & any) {
      const map = this._map as L.Map;
      const size = map.getSize();
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      const canvas = this._canvas as HTMLCanvasElement;

      const wpx = size.x + "px";
      const hpx = size.y + "px";
      if (canvas.style.width !== wpx) canvas.style.width = wpx;
      if (canvas.style.height !== hpx) canvas.style.height = hpx;

      const bw = Math.max(1, (size.x * dpr) | 0);
      const bh = Math.max(1, (size.y * dpr) | 0);
      if (canvas.width !== bw || canvas.height !== bh) {
        canvas.width = bw;
        canvas.height = bh;
      }
      this._dpr = dpr;
      (this._gl as WebGL2RenderingContext).viewport(0, 0, bw, bh);

      const topLeft = map.containerPointToLayerPoint([0, 0]);
      L.DomUtil.setPosition(canvas, topLeft);
    },

    _scheduleDraw(this: WMTLayer & any) {
      if (this._raf) return;
      this._raf = requestAnimationFrame(() => {
        this._raf = 0;
        this._draw();
      });
    },

    _tileKey(varId: number, t: number, z: number, x: number, y: number): string {
      return `${varId}|${t}|${z}|${x}|${y}`;
    },

    _evictIfNeeded(this: WMTLayer & any) {
      if (this._cache.size <= TEX_CACHE_MAX) return;
      const entries: Array<[string, CacheEntry]> = [...this._cache.entries()];
      entries.sort((a, b) => a[1].used - b[1].used);
      const drop = this._cache.size - TEX_CACHE_MAX;
      const gl = this._gl as WebGL2RenderingContext;
      for (let i = 0; i < drop; i++) {
        gl.deleteTexture(entries[i][1].tex);
        this._cache.delete(entries[i][0]);
      }
    },

    _uploadTile(this: WMTLayer & any, key: string, pixels: Float32Array) {
      const gl = this._gl as WebGL2RenderingContext;
      const tex = gl.createTexture();
      if (!tex) return;
      gl.bindTexture(gl.TEXTURE_2D, tex);
      gl.texImage2D(
        gl.TEXTURE_2D,
        0,
        gl.R32F,
        tileSize,
        tileSize,
        0,
        gl.RED,
        gl.FLOAT,
        pixels,
      );
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
      this._cache.set(key, { tex, used: ++this._frame });
      this._evictIfNeeded();
    },

    _fetchMissing(
      this: WMTLayer & any,
      varId: number,
      t: number,
      z: number,
      missing: TileCoord[],
    ) {
      if (missing.length === 0) return;
      const gen = this.state.gen;
      const variable = this.state.variable as Variable;
      const fetchKey = `${varId}|${t}|${z}|batch`;
      if (this._pending.has(fetchKey)) return;

      const p = (async () => {
        try {
          const arrays = await variable.tiles({ time: t, coords: missing });
          if (gen !== this.state.gen) return;
          for (let i = 0; i < missing.length; i++) {
            const c = missing[i];
            const k = this._tileKey(varId, t, c.z, c.x, c.y);
            if (this._cache.has(k)) continue;
            this._uploadTile(k, arrays[i]);
          }
          this._scheduleDraw();
        } catch (err) {
          console.error("tile batch failed", err);
        } finally {
          this._pending.delete(fetchKey);
        }
      })();
      this._pending.set(fetchKey, p);
    },

    _findTex(
      this: WMTLayer & any,
      varId: number,
      t: number,
      z: number,
      x: number,
      y: number,
    ): { tex: WebGLTexture; ox: number; oy: number; s: number } | null {
      const k = this._tileKey(varId, t, z, x, y);
      const e = this._cache.get(k) as CacheEntry | undefined;
      if (e) {
        e.used = ++this._frame;
        return { tex: e.tex, ox: 0, oy: 0, s: 1 };
      }
      for (let dz = 1; dz <= 6 && z - dz >= wmt.zoomRange.min; dz++) {
        const za = z - dz;
        const xa = x >> dz;
        const ya = y >> dz;
        const ak = this._tileKey(varId, t, za, xa, ya);
        const ae = this._cache.get(ak) as CacheEntry | undefined;
        if (ae) {
          const inv = 1 / (1 << dz);
          ae.used = ++this._frame;
          return {
            tex: ae.tex,
            ox: (x - (xa << dz)) * inv,
            oy: (y - (ya << dz)) * inv,
            s: inv,
          };
        }
      }
      return null;
    },

    _drawPair(
      this: WMTLayer & any,
      A: { tex: WebGLTexture; ox: number; oy: number; s: number },
      B: { tex: WebGLTexture; ox: number; oy: number; s: number },
      lerp: number,
      sx0: number,
      sy0: number,
      sx1: number,
      sy1: number,
    ) {
      const gl = this._gl as WebGL2RenderingContext;
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, A.tex);
      gl.activeTexture(gl.TEXTURE1);
      gl.bindTexture(gl.TEXTURE_2D, B.tex);
      gl.uniform4f(this._uRect, sx0, sy0, sx1, sy1);
      gl.uniform2f(this._uUvOffA, A.ox, A.oy);
      gl.uniform2f(this._uUvScaleA, A.s, A.s);
      gl.uniform2f(this._uUvOffB, B.ox, B.oy);
      gl.uniform2f(this._uUvScaleB, B.s, B.s);
      gl.uniform1f(this._uLerp, lerp);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    },

    _attempt(
      this: WMTLayer & any,
      varId: number,
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
    ): boolean {
      const A = this._findTex(varId, tF, z, x, y);
      const B = frac > 0 ? this._findTex(varId, tC, z, x, y) : A;
      if (A && B) {
        this._drawPair(A, B, frac, sx0, sy0, sx1, sy1);
        return true;
      }
      if (A) {
        this._drawPair(A, A, 0, sx0, sy0, sx1, sy1);
        return true;
      }
      if (B) {
        this._drawPair(B, B, 0, sx0, sy0, sx1, sy1);
        return true;
      }
      return false;
    },

    _draw(this: WMTLayer & any) {
      const gl = this._gl as WebGL2RenderingContext;
      const map = this._map as L.Map;
      const { variable, t, vmin, vmax } = this.state as LayerState;

      if (gl.canvas.width <= 1 || gl.canvas.height <= 1) return;

      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);

      const maxStep = wmt.timeStepCount - 1;
      const tF = Math.max(0, Math.min(maxStep, Math.floor(t)));
      const tCRaw = tF + 1;
      const tC = Math.min(maxStep, tCRaw);
      const frac = tCRaw > maxStep ? 0 : t - tF;
      const tPRaw = (frac > 0 ? tC : tF) + 1;
      const tP = tPRaw <= maxStep && tPRaw !== tF && tPRaw !== tC ? tPRaw : -1;

      const z = Math.min(
        Math.max(map.getZoom() | 0, wmt.zoomRange.min),
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

      const dpr = this._dpr as number;
      gl.useProgram(this._prog);
      gl.bindVertexArray(this._vao);
      gl.uniform2f(this._uScreen, gl.canvas.width, gl.canvas.height);
      gl.uniform2f(this._uRange, vmin, vmax);
      gl.uniform1f(this._uAlpha, ALPHA);
      gl.uniform1i(this._uTexA, 0);
      gl.uniform1i(this._uTexB, 1);

      const missingF: TileCoord[] = [];
      const missingC: TileCoord[] = [];
      const missingP: TileCoord[] = [];
      const seenF = new Set<string>();
      const seenC = new Set<string>();
      const seenP = new Set<string>();

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
          const sx0 = p0.x * dpr;
          const sy0 = p0.y * dpr;
          const sx1 = p1.x * dpr;
          const sy1 = p1.y * dpr;

          const drew = this._attempt(
            variable.id, tF, tC, frac, z, wrapped, yi,
            sx0, sy0, sx1, sy1,
          );

          if (!drew && z + 1 <= wmt.zoomRange.max) {
            const cz = z + 1;
            const w = sx1 - sx0;
            const h = sy1 - sy0;
            for (let cy = 0; cy < 2; cy++) {
              for (let cx = 0; cx < 2; cx++) {
                this._attempt(
                  variable.id, tF, tC, frac, cz,
                  wrapped * 2 + cx, yi * 2 + cy,
                  sx0 + (cx * w) / 2,
                  sy0 + (cy * h) / 2,
                  sx0 + ((cx + 1) * w) / 2,
                  sy0 + ((cy + 1) * h) / 2,
                );
              }
            }
          }

          const kF = this._tileKey(variable.id, tF, z, wrapped, yi);
          if (!this._cache.has(kF) && !seenF.has(kF)) {
            seenF.add(kF);
            missingF.push({ z, x: wrapped, y: yi });
          }
          if (frac > 0) {
            const kC = this._tileKey(variable.id, tC, z, wrapped, yi);
            if (!this._cache.has(kC) && !seenC.has(kC)) {
              seenC.add(kC);
              missingC.push({ z, x: wrapped, y: yi });
            }
          }
          if (tP >= 0) {
            const kP = this._tileKey(variable.id, tP, z, wrapped, yi);
            if (!this._cache.has(kP) && !seenP.has(kP)) {
              seenP.add(kP);
              missingP.push({ z, x: wrapped, y: yi });
            }
          }
        }
      }

      if (missingF.length > 0) {
        this._fetchMissing(variable.id, tF, z, missingF);
      }
      if (missingC.length > 0) {
        this._fetchMissing(variable.id, tC, z, missingC);
      }
      if (tP >= 0 && missingP.length > 0) {
        this._fetchMissing(variable.id, tP, z, missingP);
      }
    },
  });

  return new Layer() as WMTLayer;
}
