import { colormapToGLSL, type Colormap } from "./colormap.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
import { buildQuadVAO, compileShader } from "./gl.js";

export { buildQuadVAO };

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

function buildFS(colormap: Colormap): string {
  return `#version 300 es
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

${MISSING_GLSL_PREAMBLE}
${colormapToGLSL(colormap)}

void main() {
  float vA = texture(u_texA, u_uvOffA + v_uv * u_uvScaleA).r;
  float vB = texture(u_texB, u_uvOffB + v_uv * u_uvScaleB).r;
  if (isMissing(vA) || isMissing(vB)) discard;
  float v = mix(vA, vB, u_lerp);
  float t = (v - u_range.x) / max(u_range.y - u_range.x, 1e-30);
  outColor = vec4(colormap(t), u_alpha);
}`;
}

export interface ProgramHandles {
  program: WebGLProgram;
  uScreen: WebGLUniformLocation | null;
  uRect: WebGLUniformLocation | null;
  uRange: WebGLUniformLocation | null;
  uAlpha: WebGLUniformLocation | null;
  uTexA: WebGLUniformLocation | null;
  uTexB: WebGLUniformLocation | null;
  uUvOffA: WebGLUniformLocation | null;
  uUvScaleA: WebGLUniformLocation | null;
  uUvOffB: WebGLUniformLocation | null;
  uUvScaleB: WebGLUniformLocation | null;
  uLerp: WebGLUniformLocation | null;
}

export function buildProgram(
  gl: WebGL2RenderingContext,
  colormap: Colormap,
): ProgramHandles {
  const vs = compileShader(gl, gl.VERTEX_SHADER, VS);
  const fs = compileShader(gl, gl.FRAGMENT_SHADER, buildFS(colormap));
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
    uScreen:   gl.getUniformLocation(program, "u_screen"),
    uRect:     gl.getUniformLocation(program, "u_rect"),
    uRange:    gl.getUniformLocation(program, "u_range"),
    uAlpha:    gl.getUniformLocation(program, "u_alpha"),
    uTexA:     gl.getUniformLocation(program, "u_texA"),
    uTexB:     gl.getUniformLocation(program, "u_texB"),
    uUvOffA:   gl.getUniformLocation(program, "u_uvOffA"),
    uUvScaleA: gl.getUniformLocation(program, "u_uvScaleA"),
    uUvOffB:   gl.getUniformLocation(program, "u_uvOffB"),
    uUvScaleB: gl.getUniformLocation(program, "u_uvScaleB"),
    uLerp:     gl.getUniformLocation(program, "u_lerp"),
  };
}

