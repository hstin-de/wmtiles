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

export interface TilePixel {
  x: number;
  y: number;
  col: number;
  row: number;
}

// Web Mercator pole limit: atan(sinh(π))·180/π
const MERC_LAT_CUTOFF = 85.05112877980659;

// inverse of tiler.PixelToLatLon: returns the (tile, pixel) containing (lat, lon).
// returns null for non-finite inputs or |lat|>90. longitude wraps to [-180, 180).
export function latLonToTilePixel(
  z: number,
  lat: number,
  lon: number,
  pixSize: number,
): TilePixel | null {
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) return null;
  if (lat > 90 || lat < -90) return null;
  if (lat > MERC_LAT_CUTOFF) lat = MERC_LAT_CUTOFF;
  if (lat < -MERC_LAT_CUTOFF) lat = -MERC_LAT_CUTOFF;
  // wrap to [-180, 180); 180 collapses to -180 (same antimeridian point)
  const wrapped = (((lon + 180) % 360) + 360) % 360 - 180;

  const n = 1 << z;
  const xmerc = (wrapped + 180) / 360;
  const latRad = (lat * Math.PI) / 180;
  const ymerc =
    (1 - Math.log(Math.tan(latRad) + 1 / Math.cos(latRad)) / Math.PI) / 2;

  const xt = xmerc * n;
  const yt = ymerc * n;
  let x = Math.floor(xt);
  let y = Math.floor(yt);
  if (x < 0) x = 0;
  if (x >= n) x = n - 1;
  if (y < 0) y = 0;
  if (y >= n) y = n - 1;

  let col = Math.floor((xt - x) * pixSize);
  let row = Math.floor((yt - y) * pixSize);
  if (col < 0) col = 0;
  if (col >= pixSize) col = pixSize - 1;
  if (row < 0) row = 0;
  if (row >= pixSize) row = pixSize - 1;

  return { x, y, col, row };
}
