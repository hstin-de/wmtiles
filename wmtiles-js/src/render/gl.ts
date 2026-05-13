// WebGL2 boilerplate shared across every renderer.

export function compileShader(
  gl: WebGL2RenderingContext,
  type: number,
  src: string,
): WebGLShader {
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

export function linkProgram(
  gl: WebGL2RenderingContext,
  vs: string,
  fs: string,
): WebGLProgram {
  const v = compileShader(gl, gl.VERTEX_SHADER, vs);
  const f = compileShader(gl, gl.FRAGMENT_SHADER, fs);
  const p = gl.createProgram();
  if (!p) throw new Error("createProgram failed");
  gl.attachShader(p, v);
  gl.attachShader(p, f);
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) {
    const log = gl.getProgramInfoLog(p) ?? "";
    throw new Error("link: " + log);
  }
  return p;
}

// Defaults: R32F / RED / FLOAT / nearest, the tile-storage shape every
// renderer wants.
export interface CreateTextureOptions {
  internalFormat?: number;
  format?: number;
  type?: number;
  filter?: "nearest" | "linear";
  pixels?: ArrayBufferView | null;
}

export function createTexture(
  gl: WebGL2RenderingContext,
  width: number,
  height: number,
  opts: CreateTextureOptions = {},
): WebGLTexture {
  const tex = gl.createTexture();
  if (!tex) throw new Error("createTexture failed");
  const internalFormat = opts.internalFormat ?? gl.R32F;
  const format = opts.format ?? gl.RED;
  const type = opts.type ?? gl.FLOAT;
  const filter = opts.filter === "linear" ? gl.LINEAR : gl.NEAREST;
  const pixels = opts.pixels ?? null;
  gl.bindTexture(gl.TEXTURE_2D, tex);
  gl.texImage2D(
    gl.TEXTURE_2D, 0, internalFormat, width, height, 0, format, type, pixels,
  );
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, filter);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, filter);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  return tex;
}

export function createFBO(
  gl: WebGL2RenderingContext,
  tex: WebGLTexture,
): WebGLFramebuffer {
  const fbo = gl.createFramebuffer();
  if (!fbo) throw new Error("createFramebuffer failed");
  gl.bindFramebuffer(gl.FRAMEBUFFER, fbo);
  gl.framebufferTexture2D(
    gl.FRAMEBUFFER,
    gl.COLOR_ATTACHMENT0,
    gl.TEXTURE_2D,
    tex,
    0,
  );
  return fbo;
}

// Unit-quad VAO, vec2 attribute at location 0, TRIANGLE_STRIP of 4. Caller
// maps to NDC themselves (atlas slot or fullscreen).
export function buildQuadVAO(gl: WebGL2RenderingContext): WebGLVertexArrayObject {
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
  return vao;
}

export const VS_FULLSCREEN = `#version 300 es
precision highp float;
layout(location=0) in vec2 a_pos;
out vec2 v_uv;
void main() {
  v_uv = a_pos;
  gl_Position = vec4(a_pos * 2.0 - 1.0, 0.0, 1.0);
}`;

// Maps the unit square to NDC rect u_slot = (x0,y0,x1,y1).
export const VS_ATLAS_SLOT = `#version 300 es
precision highp float;
layout(location=0) in vec2 a_pos;
uniform vec4 u_slot;     // x0,y0,x1,y1 in NDC
out vec2 v_uv;
void main() {
  v_uv = a_pos;
  vec2 p = mix(u_slot.xy, u_slot.zw, a_pos);
  gl_Position = vec4(p, 0.0, 1.0);
}`;
