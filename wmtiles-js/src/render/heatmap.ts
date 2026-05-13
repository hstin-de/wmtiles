import type { TileCoord, Variable, WMT } from "../reader.js";
import {
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
import { buildProgram, buildQuadVAO, type ProgramHandles } from "./shader.js";
import { TileSource, type TexRef, type TileSourceOptions } from "./source.js";
import { computeTimeWindow } from "./time.js";

export interface HeatmapRendererOptions extends TileSourceOptions {
  colormap?: Colormap | BuiltinColormapName;
  alpha?: number;
  childFallback?: boolean;
  prefetchNext?: boolean;
  disableTimeLerp?: boolean;
  onFrame?: (frameMs: number) => void; // cpu time per draw, for FPS overlays
}

export interface HeatmapRendererState {
  variable: Variable;
  t: number;
  vmin: number;
  vmax: number;
}

export interface TileDrawRect {
  z: number;
  x: number; // wrapped, [0, 2^z), use for cache lookup / fetch
  y: number;
  // un-wrapped column. equals x on canonical world, differs by 2^z on wrap
  // copies. atlas-style renderers filter to worldX === x.
  worldX: number;
  sx0: number;
  sy0: number;
  sx1: number;
  sy1: number;
}

const DEFAULTS = {
  alpha: 0.85,
  childFallback: true,
  prefetchNext: true,
  disableTimeLerp: false,
} as const;

export class HeatmapRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly prog: ProgramHandles;
  private readonly vao: WebGLVertexArrayObject;
  private readonly source: TileSource;
  private readonly ownsSource: boolean;

  private readonly opts: Required<
    Pick<
      HeatmapRendererOptions,
      "alpha" | "childFallback" | "prefetchNext" | "disableTimeLerp"
    >
  >;
  private readonly onFrame?: (frameMs: number) => void;
  private view: TileDrawRect[] = [];
  private raf = 0;
  private disposed = false;

  state: HeatmapRendererState;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    private readonly wmt: WMT,
    options?: HeatmapRendererOptions,
    source?: TileSource,
  ) {
    const gl = canvas.getContext("webgl2", {
      premultipliedAlpha: false,
      antialias: false,
      preserveDrawingBuffer: true,
    }) as WebGL2RenderingContext | null;
    if (!gl) throw new Error("WebGL2 not supported");
    this.gl = gl;

    const colormap = resolveColormap(options?.colormap);
    this.opts = {
      alpha: options?.alpha ?? DEFAULTS.alpha,
      childFallback: options?.childFallback ?? DEFAULTS.childFallback,
      prefetchNext: options?.prefetchNext ?? DEFAULTS.prefetchNext,
      disableTimeLerp: options?.disableTimeLerp ?? DEFAULTS.disableTimeLerp,
    };
    this.onFrame = options?.onFrame;

    this.prog = buildProgram(gl, colormap);
    this.vao = buildQuadVAO(gl);

    if (source) {
      this.source = source;
      this.ownsSource = false;
    } else {
      this.source = new TileSource(gl, wmt, {
        ...options,
        onUpdate: () => this.scheduleDraw(),
        // mobile highp collapses (largeVal - largeVal), pre-shift to keep deltas small
        shiftValuesByBaseline: true,
      });
      this.ownsSource = true;
    }

    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);

    this.state = {
      variable: wmt.variables[0],
      t: 0,
      vmin: 0,
      vmax: 1,
    };
  }

  setState(patch: Partial<HeatmapRendererState>): void {
    if (this.disposed) return;
    Object.assign(this.state, patch);
    this.source.invalidate();
    this.scheduleDraw();
  }

  setView(tiles: TileDrawRect[]): void {
    if (this.disposed) return;
    this.view = tiles;
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
    this.gl.viewport(0, 0, w, h);
    this.scheduleDraw();
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    if (this.ownsSource) this.source.dispose();
    const gl = this.gl;
    gl.deleteProgram(this.prog.program);
    gl.deleteVertexArray(this.vao);
  }

  private scheduleDraw(): void {
    if (this.raf || this.disposed) return;
    this.raf = requestAnimationFrame(() => {
      this.raf = 0;
      if (!this.disposed) this.draw();
    });
  }

  private drawPair(
    A: TexRef,
    B: TexRef,
    lerp: number,
    sx0: number,
    sy0: number,
    sx1: number,
    sy1: number,
  ): void {
    const gl = this.gl;
    const u = this.prog;
    gl.activeTexture(gl.TEXTURE0);
    gl.bindTexture(gl.TEXTURE_2D, A.tex);
    gl.activeTexture(gl.TEXTURE1);
    gl.bindTexture(gl.TEXTURE_2D, B.tex);
    gl.uniform4f(u.uRect, sx0, sy0, sx1, sy1);
    gl.uniform2f(u.uUvOffA, A.ox, A.oy);
    gl.uniform2f(u.uUvScaleA, A.s, A.s);
    gl.uniform2f(u.uUvOffB, B.ox, B.oy);
    gl.uniform2f(u.uUvScaleB, B.s, B.s);
    gl.uniform1f(u.uLerp, lerp);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
  }

  private attempt(
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
  ): boolean {
    const A = this.source.findTex(variable, tF, z, x, y);
    const B = frac > 0 ? this.source.findTex(variable, tC, z, x, y) : A;
    if (A && B) {
      this.drawPair(A, B, frac, sx0, sy0, sx1, sy1);
      return true;
    }
    if (A) {
      this.drawPair(A, A, 0, sx0, sy0, sx1, sy1);
      return true;
    }
    if (B) {
      this.drawPair(B, B, 0, sx0, sy0, sx1, sy1);
      return true;
    }
    return false;
  }

  private draw(): void {
    const gl = this.gl;
    if (gl.canvas.width <= 1 || gl.canvas.height <= 1) return;
    const tStart = performance.now();

    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);

    const view = this.view;
    if (view.length === 0) {
      this.onFrame?.(performance.now() - tStart);
      return;
    }

    const { variable, t, vmin, vmax } = this.state;
    const { tF, tC, frac, tP } = computeTimeWindow(this.wmt, t, this.opts);

    // samples were shifted in source.ts, shift the range uniform to match
    const baseline = this.source.getBaseline(variable);

    gl.useProgram(this.prog.program);
    gl.bindVertexArray(this.vao);
    gl.uniform2f(this.prog.uScreen, gl.canvas.width, gl.canvas.height);
    gl.uniform2f(this.prog.uRange, vmin - baseline, vmax - baseline);
    gl.uniform1f(this.prog.uAlpha, this.opts.alpha);
    gl.uniform1i(this.prog.uTexA, 0);
    gl.uniform1i(this.prog.uTexB, 1);

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
        variable, tF, tC, frac,
        r.z, r.x, r.y,
        r.sx0, r.sy0, r.sx1, r.sy1,
      );

      if (!drew && childOK && r.z + 1 <= maxZ) {
        const cz = r.z + 1;
        const w = r.sx1 - r.sx0;
        const h = r.sy1 - r.sy0;
        for (let cy = 0; cy < 2; cy++) {
          for (let cx = 0; cx < 2; cx++) {
            this.attempt(
              variable, tF, tC, frac, cz,
              r.x * 2 + cx, r.y * 2 + cy,
              r.sx0 + (cx * w) / 2,
              r.sy0 + (cy * h) / 2,
              r.sx0 + ((cx + 1) * w) / 2,
              r.sy0 + ((cy + 1) * h) / 2,
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
