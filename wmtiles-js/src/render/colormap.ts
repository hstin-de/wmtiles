// Colormaps are either a list of RGB stops (uniformly spaced, mixed in shader)
// or raw GLSL that defines `vec3 colormap(float t)`.

export type RGB = readonly [number, number, number];

export interface StopColormap {
  kind: "stops";
  stops: readonly RGB[];
}

export interface GLSLColormap {
  kind: "glsl";
  // Body that defines exactly one function with the signature:
  //   vec3 colormap(float t)   // t in [0,1], returns linear RGB in [0,1]
  body: string;
}

export type Colormap = StopColormap | GLSLColormap;

// Stops are 0..255 in source data, divided by 255 in the generated GLSL.
const viridisStops: RGB[] = [
  [ 68,   1,  84],
  [ 72,  35, 116],
  [ 64,  67, 135],
  [ 52,  94, 141],
  [ 41, 121, 142],
  [ 32, 144, 140],
  [ 34, 167, 132],
  [ 68, 190, 112],
  [253, 231,  37],
];

const plasmaStops: RGB[] = [
  [ 13,   8, 135],
  [ 75,   3, 161],
  [125,   3, 168],
  [168,  34, 150],
  [203,  70, 121],
  [229, 107,  93],
  [248, 148,  65],
  [253, 195,  40],
  [240, 249,  33],
];

const infernoStops: RGB[] = [
  [  0,   0,   4],
  [ 31,  12,  72],
  [ 85,  15, 109],
  [136,  34, 106],
  [186,  54,  85],
  [227,  89,  51],
  [249, 140,  10],
  [249, 201,  50],
  [252, 255, 164],
];

const grayStops: RGB[] = [
  [  0,   0,   0],
  [255, 255, 255],
];

const whiteStops: RGB[] = [
  [255, 255, 255],
  [255, 255, 255],
];

// matplotlib RdBu reversed, diverging (blue-white-red) for anomaly fills
const rdbuStops: RGB[] = [
  [  5,  48,  97],
  [ 33, 102, 172],
  [ 67, 147, 195],
  [146, 197, 222],
  [209, 229, 240],
  [247, 247, 247],
  [253, 219, 199],
  [244, 165, 130],
  [214,  96,  77],
  [178,  24,  43],
  [103,   0,  31],
];

// Discrete two-tone (blue<0.5, red>=0.5) for sharp high/low fills
const hilowColormap: GLSLColormap = {
  kind: "glsl",
  body: `
vec3 colormap(float t) {
  return t < 0.5
    ? vec3(0.16, 0.35, 0.78)
    : vec3(0.83, 0.18, 0.18);
}
`,
};

export const builtinColormaps = {
  viridis: { kind: "stops", stops: viridisStops } as StopColormap,
  plasma:  { kind: "stops", stops: plasmaStops  } as StopColormap,
  inferno: { kind: "stops", stops: infernoStops } as StopColormap,
  gray:    { kind: "stops", stops: grayStops    } as StopColormap,
  white:   { kind: "stops", stops: whiteStops   } as StopColormap,
  rdbu:    { kind: "stops", stops: rdbuStops    } as StopColormap,
  hilow:   hilowColormap,
} as const;

export type BuiltinColormapName = keyof typeof builtinColormaps;

export function resolveColormap(
  cm: Colormap | BuiltinColormapName | undefined,
): Colormap {
  if (cm === undefined) return builtinColormaps.viridis;
  if (typeof cm === "string") {
    const hit = builtinColormaps[cm];
    if (!hit) throw new Error(`unknown builtin colormap: ${cm}`);
    return hit;
  }
  return cm;
}

// Inlined into a fragment shader, defines `vec3 colormap(float t)`. No
// #version/precision header.
export function colormapToGLSL(cm: Colormap): string {
  if (cm.kind === "glsl") return cm.body;

  const n = cm.stops.length;
  if (n < 2) throw new Error("stop colormap needs at least 2 stops");

  const arr = cm.stops
    .map((s) => `vec3(${s[0].toFixed(1)}, ${s[1].toFixed(1)}, ${s[2].toFixed(1)})`)
    .join(",\n  ");

  return `
const int CM_N = ${n};
const vec3 CM_STOPS[${n}] = vec3[${n}](
  ${arr}
);
vec3 colormap(float t) {
  t = clamp(t, 0.0, 1.0);
  float f = t * float(CM_N - 1);
  int i = int(min(float(CM_N - 2), floor(f)));
  float frac = f - float(i);
  return mix(CM_STOPS[i], CM_STOPS[i + 1], frac) / 255.0;
}
`;
}
