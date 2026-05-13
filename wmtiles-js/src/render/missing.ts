// Missing-data convention shared by every renderer. NaN isn't safe to ship to
// the GPU: mobile GLSL frequently breaks `v != v`, doesn't propagate NaN
// through arithmetic, and demotes highp to FP16. So we stamp a sentinel that
// sits below every real shifted sample but inside FP16 range, and shaders use
// a magnitude check.

export const MISSING_SENTINEL = -1e4;  // inside FP16 range
export const MISSING_THRESHOLD = -1e3; // v < threshold == missing

// Drop into any shader handling tile data. Defines isMissing, lerpR, packR,
// packRG. All helpers propagate missingness so atlas slots stay clean.
export const MISSING_GLSL_PREAMBLE = `
const float MISSING_THRESHOLD = -1e3;
const float MISSING_SENTINEL  = -1e4;
bool isMissing(float v) { return v < MISSING_THRESHOLD; }
float lerpR(float a, float b, float t) {
  return (isMissing(a) || isMissing(b)) ? MISSING_SENTINEL : mix(a, b, t);
}
vec4 packR(float v) {
  return isMissing(v)
    ? vec4(MISSING_SENTINEL, 0.0, 0.0, 1.0)
    : vec4(v, 0.0, 0.0, 1.0);
}
vec4 packRG(float r, float g) {
  return (isMissing(r) || isMissing(g))
    ? vec4(MISSING_SENTINEL, MISSING_SENTINEL, 0.0, 1.0)
    : vec4(r, g, 0.0, 1.0);
}
`;

// Missing-aware manual bilinear on an R-channel texture. Requires the
// preamble above. Any missing tap collapses the output to MISSING_SENTINEL,
// which is what fwidth() needs to stay sane at the data boundary.
export const BILINEAR_R_GLSL = `
float bilinearR(sampler2D tex, vec2 uv) {
  vec2 sz = vec2(textureSize(tex, 0));
  vec2 px = uv * sz - 0.5;
  vec2 i = floor(px);
  vec2 fr = px - i;
  vec2 base = (i + 0.5) / sz;
  vec2 dxv = vec2(1.0 / sz.x, 0.0);
  vec2 dyv = vec2(0.0, 1.0 / sz.y);
  float v00 = texture(tex, base).r;
  float v10 = texture(tex, base + dxv).r;
  float v01 = texture(tex, base + dyv).r;
  float v11 = texture(tex, base + dxv + dyv).r;
  if (isMissing(min(min(v00, v10), min(v01, v11)))) return MISSING_SENTINEL;
  return mix(mix(v00, v10, fr.x), mix(v01, v11, fr.x), fr.y);
}
`;
