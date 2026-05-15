export interface GlobeShaderData {
  variantName: string;
  vertexShaderPrelude: string;
  define: string;
}

export interface GlobeProjectionData {
  mainMatrix: ArrayLike<number>;
  tileMercatorCoords: [number, number, number, number];
  clippingPlane: [number, number, number, number];
  projectionTransition: number;
  fallbackMatrix: ArrayLike<number>;
}

export interface ProjectionUniformLocs {
  matrix: WebGLUniformLocation | null;
  tileMercator: WebGLUniformLocation | null;
  clipping: WebGLUniformLocation | null;
  transition: WebGLUniformLocation | null;
  fallback: WebGLUniformLocation | null;
}

export function getProjectionUniformLocs(
  gl: WebGL2RenderingContext,
  program: WebGLProgram,
): ProjectionUniformLocs {
  return {
    matrix: gl.getUniformLocation(program, "u_projection_matrix"),
    tileMercator: gl.getUniformLocation(
      program,
      "u_projection_tile_mercator_coords",
    ),
    clipping: gl.getUniformLocation(program, "u_projection_clipping_plane"),
    transition: gl.getUniformLocation(program, "u_projection_transition"),
    fallback: gl.getUniformLocation(program, "u_projection_fallback_matrix"),
  };
}

const SCRATCH16 = new Float32Array(16);
function asFloat32(m: ArrayLike<number>): Float32Array {
  if (m instanceof Float32Array) return m;
  for (let i = 0; i < 16; i++) SCRATCH16[i] = m[i];
  return SCRATCH16;
}

export function setProjectionUniforms(
  gl: WebGL2RenderingContext,
  locs: ProjectionUniformLocs,
  data: GlobeProjectionData,
): void {
  if (locs.matrix) {
    gl.uniformMatrix4fv(locs.matrix, false, asFloat32(data.mainMatrix));
  }
  if (locs.tileMercator) {
    gl.uniform4fv(locs.tileMercator, data.tileMercatorCoords);
  }
  if (locs.clipping) {
    gl.uniform4fv(locs.clipping, data.clippingPlane);
  }
  if (locs.transition) {
    gl.uniform1f(locs.transition, data.projectionTransition);
  }
  if (locs.fallback) {
    gl.uniformMatrix4fv(locs.fallback, false, asFloat32(data.fallbackMatrix));
  }
}

export interface SubdividedQuad {
  vao: WebGLVertexArrayObject;
  indexCount: number;
}

export function buildSubdividedQuadVAO(
  gl: WebGL2RenderingContext,
  segments: number,
): SubdividedQuad {
  const n = Math.max(1, segments | 0);
  const verts = new Float32Array((n + 1) * (n + 1) * 2);
  let vi = 0;
  for (let y = 0; y <= n; y++) {
    for (let x = 0; x <= n; x++) {
      verts[vi++] = x / n;
      verts[vi++] = y / n;
    }
  }
  const idx = new Uint16Array(n * n * 6);
  let ii = 0;
  for (let y = 0; y < n; y++) {
    for (let x = 0; x < n; x++) {
      const a = y * (n + 1) + x;
      const b = a + 1;
      const c = a + (n + 1);
      const d = c + 1;
      idx[ii++] = a; idx[ii++] = b; idx[ii++] = c;
      idx[ii++] = b; idx[ii++] = d; idx[ii++] = c;
    }
  }
  const vbo = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, vbo);
  gl.bufferData(gl.ARRAY_BUFFER, verts, gl.STATIC_DRAW);
  const ibo = gl.createBuffer();
  gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, ibo);
  gl.bufferData(gl.ELEMENT_ARRAY_BUFFER, idx, gl.STATIC_DRAW);
  const vao = gl.createVertexArray();
  if (!vao) throw new Error("createVertexArray failed");
  gl.bindVertexArray(vao);
  gl.bindBuffer(gl.ARRAY_BUFFER, vbo);
  gl.enableVertexAttribArray(0);
  gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
  gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, ibo);
  gl.bindVertexArray(null);
  gl.bindBuffer(gl.ARRAY_BUFFER, null);
  gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, null);
  return { vao, indexCount: idx.length };
}
