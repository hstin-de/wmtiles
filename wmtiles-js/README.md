# wmtiles

Pure-TypeScript reader for the [WMTiles](https://github.com/hstin-de/wmtiles)
single-file format. Parses headers, snapshots, block tables; decodes per-tile
codecs; dequantizes per-block. No DOM dependency: works in browsers, Node,
Bun, Cloudflare Workers, anywhere with `fetch`.

## Install

```sh
npm install wmtiles fzstd
# or
bun add wmtiles fzstd
```

## Usage

### Browser / HTTP-served file

```ts
import { WMT, httpRangeFetcher } from "wmtiles";

const r = await new WMT(httpRangeFetcher("/data.wmt")).open();
// r.catalog: variable list
// r.timeCatalog: time-step axis
// r.header: bbox, zoom range, generation, …

const pixels = await r.getTilePixels("temperature_2m", 12, 5, 16, 11);
// Float32Array of r.nPixels values, NaN where the encoder marked NoData.
```

### Node / Bun / from a local file

```ts
import { readFileSync } from "node:fs";
import { WMT, bytesFetcher } from "wmtiles";

const r = await new WMT(
  bytesFetcher(new Uint8Array(readFileSync("./data.wmt")))
).open();
```

### Custom byte source

Implement the `RangeFetcher` interface:

```ts
import type { RangeFetcher } from "wmtiles";

const s3Fetcher: RangeFetcher = {
  async fetchRange(offset, length) {
    const resp = await s3Client.send(new GetObjectCommand({
      Bucket: "wx",
      Key: "data.wmt",
      Range: `bytes=${offset}-${offset + length - 1}`,
    }));
    return new Uint8Array(await resp.Body!.transformToByteArray());
  },
};

const r = await new WMT(s3Fetcher).open();
```

### Coalesced multi-tile fetch

For UIs that paint several tiles in one frame, `getTilesInBlock` issues 1 to 2
range requests (instead of one per tile) when all tiles share the same
`(variable, time)` block:

```ts
const tiles = await r.getTilesInBlock("temperature_2m", 12, [
  { z: 5, x: 16, y: 11 },
  { z: 5, x: 17, y: 11 },
  { z: 5, x: 18, y: 11 },
]);
// tiles[i] is a Float32Array, or NaN-filled if that coord is out of range
// or missing from the file.
```

## API surface

The library exports three layers: pick what fits:

| Layer | What |
|---|---|
| `WMT`, `httpRangeFetcher`, `bytesFetcher` | High-level reader. Use this. |
| `parseHeader`, `parseSnapshotHeader`, `parseBlockTable`, … | Raw parsers if you want to build your own caching/streaming. |
| `decodeCodec`, `dequantize`, `encode3D` | Tile-level primitives if you fetch blobs yourself. |

See `src/index.ts` for the full export list.

This library is a faithful port of the Go reader in `reader/reader.go`.

## License

MIT.
