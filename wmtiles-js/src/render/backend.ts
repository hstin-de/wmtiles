// composable screen-mode vs matrix-mode helpers, picked per renderer

import type { GlobeShaderData } from "./globe.js";

export interface ProgramFactory<P> {
  buildScreen(gl: WebGL2RenderingContext): P;
  buildMatrix(gl: WebGL2RenderingContext, shaderData: GlobeShaderData): P;
  destroy(gl: WebGL2RenderingContext, prog: P): void;
}

export class VariantProgramCache<P> {
  private screenProg: P | null;
  private readonly matrixProgs = new Map<string, P>();

  constructor(
    private readonly gl: WebGL2RenderingContext,
    private readonly matrixMode: boolean,
    private readonly factory: ProgramFactory<P>,
  ) {
    // compile up front; screen mode always needs its one program
    this.screenProg = matrixMode ? null : factory.buildScreen(gl);
  }

  // matrix mode keys the program by variant (mercator <-> globe); screen
  // mode ignores shaderData
  get(shaderData: GlobeShaderData | null): P {
    if (!this.matrixMode) return this.screenProg as P;
    if (!shaderData) {
      throw new Error("matrix-mode program cache needs shaderData");
    }
    let prog = this.matrixProgs.get(shaderData.variantName);
    if (!prog) {
      prog = this.factory.buildMatrix(this.gl, shaderData);
      this.matrixProgs.set(shaderData.variantName, prog);
    }
    return prog;
  }

  dispose(): void {
    if (this.screenProg) this.factory.destroy(this.gl, this.screenProg);
    this.screenProg = null;
    for (const p of this.matrixProgs.values()) {
      this.factory.destroy(this.gl, p);
    }
    this.matrixProgs.clear();
  }
}

// rAF loop in screen mode, host poke in matrix mode; particles drives its
// own loop and skips this
export class RedrawScheduler {
  private raf = 0;
  private disposed = false;

  constructor(
    private readonly matrixMode: boolean,
    private readonly render: () => void,
    private readonly onRedraw: (() => void) | undefined,
  ) {}

  schedule(): void {
    if (this.disposed) return;
    if (this.matrixMode) {
      this.onRedraw?.();
      return;
    }
    if (this.raf) return;
    this.raf = requestAnimationFrame(() => {
      this.raf = 0;
      if (!this.disposed) this.render();
    });
  }

  dispose(): void {
    this.disposed = true;
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
  }
}

// resets GL state the host may have left dirty; does not bind a framebuffer,
// callers with their own FBOs bind null first
export function beginHostFrame(
  gl: WebGL2RenderingContext,
  width: number,
  height: number,
  blend: "premultiplied" | "straight" = "premultiplied",
): void {
  gl.viewport(0, 0, width, height);
  gl.enable(gl.BLEND);
  gl.blendFunc(
    blend === "premultiplied" ? gl.ONE : gl.SRC_ALPHA,
    gl.ONE_MINUS_SRC_ALPHA,
  );
  gl.disable(gl.DEPTH_TEST);
  gl.depthMask(false);
  gl.disable(gl.CULL_FACE);
  gl.disable(gl.SCISSOR_TEST);
}
