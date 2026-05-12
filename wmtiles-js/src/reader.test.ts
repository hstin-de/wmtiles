import { test, expect } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import {
  WMT,
  open,
  bytesSource,
  latLonToTilePixel,
  UnknownVariableError,
  TimeOutOfRangeError,
} from "./index";

const TESTDATA = resolve(__dirname, "../../format/testdata");

function load(name: string): Uint8Array {
  return new Uint8Array(readFileSync(resolve(TESTDATA, name)));
}

test("opens minimal.wmt", async () => {
  const r = await open(load("minimal.wmt"));
  expect(r.snapshotGeneration).toBe(0);
  expect(r.variables.length).toBe(1);
  expect(r.variables[0].name).toBe("temp");
  expect(r.timeStepCount).toBe(1);
  expect(r.zoomRange.min).toBe(0);

  const px = await r.variable("temp").tile({ time: 0, z: 0, x: 0, y: 0 });
  expect(px).not.toBeNull();
  expect(px!.length).toBe(r.tileSize * r.tileSize);
  expect(Math.abs(px![0])).toBeLessThan(0.5);
});

test("opens extended.wmt with 3 variables", async () => {
  const r = await WMT.open(bytesSource(load("extended.wmt")));
  expect(r.snapshotGeneration).toBe(2);
  expect(r.variables.length).toBe(3);
  const names = r.variables.map((v) => v.name).sort();
  expect(names).toEqual(["precip", "temp", "wind"]);

  for (const v of r.variables) {
    const px = await v.tile({ time: 0, z: 0, x: 0, y: 0 });
    expect(px).not.toBeNull();
  }
});

test("variable() throws UnknownVariableError on miss", async () => {
  const r = await WMT.open(bytesSource(load("minimal.wmt")));
  expect(() => r.variable("nope")).toThrow(UnknownVariableError);
  expect(r.findVariable("nope")).toBeUndefined();
  expect(r.findVariable("temp")?.name).toBe("temp");
});

test("Variable.tiles returns one entry per coord", async () => {
  const r = await WMT.open(bytesSource(load("compacted.wmt")));
  const tiles = await r.variable("temp").tiles({
    time: 0,
    coords: [
      { z: 0, x: 0, y: 0 },
      { z: 5, x: 9, y: 9 },
    ],
  });
  expect(tiles.length).toBe(2);
  expect(Number.isNaN(tiles[1][0])).toBe(true);
});

test("timeAt and timeIndexOf round-trip", async () => {
  const r = await WMT.open(bytesSource(load("minimal.wmt")));
  const d = r.timeAt(0);
  expect(d).toBeInstanceOf(Date);
  expect(r.timeIndexOf(d)).toBe(0);
  expect(r.timeIndexOf(0)).toBe(0);
  expect(() => r.timeIndexOf(99)).toThrow(TimeOutOfRangeError);
  expect(() => r.timeIndexOf(new Date(d.getTime() + 1))).toThrow(
    TimeOutOfRangeError,
  );
});

test("timeAxis exposes Date-typed axis", async () => {
  const r = await WMT.open(bytesSource(load("minimal.wmt")));
  const axis = r.timeAxis;
  if (axis.kind === "regular") {
    expect(axis.start).toBeInstanceOf(Date);
    expect(typeof axis.intervalMs).toBe("number");
  } else {
    expect(axis.times[0]).toBeInstanceOf(Date);
  }
});

// mirrors tiler.PixelToLatLon in Go
function pixelCenterLatLon(
  z: number, x: number, y: number, pixSize: number, col: number, row: number,
) {
  const n = 1 << z;
  const xmerc = (x + (col + 0.5) / pixSize) / n;
  const ymerc = (y + (row + 0.5) / pixSize) / n;
  const lon = xmerc * 360 - 180;
  const lat = (Math.atan(Math.sinh(Math.PI * (1 - 2 * ymerc))) * 180) / Math.PI;
  return { lat, lon };
}

test("latLonToTilePixel round-trips pixel centers", () => {
  const pixSize = 256;
  for (const z of [0, 1, 5, 12]) {
    const n = 1 << z;
    const samples = [
      { x: 0, y: 0, col: 0, row: 0 },
      { x: n - 1, y: n - 1, col: pixSize - 1, row: pixSize - 1 },
      { x: (n / 2) | 0, y: (n / 2) | 0, col: 17, row: 42 },
    ];
    for (const s of samples) {
      const { lat, lon } = pixelCenterLatLon(z, s.x, s.y, pixSize, s.col, s.row);
      const got = latLonToTilePixel(z, lat, lon, pixSize);
      expect(got).not.toBeNull();
      expect(got!.x).toBe(s.x);
      expect(got!.y).toBe(s.y);
      expect(got!.col).toBe(s.col);
      expect(got!.row).toBe(s.row);
    }
  }
});

test("Variable.sample agrees with tile pixel", async () => {
  const r = await WMT.open(bytesSource(load("minimal.wmt")));
  const v = r.variable("temp");
  const cases = [
    { lat: 0, lon: 0 },
    { lat: 45, lon: -120 },
    { lat: -33.86, lon: 151.21 },
  ];
  for (const c of cases) {
    const px = latLonToTilePixel(0, c.lat, c.lon, r.tileSize)!;
    const tile = await v.tile({ time: 0, z: 0, x: px.x, y: px.y });
    const expected = tile![px.row * r.tileSize + px.col];
    const got = await v.sample({ time: 0, lat: c.lat, lon: c.lon, z: 0 });
    expect(got).toBe(expected);
  }
});

test("Variable.sample rejects invalid coords / out-of-range zoom", async () => {
  const r = await WMT.open(bytesSource(load("minimal.wmt")));
  const v = r.variable("temp");
  expect(await v.sample({ time: 0, lat: NaN, lon: 0 })).toBeNull();
  expect(await v.sample({ time: 0, lat: 91, lon: 0 })).toBeNull();
  expect(
    await v.sample({ time: 0, lat: 0, lon: 0, z: r.zoomRange.max + 1 }),
  ).toBeNull();
});

test("WMT.forecast returns one series per variable across all steps", async () => {
  const r = await WMT.open(bytesSource(load("multistep.wmt")));
  const fc = await r.forecast({
    lat: 0,
    lon: 0,
    variables: ["temp", "wind"],
  });
  expect(fc.times.length).toBe(4);
  expect(fc.times[0].toISOString()).toBe("2026-05-03T00:00:00.000Z");
  expect(fc.times[3].toISOString()).toBe("2026-05-03T03:00:00.000Z");
  // fixture encodes pixel = offset + step: temp offset 100, wind offset 200
  expect(Array.from(fc.values.temp)).toEqual([100, 101, 102, 103]);
  expect(Array.from(fc.values.wind)).toEqual([200, 201, 202, 203]);
});

test("WMT.forecast slices via timeRange with indices and Dates", async () => {
  const r = await WMT.open(bytesSource(load("multistep.wmt")));

  const byIndex = await r.forecast({
    lat: 0, lon: 0, variables: ["temp"],
    timeRange: { start: 1, end: 2 },
  });
  expect(byIndex.times.length).toBe(2);
  expect(Array.from(byIndex.values.temp)).toEqual([101, 102]);

  const byDate = await r.forecast({
    lat: 0, lon: 0, variables: ["temp"],
    timeRange: {
      start: new Date("2026-05-03T01:00:00Z"),
      end: new Date("2026-05-03T02:00:00Z"),
    },
  });
  expect(Array.from(byDate.values.temp)).toEqual([101, 102]);

  // Open-ended range: only start → runs to last step.
  const tail = await r.forecast({
    lat: 0, lon: 0, variables: ["temp"],
    timeRange: { start: 2 },
  });
  expect(Array.from(tail.values.temp)).toEqual([102, 103]);
});

test("WMT.forecast validates inputs and signals missing data with NaN", async () => {
  const r = await WMT.open(bytesSource(load("multistep.wmt")));

  // Unknown variable name must fail before any I/O.
  await expect(
    r.forecast({ lat: 0, lon: 0, variables: ["nope"] }),
  ).rejects.toBeInstanceOf(UnknownVariableError);

  // start > end → TimeOutOfRangeError.
  await expect(
    r.forecast({
      lat: 0, lon: 0, variables: ["temp"],
      timeRange: { start: 3, end: 1 },
    }),
  ).rejects.toBeInstanceOf(TimeOutOfRangeError);

  // Invalid coords → series stays NaN-filled at every slot.
  const fc = await r.forecast({
    lat: 91, lon: 0, variables: ["temp"],
  });
  expect(fc.values.temp.length).toBe(4);
  for (const v of fc.values.temp) expect(Number.isNaN(v)).toBe(true);
});

test("WMT.value returns a snapshot of many variables at one time", async () => {
  const r = await WMT.open(bytesSource(load("multistep.wmt")));

  // step 2 → temp = 102, wind = 202 (offset + step from fixture)
  const snap = await r.value({
    lat: 0, lon: 0, time: 2, variables: ["temp", "wind"],
  });
  expect(snap.time.toISOString()).toBe("2026-05-03T02:00:00.000Z");
  expect(snap.values.temp).toBe(102);
  expect(snap.values.wind).toBe(202);

  // Date-based time ref must produce the same result.
  const byDate = await r.value({
    lat: 0, lon: 0,
    time: new Date("2026-05-03T02:00:00Z"),
    variables: ["temp"],
  });
  expect(byDate.values.temp).toBe(102);

  // Unknown variable name throws before any I/O.
  await expect(
    r.value({ lat: 0, lon: 0, time: 0, variables: ["nope"] }),
  ).rejects.toBeInstanceOf(UnknownVariableError);

  // Invalid coords → NaN per variable (never null).
  const oob = await r.value({
    lat: 91, lon: 0, time: 0, variables: ["temp", "wind"],
  });
  expect(Number.isNaN(oob.values.temp)).toBe(true);
  expect(Number.isNaN(oob.values.wind)).toBe(true);
});

test("crc_corrupted.wmt falls back to previous snapshot", async () => {
  const r = await WMT.open(bytesSource(load("crc_corrupted.wmt")));
  const names = r.variables.map((v) => v.name).sort();
  expect(names).toContain("temp");
  expect(names).toContain("wind");
  expect(names).not.toContain("precip");
});
