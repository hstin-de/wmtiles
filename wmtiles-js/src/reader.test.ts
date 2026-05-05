import { test, expect } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { WMT, bytesFetcher } from "./index";

const TESTDATA = resolve(__dirname, "../../format/testdata");

function load(name: string): Uint8Array {
  return new Uint8Array(readFileSync(resolve(TESTDATA, name)));
}

test("opens minimal.wmt", async () => {
  const r = await new WMT(bytesFetcher(load("minimal.wmt"))).open();
  expect(r.header.formatVersion).toBe(1);
  expect(r.header.snapshotGeneration).toBe(0);
  expect(r.catalog.length).toBe(1);
  expect(r.catalog[0].name).toBe("temp");
  expect(r.timeCatalog.count).toBe(1);
  expect(r.blockTableRoot.length).toBe(1);

  const px = await r.getTilePixels("temp", 0, 0, 0, 0);
  expect(px).not.toBeNull();
  expect(px!.length).toBe(r.nPixels);
  expect(Math.abs(px![0])).toBeLessThan(0.5);
});

test("opens extended.wmt with 3 variables", async () => {
  const r = await new WMT(bytesFetcher(load("extended.wmt"))).open();
  expect(r.header.snapshotGeneration).toBe(2);
  expect(r.catalog.length).toBe(3);
  const names = r.catalog.map((v) => v.name).sort();
  expect(names).toEqual(["precip", "temp", "wind"]);

  for (const name of names) {
    const px = await r.getTilePixels(name, 0, 0, 0, 0);
    expect(px).not.toBeNull();
  }
});

test("getTilesInBlock returns one entry per coord", async () => {
  const r = await new WMT(bytesFetcher(load("compacted.wmt"))).open();
  const tiles = await r.getTilesInBlock("temp", 0, [
    { z: 0, x: 0, y: 0 },
    { z: 5, x: 9, y: 9 },
  ]);
  expect(tiles.length).toBe(2);
  expect(tiles[0]).not.toBeNull();
  expect(tiles[1]).not.toBeNull();
  expect(Number.isNaN(tiles[1]![0])).toBe(true);
});

test("crc_corrupted.wmt falls back to previous snapshot", async () => {
  const r = await new WMT(bytesFetcher(load("crc_corrupted.wmt"))).open();
  const names = r.catalog.map((v) => v.name).sort();
  expect(names).toContain("temp");
  expect(names).toContain("wind");
  expect(names).not.toContain("precip");
});
