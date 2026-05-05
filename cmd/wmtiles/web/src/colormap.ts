// 9-stop sample of matplotlib's viridis: perceptually uniform, good for scientific data
const VIRIDIS: Array<[number, number, number]> = [
  [68, 1, 84],
  [72, 35, 116],
  [64, 67, 135],
  [52, 94, 141],
  [41, 121, 142],
  [32, 144, 140],
  [34, 167, 132],
  [68, 190, 112],
  [253, 231, 37],
];

export function viridis(t: number): [number, number, number] {
  if (t <= 0) return VIRIDIS[0];
  if (t >= 1) return VIRIDIS[VIRIDIS.length - 1];
  const n = VIRIDIS.length - 1;
  const f = t * n;
  const i = Math.min(n - 1, f | 0);
  const frac = f - i;
  const a = VIRIDIS[i];
  const b = VIRIDIS[i + 1];
  return [
    (a[0] + (b[0] - a[0]) * frac) | 0,
    (a[1] + (b[1] - a[1]) * frac) | 0,
    (a[2] + (b[2] - a[2]) * frac) | 0,
  ];
}

export function renderTile(
  pixels: Float32Array,
  pixSize: number,
  vmin: number,
  vmax: number,
  ctx: CanvasRenderingContext2D,
): void {
  const span = vmax - vmin || 1;
  const img = ctx.createImageData(pixSize, pixSize);
  const data = img.data;
  for (let i = 0; i < pixels.length; i++) {
    const v = pixels[i];
    if (Number.isNaN(v)) {
      data[4 * i + 3] = 0; // NaN → fully transparent so the basemap shows through
      continue;
    }
    const [r, g, b] = viridis((v - vmin) / span);
    data[4 * i] = r;
    data[4 * i + 1] = g;
    data[4 * i + 2] = b;
    data[4 * i + 3] = 255;
  }
  ctx.putImageData(img, 0, 0);
}
