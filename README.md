# WMTiles

> A cloud-optimised, append-extensible single-file format for tiled,
> time-resolved weather data, plus the encoder, reader, library and viewer
> that go with it.

WMTiles takes a GRIB2 forecast and turns it into one `.wmt` file you can drop
on any static HTTP host. Browsers stream it tile-by-tile with HTTP Range
requests, with no tile server, no database, no pre-rendering. On local/POSIX
storage, new forecast hours or variables are appended in place; readers either
see the previous snapshot or the new one after a crash or torn write.

Borrows what works from [PMTiles](https://protomaps.com/): Hilbert tile IDs,
root+leaf directories, varint columns, range coalescing, and rebuilds
everything else around the things weather data actually needs: many
variables, an unbounded time axis, value tiles (not pre-rendered RGB),
per-block quantisation, append safety.

---

## Why

Conventional tile servers (XYZ, WMS, MVT) render rasters before serving
them. That's fine for basemaps; it's wasteful for scientific data, where the
client wants **values**, not a pre-coloured PNG. But the alternative,
shipping NetCDF or GRIB2 to the browser, falls over on cold-start latency,
random access, and the absence of a sane multi-axis index.

WMTiles is the in-between point:

- **Values, not pixels.** Each tile is a Float32 raster. The browser picks
  the colour ramp, the contour level, the masking rule. Switch palettes
  without re-fetching a single byte.
- **Single file, plain HTTP.** A static server with byte-range support is
  the entire backend. S3, R2, a CDN, `python -m http.server`, all work.
- **Cold-start in 1 to 2 round trips.** Header + active snapshot are pinned to
  the first 64 KiB of the file. One `Range: bytes=0-65535` request gets the
  whole catalog.
- **Append, never rewrite blocks.** New forecast hours, new variables, whole
  new model runs are concatenated to the end. Existing block bytes are
  untouched; publishing the new state is a small header rewrite.
- **Crash-safe by construction.** Header CRC + magic tail detect torn
  local writes; readers automatically fall back to the previous snapshot. No
  fixup script, no `fsck`.
- **Lossless or near-lossless.** Quantisation parameters live per **block**
  (one block per (variable, time)), so a heat wave next month doesn't
  invalidate January's encoding. Pick `precision=0.1 K` for a fixed error
  budget; the encoder uses precision as the actual quantisation step, so any
  high bit-planes left over by a coarse precision stay empty and bitshuffle
  + zstd collapses them to almost nothing. If a positive precision cannot
  fit in u16, the encoder falls back to f32. `precision=0` forces full-range
  u16 quantisation across the observed block range.

---

## Quick start

### Install

```sh
# CLI (Go ≥ 1.26, eccodes for GRIB2 parsing)
sudo apt install libeccodes-dev   # or `brew install eccodes`
git clone https://github.com/hstin-de/wmtiles && cd wmtiles
make                               # builds the wmtiles binary with viewer

# Browser/Node library
npm install wmtiles fzstd
```

### Encode

```sh
wmtiles encode forecast.grib2 -o forecast.wmt \
    --min-zoom 0 --max-zoom 6 \
    --filter 2t,10u,10v \
    --precision 2t=0.05,10u=0.1,10v=0.1
```

### Append a follow-up run

```sh
wmtiles extend forecast.wmt next-run.grib2
```

### Inspect, verify, compact

```sh
wmtiles inspect  forecast.wmt          # header + catalog + stats
wmtiles verify   forecast.wmt          # CRCs, structural sanity
wmtiles compact  forecast.wmt out.wmt  # 1-RT cold-start again
wmtiles compare  forecast.grib2 forecast.wmt   # pixel-level fidelity
```

### Serve & view

```sh
wmtiles serve forecast.wmt --addr :8080
```

Opens an embedded Leaflet viewer at `http://localhost:8080/`. The browser
pulls byte ranges directly from the same `.wmt`; there's no rendering
backend. The viewer is a Bun-bundled IIFE compiled into the Go binary via
`go:embed`.

### Read from JavaScript

```ts
import { WMT, httpRangeFetcher } from "wmtiles";

const r = await new WMT(httpRangeFetcher("/forecast.wmt")).open();
console.log(r.catalog);          // available variables
console.log(r.timeCatalog);      // forecast steps

const px = await r.getTilePixels("2t", /*timeStep*/ 12, /*z*/ 5, 16, 11);
// Float32Array(256*256), NaN where the encoder marked NoData
```

For multi-tile fetches at the same `(variable, time)`, `getTilesInBlock`
coalesces 9 viewport tiles into 1 to 2 range requests:

```ts
const tiles = await r.getTilesInBlock("2t", 12, [
  { z: 5, x: 16, y: 11 },
  { z: 5, x: 17, y: 11 },
  { z: 5, x: 18, y: 11 },
]);
```

### Read from Go

```go
import "github.com/hstin-de/wmtiles/reader"

r, err := reader.Open("forecast.wmt")
defer r.Close()

pixels := make([]float32, r.PixelCount())
err = r.ReadTile("2t", /*timeStep*/ 12, /*z*/ 5, 16, 11, pixels)
```

---

## File anatomy

The byte-level wire format (every offset, magic, CRC, codec tag) is
specified in [FORMAT.md](FORMAT.md). What follows is the high-level shape.

```
+-----------+-------------------+---------------------+----------------+
|           | Initial snapshot  | Initial blocks      | Append zone …  |
| Header    |  (catalog +       |   Block₀ Block₁ …   |  Blockₙ … +    |
| 256 B     |   block table)    |                     |  new snapshot  |
+-----------+-------------------+---------------------+----------------+
   0       256                       …                       …      [trailer]
```

| Region | Mutable? | Contents |
|---|---|---|
| **Header** (256 B at offset 0) | yes: atomic 256-B swap | magic, version, CRC, pointer to active snapshot, generation, bbox, zoom range, tile size |
| **Snapshot** | no: append-only, multiple may coexist | variable catalog, time catalog, block table (root + optional leaves), metadata JSON, trailer with CRC |
| **Block** | no | self-contained tile pyramid for one `(variable, time)`: header, root directory, optional leaves, packed tile data |
| **File trailer** (16 B) | no | logical-end marker for verification |

A **block** is the unit of independence. It carries its own quantisation
parameters (`scale`, `offset`, `dtype`, `vmin`, `vmax`) and its own tile
directory. Blocks never reference each other and can be reordered by
`compact` without touching their bytes.

A **snapshot** is a self-contained logical view. Appending writes a fresh
snapshot at the end of the file and atomically retargets the header to it.
The previous snapshot stays in place as a fallback for crash recovery.

### Tile addressing

3D Hilbert TileIDs (PMTiles-compatible numbering):

$$
\mathrm{TileID}(z, x, y) = \tfrac{4^{z} - 1}{3} + h_z(x, y)
$$

Verification vector: `TileID(12, 3423, 1763) = 19078479`.

Hilbert ordering keeps spatially-adjacent tiles close in TileID space; that
becomes byte locality in the block, which becomes a single coalesced range
request when the viewport repaints.

### Quantisation, per block

Each block picks `(scale, offset, dtype)` from its observed value range:

- `dtype = u8` if `(vmax minus vmin)/precision + 1 ≤ 255`
- `dtype = u16` if the same fits in 65 535 steps
- `dtype = f32` for the lossless path

`scale` is the requested precision exactly, not `range/MaxQ`. When the
precision is coarser than the dtype's full grid (e.g. 0.125 K of swing in a
u16), the high bit-planes are zero on every sample. Bitshuffle transposes
those into all-zero rows that zstd encodes in a handful of bytes. Most of
the recent file-size win lives in this interaction. The top sentinel value
(`0xFF` / `0xFFFF` / quiet-NaN) is reserved for **NoData**.

Variables without an explicit precision (neither `--precision` nor a
shortName/unit lookup) get a 10-bit auto-cap on the observed range
(`range / 1024`), well above NWP-grade SNR.

### Per-tile codecs

| ID | Codec | Use |
|---|---|---|
| `0x01` | constant | block-of-equal-values, 5 bytes total (tag + 4-byte value) |
| `0x02` | raw + zstd | row-major dump, zstd compressed |
| `0x03` | bitshuffle + zstd | transpose then zstd, typically 25 to 40 % of source for Float32 fields |
| `0x04` | spatial 2D-delta + zstd | smooth fields (geopotential, temperature gradients) |
| `0x05` | Lorenzo predictor + zstd | 2D Lorenzo predictor in quantised space, then zstd; wins on smooth fields at ~3× the CPU of bitshuffle alone |

Codec is chosen per block by a small bandit: sample bitshuffle vs. delta
vs. lorenzo on the first few tiles, commit to the cheapest output for the
next ~1000 tiles, then re-sample. Constant tiles are detected and dedup'd
before encoding; identical tile contents share one blob within a block.

### Atomic append

```
1. Append new tile blobs at file end.
2. Append new block headers + directories.
3. Append new snapshot (full, not diff).
4. fsync(fd).
5. Build new 256-B header (active offset, generation+1, fresh CRC).
6. pwrite(fd, header, 0, 256).   ← small publish write; CRC/tail reject tears
7. fsync(fd).
```

Crash before step 6 → file in old state, append discarded. Crash mid-step 6
→ header CRC fails → reader falls back to `previous_snapshot_offset`. Crash
after step 7 → done. Object-store-friendly append is still an open design
topic; today this flow targets local filesystems with random writes.

---

## CLI reference

```
wmtiles encode           <input.grib2> -o out.wmt …    convert GRIB2 → fresh .wmt
wmtiles extend           <file.wmt> <input.grib2>      append blocks for new (var, time) pairs
wmtiles compact          <input.wmt> <output.wmt>      rewrite with snapshot in cold-start window
wmtiles snapshot-history <file.wmt>                    list active + previous snapshots
wmtiles inspect          <file.wmt>                    dump header + catalog + stats
wmtiles verify           <file.wmt>                    structural sanity + CRC validation
wmtiles compare          <input.grib2> <file.wmt> …    pixel-by-pixel fidelity vs. source GRIB
wmtiles serve            <file.wmt> [--addr :8080]     bundled web viewer
```

`encode` flags:

| Flag | Default | Meaning |
|---|---|---|
| `-o PATH` | (required) | output `.wmt` path |
| `--min-zoom N` | `0` | minimum zoom level |
| `--max-zoom N` | `5` | maximum zoom level |
| `--tile-size-log2 N` | `8` (256 px) | tile pixel size, allowed `7..10` (128..1024) |
| `--filter SHORTNAMES` | (none = all) | comma-separated GRIB shortNames to keep |
| `--precision NAME=K,…` | shortName/unit lookup, then 10-bit auto-cap | quantisation precision overrides; `=0` forces full-range u16 |

---

## Performance

These are design-target numbers, not benchmark guarantees.

**Cold start** (browser, 100 ms RTT, 50 MB/s):

| Scenario | Round trips | Time-to-first-tile |
|---|---|---|
| Initial encode or post-`compact` | 1 RT (header+snapshot) + 1 RT (tiles) | ~470 ms |
| After 50 appends, no compact | 2 RT (snapshot outside cold-start window) + 1 RT (tiles) | ~580 ms |

**Encoder throughput** (wall clock, 16 workers): ~800 tiles/s. A typical
GFS forecast (5 vars × 168 h × 5461 tiles per block, `z ≤ 6`) takes
~96 minutes to encode, ~14 minutes to extend by another 6-hour run.

**File sizes.** The bit-plane fix to quantisation, the Lorenzo predictor,
and the precision-table tightening (e.g. 0.5 K → 0.125 K for temperature)
have together cut typical block sizes by ~30 to 40 % vs. the first
release. Two ground-truth points from the current encoder:

| Source | Variables × times | Zoom | Source GRIB | `.wmt` | Per-block |
|---|---|---|---|---|---|
| ICON-D2 (regional, 2 km) | 1 × 49 h | `z ≤ 10` | 76 MB | 1.79 GB | ~37 MB |
| GFS 0.25° (one full run) | ~700 × 1 h | `z ≤ 4` | 486 MB | 2.20 GB | ~3.2 MB |

Extrapolated to typical archive scenarios at GFS 0.25°, `z ≤ 6`:

| Scenario | Blocks | Snapshot | Total |
|---|---|---|---|
| 1 run, 5 variables, 168 h | 840 | ~45 KB | ~30 GB |
| Daily archive, 30 days | ~25 000 | ~1.2 MB | ~900 GB |
| 5-year archive | ~1.5 M | ~75 MB | ~55 TB |

The snapshot stays under 16 MB up to ~3 M blocks. Beyond that, block-table
hierarchisation (root + leaves, like the per-block tile directory) keeps
cold-start in two range requests.

---

## Repository layout

```
format/        on-disk layout: header, snapshot, block, block-table, file trailer
tileid/        3D Hilbert TileIDs
directory/     per-block tile directories (varint columns, +1/0 offset trick)
quantize/      u8 / u16 / lossless f32 with NaN sentinels
codec/         per-tile codec registry (constant, raw_zstd, bitshuffle, delta)
bitshuffle/    bit transpose
varint/        PMTiles-style varints
encoder/       streaming encoder + atomic header swap + append API
reader/        cold-start, LRU, per-block decode
parser/        GRIB2 ingest (cgo + eccodes)
tiler/         GRIB grid → Web-Mercator tile sampler
cmd/wmtiles/   CLI: encode, extend, compact, inspect, verify, compare, serve
cmd/wmtiles/web/   Bun-bundled HTML viewer, embedded into the binary
cmd/gen-testdata/  deterministic test-fixture generator (format/testdata/*.wmt)
wmtiles-js/    pure-TypeScript reader (browser, Node, Bun, Cloudflare Workers)
```

---

## Building from source

System dependencies: Go ≥ 1.26, [Bun](https://bun.sh) for the TypeScript
build, and **eccodes** (the ECMWF GRIB2 library):

```sh
sudo apt install libeccodes-dev    # Debian/Ubuntu
brew install eccodes               # macOS
```

Then:

```sh
make             # build the CLI binary with the embedded viewer
make test        # go test -race ./...  +  bun test
make typecheck   # tsc --noEmit on both TS packages
make clean       # remove generated artifacts
```

`make` orchestrates: `bun install` → `bun build` (viewer bundle) →
`go build -tags embed`. `make test` regenerates deterministic format
fixtures before running the Go and TypeScript tests. `make lib` builds the
publishable TypeScript `dist/` artifacts.

The Go module is buildable **without** Bun: the default build (`go build
./cmd/wmtiles/`) uses `embed_stub.go` so the CLI works without the viewer.
The `embed` build tag activates `embed.go`, which `go:embed`s the Bun
output. CI exercises both paths.

---

## Stability vectors

Format compatibility is pinned by deterministic fixtures regenerated on
every CI run:

- `format/testdata/minimal.wmt`: 1 variable, 1 time, 1 tile at `z=0`.
- `format/testdata/extended.wmt`: same after two appends.
- `format/testdata/compacted.wmt`: same after `compact`.
- `format/testdata/crc_corrupted.wmt`: header-CRC torn; reader must
  recover via `previous_snapshot`.

Any third-party implementation that produces matching bytes for these
inputs is wire-compatible.

---

## Status

The format version is `1`. The CLI ships `encode`, `extend`, `compact`,
`inspect`, `verify`, `compare`, `serve`. The Go reader and the TypeScript
reader are at parity for the read path. The encoder is Go-only.

Open design questions: multi channel tiles (e.g. wind u/v together), an
explicit vertical level axis, live update polling for long running readers,
and an S3 friendly append model that doesn't rely on random writes.

---

## License

MIT. See [`wmtiles-js/README.md`](wmtiles-js/README.md) for the npm package.
