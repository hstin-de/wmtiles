import type { TileCoord, Variable, WMT } from "../reader.js";
import { TileTextureCache } from "./cache.js";
import { MISSING_SENTINEL } from "./missing.js";

export interface TileSourceOptions {
  cacheSize?: number;
  parentFallbackLevels?: number;
  onUpdate?: () => void;
  // "auto" probes R32F and falls back to R16F on drivers that silently zero
  // R32F uploads (iOS Safari, some Mali/Adreno).
  tileTextureFormat?: "r32f" | "r16f" | "auto";
  // Subtracts variable.range.min from every uploaded sample. Renderer must
  // mirror the shift on any range uniform via getBaseline(). Needed for
  // layers doing (v - vmin)/range where mobile highp gets demoted to FP16
  // and large absolute values (e.g. surface pressure ~1e5 Pa) collapse the
  // colormap to one colour. Off by default: breaks layers that read
  // absolute quantities (wind, isobar contours).
  shiftValuesByBaseline?: boolean;
}

export interface TexRef {
  tex: WebGLTexture;
  ox: number;
  oy: number;
  s: number;
}

const DEFAULTS = {
  cacheSize: 384,
  parentFallbackLevels: 6,
} as const;

// Round-trip a known value through upload + sample + readback. Some older
// Mali/Adreno accept 1x1 float uploads but zero out tile-sized ones, so we
// probe at the real tile size.
const PROBE_VAL = 0.5;
function probeFloatTexture(
  gl: WebGL2RenderingContext,
  internalFormat: number,
  size: number,
): boolean {
  const tex = gl.createTexture();
  const fbo = gl.createFramebuffer();
  const colorTex = gl.createTexture();
  const vbo = gl.createBuffer();
  const vao = gl.createVertexArray();
  const vs = gl.createShader(gl.VERTEX_SHADER);
  const fs = gl.createShader(gl.FRAGMENT_SHADER);
  const prog = gl.createProgram();
  if (!tex || !fbo || !colorTex || !vbo || !vao || !vs || !fs || !prog) {
    return false;
  }
  let ok = false;
  try {
    const data = new Float32Array(size * size).fill(PROBE_VAL);
    gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.texImage2D(
      gl.TEXTURE_2D, 0, internalFormat, size, size, 0, gl.RED, gl.FLOAT, data,
    );
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
    if (gl.getError() !== gl.NO_ERROR) return false;

    gl.bindTexture(gl.TEXTURE_2D, colorTex);
    gl.texImage2D(
      gl.TEXTURE_2D, 0, gl.RGBA8, 1, 1, 0, gl.RGBA, gl.UNSIGNED_BYTE, null,
    );
    gl.bindFramebuffer(gl.FRAMEBUFFER, fbo);
    gl.framebufferTexture2D(
      gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, colorTex, 0,
    );
    if (gl.checkFramebufferStatus(gl.FRAMEBUFFER) !== gl.FRAMEBUFFER_COMPLETE) {
      return false;
    }

    gl.shaderSource(vs, `#version 300 es
layout(location=0) in vec2 a_pos;
out vec2 v_uv;
void main() { v_uv = a_pos; gl_Position = vec4(a_pos * 2.0 - 1.0, 0.0, 1.0); }`);
    gl.compileShader(vs);
    gl.shaderSource(fs, `#version 300 es
precision highp float;
in vec2 v_uv;
uniform sampler2D u_tex;
out vec4 outColor;
void main() { outColor = vec4(texture(u_tex, v_uv).r, 0.0, 0.0, 1.0); }`);
    gl.compileShader(fs);
    if (
      !gl.getShaderParameter(vs, gl.COMPILE_STATUS) ||
      !gl.getShaderParameter(fs, gl.COMPILE_STATUS)
    ) {
      return false;
    }
    gl.attachShader(prog, vs);
    gl.attachShader(prog, fs);
    gl.linkProgram(prog);
    if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) return false;

    gl.bindBuffer(gl.ARRAY_BUFFER, vbo);
    gl.bufferData(
      gl.ARRAY_BUFFER,
      new Float32Array([0, 0, 1, 0, 0, 1, 1, 1]),
      gl.STATIC_DRAW,
    );
    gl.bindVertexArray(vao);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);

    gl.viewport(0, 0, 1, 1);
    gl.useProgram(prog);
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.uniform1i(gl.getUniformLocation(prog, "u_tex"), 0);
    gl.disable(gl.BLEND);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    if (gl.getError() !== gl.NO_ERROR) return false;

    const out = new Uint8Array(4);
    gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, out);
    ok = out[0] >= 100 && out[0] <= 156; // 0.5 * 255 ≈ 128, plus rounding slack
    return ok;
  } finally {
    gl.bindBuffer(gl.ARRAY_BUFFER, null);
    gl.bindVertexArray(null);
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.bindTexture(gl.TEXTURE_2D, null);
    gl.deleteProgram(prog);
    gl.deleteShader(vs);
    gl.deleteShader(fs);
    gl.deleteVertexArray(vao);
    gl.deleteBuffer(vbo);
    gl.deleteFramebuffer(fbo);
    gl.deleteTexture(colorTex);
    gl.deleteTexture(tex);
  }
}

export interface TileSourceFormatDiagnostics {
  chosen: "r32f" | "r16f";
  r32fProbe: boolean | null; // null when not probed
  r16fProbe: boolean | null;
  renderer: string; // vendor / renderer via WEBGL_debug_renderer_info if available
}

export interface TileDataDiagnostics {
  tilesUploaded: number;
  minSample: number; // NaN samples ignored
  maxSample: number;
  nanCount: number;
  lastTileHead: number[]; // first few samples of most recent upload
}

let lastDiag: TileSourceFormatDiagnostics | null = null;
const dataDiag: TileDataDiagnostics = {
  tilesUploaded: 0,
  minSample: Infinity,
  maxSample: -Infinity,
  nanCount: 0,
  lastTileHead: [],
};
export function getTileFormatDiagnostics(): TileSourceFormatDiagnostics | null {
  return lastDiag;
}
export function getTileDataDiagnostics(): TileDataDiagnostics {
  return dataDiag;
}

function rendererString(gl: WebGL2RenderingContext): string {
  const ext = gl.getExtension("WEBGL_debug_renderer_info");
  if (!ext) return gl.getParameter(gl.VERSION) ?? "unknown";
  const vendor = gl.getParameter(ext.UNMASKED_VENDOR_WEBGL) ?? "?";
  const renderer = gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) ?? "?";
  return `${vendor} / ${renderer}`;
}

// Per-WMT tile-data manager. Shared across renderer strategies so the LRU
// covers all variables. Fetches are deduped per (variable, time) batch and
// results after invalidate() are dropped.
export class TileSource {
  private readonly gl: WebGL2RenderingContext;
  private readonly wmt: WMT;
  private readonly tileSize: number;
  private readonly cache: TileTextureCache;
  private readonly pending = new Map<string, Promise<void>>();
  private readonly parentLevels: number;
  private readonly onUpdate?: () => void;
  private readonly internalFormat: number;
  private readonly shiftByBaseline: boolean;
  private readonly baselines = new Map<number, number>();
  private gen = 0;
  private disposed = false;

  constructor(gl: WebGL2RenderingContext, wmt: WMT, options?: TileSourceOptions) {
    this.gl = gl;
    this.wmt = wmt;
    this.tileSize = wmt.tileSize;
    this.cache = new TileTextureCache(
      gl,
      options?.cacheSize ?? DEFAULTS.cacheSize,
    );
    this.parentLevels =
      options?.parentFallbackLevels ?? DEFAULTS.parentFallbackLevels;
    this.onUpdate = options?.onUpdate;
    this.shiftByBaseline = options?.shiftValuesByBaseline ?? false;

    const requested = options?.tileTextureFormat ?? "auto";
    const renderer = rendererString(gl);
    if (requested === "r32f") {
      this.internalFormat = gl.R32F;
      lastDiag = { chosen: "r32f", r32fProbe: null, r16fProbe: null, renderer };
    } else if (requested === "r16f") {
      this.internalFormat = gl.R16F;
      lastDiag = { chosen: "r16f", r32fProbe: null, r16fProbe: null, renderer };
    } else {
      const probeSize = Math.min(this.tileSize, 256);
      const r32 = probeFloatTexture(gl, gl.R32F, probeSize);
      let r16: boolean | null = null;
      if (r32) {
        this.internalFormat = gl.R32F;
      } else {
        // last resort: still pick R16F even if probe fails, diagnostic surfaces it
        r16 = probeFloatTexture(gl, gl.R16F, probeSize);
        this.internalFormat = gl.R16F;
      }
      lastDiag = {
        chosen: this.internalFormat === gl.R32F ? "r32f" : "r16f",
        r32fProbe: r32,
        r16fProbe: r16,
        renderer,
      };
    }
  }

  private key(varId: number, t: number, z: number, x: number, y: number): string {
    return `${varId}|${t}|${z}|${x}|${y}`;
  }

  // Value subtracted from every sample of `variable` (0 when shifting is off).
  // Renderers must subtract this from range uniforms / thresholds before upload.
  getBaseline(variable: Variable): number {
    if (!this.shiftByBaseline) return 0;
    let b = this.baselines.get(variable.id);
    if (b === undefined) {
      b = Number.isFinite(variable.range.min) ? variable.range.min : 0;
      this.baselines.set(variable.id, b);
    }
    return b;
  }

  hasExact(variable: Variable, t: number, z: number, x: number, y: number): boolean {
    return this.cache.has(this.key(variable.id, t, z, x, y));
  }

  // Exact-level tile or a parent with UV offset/scale, or null. Bumps LRU.
  findTex(
    variable: Variable,
    t: number,
    z: number,
    x: number,
    y: number,
  ): TexRef | null {
    const k = this.key(variable.id, t, z, x, y);
    const tex = this.cache.get(k);
    if (tex) return { tex, ox: 0, oy: 0, s: 1 };

    const minZ = this.wmt.zoomRange.min;
    for (let dz = 1; dz <= this.parentLevels && z - dz >= minZ; dz++) {
      const za = z - dz;
      const xa = x >> dz;
      const ya = y >> dz;
      const ak = this.key(variable.id, t, za, xa, ya);
      const at = this.cache.get(ak);
      if (at) {
        const inv = 1 / (1 << dz);
        return {
          tex: at,
          ox: (x - (xa << dz)) * inv,
          oy: (y - (ya << dz)) * inv,
          s: inv,
        };
      }
    }
    return null;
  }

  // findTex without parent fallback.
  findExactTex(
    variable: Variable,
    t: number,
    z: number,
    x: number,
    y: number,
  ): WebGLTexture | null {
    return this.cache.get(this.key(variable.id, t, z, x, y));
  }

  // Idempotent batch request per (variable, t). In-flight dupes are ignored,
  // post-invalidate results dropped.
  requestTiles(variable: Variable, t: number, coords: readonly TileCoord[]): void {
    if (this.disposed || coords.length === 0) return;
    const fetchKey = `${variable.id}|${t}|batch`;
    if (this.pending.has(fetchKey)) return;
    const gen = this.gen;
    const p = (async () => {
      try {
        const arrays = await variable.tiles({ time: t, coords });
        if (gen !== this.gen || this.disposed) return;
        const baseline = this.getBaseline(variable);
        for (let i = 0; i < coords.length; i++) {
          const c = coords[i];
          const k = this.key(variable.id, t, c.z, c.x, c.y);
          if (this.cache.has(k)) continue;
          this.uploadTile(k, arrays[i], baseline);
        }
        this.onUpdate?.();
      } catch (err) {
        console.error("wmtiles: tile batch failed", err);
      } finally {
        this.pending.delete(fetchKey);
      }
    })();
    this.pending.set(fetchKey, p);
  }

  private uploadTile(key: string, pixels: Float32Array, baseline: number): void {
    const gl = this.gl;
    const tex = gl.createTexture();
    if (!tex) return;

    // NaN -> sentinel + optional baseline shift, fused into the diagnostics
    // sweep so it stays one pass. Both fixes work around mobile GLSL: NaN
    // doesn't round-trip on Apple/Mali/Adreno, and highp gets demoted so
    // (v - vmin) collapses when both sides are ~1e5. Caller doesn't retain
    // `pixels`, so mutation in place is fine.
    dataDiag.tilesUploaded++;
    let nans = 0;
    const head: number[] = [];
    for (let i = 0; i < pixels.length; i++) {
      const v = pixels[i];
      if (v !== v) { nans++; pixels[i] = MISSING_SENTINEL; continue; }
      if (v < dataDiag.minSample) dataDiag.minSample = v;
      if (v > dataDiag.maxSample) dataDiag.maxSample = v;
      if (baseline !== 0) pixels[i] = v - baseline;
    }
    dataDiag.nanCount += nans;
    for (let i = 0; i < Math.min(6, pixels.length); i++) head.push(pixels[i]);
    dataDiag.lastTileHead = head;

    gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.texImage2D(
      gl.TEXTURE_2D,
      0,
      this.internalFormat,
      this.tileSize,
      this.tileSize,
      0,
      gl.RED,
      gl.FLOAT,
      pixels,
    );
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    this.cache.set(key, tex);
  }

  // drops in-flight fetch results, cache stays
  invalidate(): void {
    this.gen++;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.cache.dispose();
    this.pending.clear();
  }
}
