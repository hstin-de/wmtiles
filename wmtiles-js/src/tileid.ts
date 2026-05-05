// (4^z - 1)/3: see Go tileid.ZoomOffset; bigint because high zooms overflow Number
export function zoomOffset(z: number): bigint {
  return ((1n << BigInt(2 * z)) - 1n) / 3n;
}

// mirror of Go hilbert.XY2D: must match exactly or tile lookup breaks
export function hilbertXY2D(z: number, x: number, y: number): bigint {
  let d = 0n;
  let xi = x | 0;
  let yi = y | 0;
  for (let s = (1 << z) >>> 1; s > 0; s >>>= 1) {
    const rx = xi & s ? 1 : 0;
    const ry = yi & s ? 1 : 0;
    d += BigInt(s) * BigInt(s) * BigInt((3 * rx) ^ ry);
    if (ry === 0) {
      if (rx === 1) {
        xi = s - 1 - xi;
        yi = s - 1 - yi;
      }
      const tmp = xi;
      xi = yi;
      yi = tmp;
    }
  }
  return d;
}

export function encode3D(z: number, x: number, y: number): bigint {
  return zoomOffset(z) + hilbertXY2D(z, x, y);
}
