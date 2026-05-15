# WMTiles

> A cloud-optimised, append-extensible single-file format for tiled,
> time-resolved weather data, plus the encoder, reader, library and viewer
> that go with it.

WMTiles takes a weather dataset (GRIB2 forecasts or HDF5 — ODIM_H5 radar
composites and CF-1.x/NetCDF4 files) and turns it into one `.wmt` file you can
drop on any static HTTP host. Browsers stream it tile-by-tile with HTTP Range
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
# CLI (Go ≥ 1.26; eccodes for GRIB2, libhdf5 for HDF5/ODIM_H5/NetCDF4)
sudo apt install libeccodes-dev libhdf5-dev   # or `brew install eccodes hdf5`
git clone https://github.com/hstin-de/wmtiles && cd wmtiles
make                               # builds the wmtiles binary with viewer

# Browser/Node library
npm install wmtiles fzstd
```

### Encode

```sh
# GRIB2 forecast (auto-detected by the GRIB magic)
wmtiles encode forecast.grib2 -o forecast.wmt \
    --min-zoom 0 --max-zoom 6 \
    --filter 2t,10u,10v \
    --precision 2t=0.05,10u=0.1,10v=0.1

# DWD ODIM_H5 radar composite (polar-stere is reprojected to lat-lon at parse time)
wmtiles encode 'composite_wn_*-hd5' -o radar.wmt --max-zoom 7

# CF-1.x / NetCDF4 file (regular lat-lon coords)
wmtiles encode model.nc4 -o model.wmt

# Weather-API mode: skip the Web-Mercator pyramid, store the source grid
# chunked in source coords. Bilinear point queries via Sample / sample();
# encodes ~50-100x faster, files shrink to ~GRIB×0.5..1.0.
wmtiles encode forecast.grib2 -o api.wmt --no-tiles
```

The input format is auto-detected by magic bytes (`GRIB` vs `\x89HDF`) with a
fallback to the file extension. Pass `--format grib2|hdf5` to override.

### Append a follow-up run

```sh
wmtiles extend forecast.wmt next-run.grib2     # GRIB2 source
wmtiles extend radar.wmt next-scan-hd5         # HDF5 source (auto-detected)
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
import { open } from "wmtiles";

const wmt = await open("/forecast.wmt");
console.log(wmt.variables);      // available variables
console.log(wmt.timeAxis);       // forecast steps

const t2m = wmt.variable("2t");
const px = await t2m.tile({ time: 12, z: 5, x: 16, y: 11 });
// Float32Array(256*256), NaN where the encoder marked NoData
```

For `--no-tiles` archives use the lat/lon sample API; the same range
coalescing keeps a batch of points down to a single byte-range request when
they fall in the same source-grid chunk neighbourhood:

```ts
const v = wmt.variable("2t_2m");
const tempBerlin = await v.sample({ time: 0, lat: 52.52, lon: 13.40 });

const cities = [
  { lat: 52.52, lon: 13.40 },  // Berlin
  { lat: 48.14, lon: 11.58 },  // Munich
  { lat: 53.55, lon:  9.99 },  // Hamburg
];
const values = await v.samples({ time: 0, points: cities });
// Float32Array(3) — NaN outside the source bbox.
```

For multi-tile fetches at the same `(variable, time)`, `tiles()`
coalesces 9 viewport tiles into 1 to 2 range requests:

```ts
const tiles = await t2m.tiles({
  time: 12,
  coords: [
    { z: 5, x: 16, y: 11 },
    { z: 5, x: 17, y: 11 },
    { z: 5, x: 18, y: 11 },
  ],
});
```

### Use from Go

The Go API has two packages: `decode` reads `.wmt` files and `encode` converts
source data (currently GRIB2) into `.wmt`. Lower-level subpackages (`reader`,
`encoder`, `format`, `codec`, ...) are available for tooling that needs direct
wire-format access.

Open a file and inspect the catalog:

```go
import "github.com/hstin-de/wmtiles/decode"

wmt, err := decode.Open("forecast.wmt")
if err != nil {
	panic(err)
}
defer wmt.Close()

vars := wmt.Variables()
times := wmt.Times()
bounds := wmt.Bounds()
```

Read one tile:

```go
pixels, err := wmt.ReadTile("2t", 12, decode.Coord(5, 16, 11))
```

For `--no-tiles` files use point sampling:

```go
v, err := wmt.Sample("2t", 12, 52.52, 13.40) // lat, lon
// Float32; NaN outside the source bbox.

values, err := wmt.Samples("2t", 12, []decode.SamplePoint{
    {Lat: 52.52, Lon: 13.40},
    {Lat: 48.14, Lon: 11.58},
})
```

Read a viewport worth of tiles with range coalescing:

```go
coords := []decode.TileCoord{
	decode.Coord(5, 16, 11),
	decode.Coord(5, 17, 11),
	decode.Coord(5, 18, 11),
}

tiles, err := wmt.ReadTiles("2t", 12, coords)
```

Reuse buffers in hot loops:

```go
pixels := wmt.NewTileBuffer()
err = wmt.ReadTileInto("2t", 12, decode.Coord(5, 16, 11), pixels)
```

Convert one or more source files to a fresh `.wmt`. GRIB2 (via ecCodes) and
HDF5 (ODIM_H5 radar composites and CF-1.x/NetCDF4 via libhdf5) are supported;
the API is format-neutral so additional readers can plug in alongside.

```go
import "github.com/hstin-de/wmtiles/encode"

enc, err := encode.NewEncoder("forecast.wmt", encode.Options{
	TileSize:        256,
	MinZoom:         0,
	MaxZoom:         5,
	FilterVariables: []string{"2t", "10u", "10v"},
	Precision: map[string]float64{
		"2t":  0.05,
		"10u": 0.1,
		"10v": 0.1,
	},
})

err = enc.AddFile("gfs-f000.grib2", encode.FormatGRIB2)
err = enc.AddFile("gfs-f001.grib2", encode.FormatGRIB2)
err = enc.AddBytes("extra.grib2", encode.FormatGRIB2, extraGRIB2)

// HDF5 inputs (ODIM_H5 or CF-1.x) use the same surface:
err = enc.AddFile("radar-composite-hd5", encode.FormatHDF5)

err = enc.Finish()
```

`encode.Encoder.Finish` scans all inputs together, builds one merged
variable/time catalog, and writes one fresh `.wmt`. It does not append/extend
once per input file.

### Raw arrays via `AddArray`

If the data is already in Go memory (custom reader, in-process model output,
test fixture, …), skip the parser and hand a `[]float32` to `AddArray`. The
grid is described by a `GridSpec` and the data layout is row-major:
`data[y*Nx + x]` is the sample at `(Lat0 + y*DY, Lon0 + x*DX)`. `DX` or `DY`
may be negative for flipped grids.

```go
import (
	"math"
	"time"
	"github.com/hstin-de/wmtiles/encode"
)

enc, _ := encode.NewEncoder("custom.wmt", encode.Options{
	TileSize: 256, MinZoom: 0, MaxZoom: 5,
	Precision: map[string]float64{"t2m": 0.05},
})

const nx, ny = 720, 361
values := make([]float32, nx*ny)
// fill values[y*nx + x] = sample at (Lat0 + y*DY, Lon0 + x*DX)

err = enc.AddArray(encode.ArrayInput{
	Variable:      "t2m",
	Unit:          "K",
	ReferenceTime: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
	Grid: encode.GridSpec{
		Nx: nx, Ny: ny,
		Lon0: -180, Lat0: -90,
		DX: 0.5, DY: 0.5,
		MissingValue: math.NaN(), // zero defaults to NaN
	},
	Data: values,
})

// Same Variable + same Grid across calls → one time series.
// Different Variable names → separate series in the same file.

err = enc.Finish()
```

`Appender.AddArray` has the same signature and lets you extend an existing
`.wmt` from in-memory data the same way.

For appending new variable/time blocks to an existing file the CLI's
`wmtiles extend` accepts both GRIB2 and HDF5 sources. Programs that need to
drive the streaming encoder or appender directly can use the lower-level
`encoder` subpackage; that path is intentionally outside the stable public
API.

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
wmtiles encode           <input> -o out.wmt …          convert GRIB2 or HDF5 → fresh .wmt (auto-detected)
wmtiles encode-grib      <input.grib2> -o out.wmt      force the GRIB2 encoder
wmtiles encode-hdf5      <input.h5|glob> -o out.wmt    force the HDF5 encoder (ODIM_H5 or CF/NetCDF4)
wmtiles extend           <file.wmt> <input>            append blocks for new (var, time) pairs (GRIB2 or HDF5)
wmtiles compact          <input.wmt> <output.wmt>      rewrite with snapshot in cold-start window
wmtiles snapshot-history <file.wmt>                    list active + previous snapshots
wmtiles inspect          <file.wmt>                    dump header + catalog + stats
wmtiles verify           <file.wmt>                    structural sanity + CRC validation
wmtiles compare          <input> <file.wmt> …          pixel-by-pixel fidelity vs. source (GRIB2 or HDF5)
wmtiles serve            <file.wmt> [--addr :8080]     bundled web viewer
```

`encode` flags:

| Flag | Default | Meaning |
|---|---|---|
| `-o PATH` | (required) | output `.wmt` path |
| `--format FMT` | auto-detect | `grib2` or `hdf5`; overrides the magic-byte/extension sniff |
| `--min-zoom N` | `0` | minimum zoom level |
| `--max-zoom N` | `5` | maximum zoom level |
| `--tile-size-log2 N` | `8` (256 px) | tile pixel size, allowed `7..10` (128..1024) |
| `--filter SHORTNAMES` | (none = all) | comma-separated shortNames to keep (GRIB shortName, ODIM quantity, or CF mapping) |
| `--precision NAME=K,…` | shortName/unit lookup, then 10-bit auto-cap | quantisation precision overrides; `=0` forces full-range u16 |
| `--no-tiles` | off | skip the Web-Mercator pyramid; store source-grid chunks for point-query (lat/lon) API use. Output is not viewable on a slippy map without on-the-fly tiling |
| `--raw-chunk-size-log2 N` | `8` (256 px) | source-pixel side of one raw-grid chunk as log2 (4..12 → 16..4096). Only consulted with `--no-tiles` |

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
encode/        source-data conversion API (GRIB2 now, other formats later)
decode/        WMTiles reading API namespace
reader/        cold-start, LRU, per-block decode
parser/        GRIB2 parser bindings (cgo + eccodes)
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
make typecheck   # typecheck both TS packages
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
