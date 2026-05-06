import { test, expect } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  CODEC_BITSHUFFLE_ZSTD,
  CODEC_LORENZO_ZSTD,
  decodeCodec,
} from "./decoder";
import { DTYPE_U8, DTYPE_U16 } from "./format";

// fixture: regenerate with `go run ./cmd/wmtiles-js-fixtures wmtiles-js/src/__fixtures__/lorenzo.json`
const FIXTURE = JSON.parse(
  readFileSync(resolve(__dirname, "__fixtures__/lorenzo.json"), "utf8"),
) as {
  w: number;
  u8: { src: string; blob: string };
  u16: { src: string; blob: string };
};

function b64(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, "base64"));
}

test("lorenzo_zstd round-trip u8 against Go-encoded fixture", () => {
  const src = b64(FIXTURE.u8.src);
  const blob = b64(FIXTURE.u8.blob);
  expect(blob[0]).toBe(CODEC_LORENZO_ZSTD);
  const out = decodeCodec(blob, DTYPE_U8, FIXTURE.w * FIXTURE.w);
  expect(out).toEqual(src);
});

test("lorenzo_zstd round-trip u16 against Go-encoded fixture", () => {
  const src = b64(FIXTURE.u16.src);
  const blob = b64(FIXTURE.u16.blob);
  expect(blob[0]).toBe(CODEC_LORENZO_ZSTD);
  const out = decodeCodec(blob, DTYPE_U16, FIXTURE.w * FIXTURE.w);
  expect(out).toEqual(src);
});

test("unknown codec raises", () => {
  const blob = new Uint8Array([0xfe, 1, 2, 3]);
  expect(() => decodeCodec(blob, DTYPE_U8, 16)).toThrow();
});

test("lorenzo and bitshuffle constants are distinct", () => {
  expect(CODEC_LORENZO_ZSTD).toBe(0x05);
  expect(CODEC_BITSHUFFLE_ZSTD).toBe(0x03);
});
