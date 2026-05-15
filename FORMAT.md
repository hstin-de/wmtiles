# WMTiles Format

This document specifies the WMTiles `.wmt` file format as implemented by this
repository. It is intentionally byte-level: a reader should be able to open a
file by following this document without depending on the Go implementation.

WMTiles is a single-file, range-request-friendly container for tiled weather
rasters. The important concepts are:

- A **header** at byte offset `0` points to the active snapshot.
- A **snapshot** is an immutable catalog view: variables, time axis, metadata,
  and a block table.
- A **block** is the independent storage unit for one `(variable, time)` pair.
- A **tile blob** is one encoded raster tile inside a block.
- Appending writes new blocks and a new snapshot, then publishes that snapshot by
  rewriting only the fixed-size header.

All integer and floating-point fields are little-endian unless this document says
otherwise. Fixed-width floating-point values are IEEE-754 `float64` or `float32`.
Variable-length integers are unsigned LEB128 `uint64` values, at most 10 bytes.
Strings are UTF-8 byte strings with a one-byte length prefix, so catalog strings
are limited to 255 bytes.

## Versions And Constants

| Name | Value | Meaning |
|---|---:|---|
| File magic | `57 4D 54 49 4C 45 53 00` | ASCII `WMTILES\0` |
| Format version | `1` | Header-level wire format |
| Header size | `256` bytes | Fixed at file offset `0` |
| Header tail magic | `0xE7E7DEAD` | Torn-write sentinel at header bytes `252..255` |
| Cold-start budget | `65536` bytes | Header plus small active snapshots fit in one range request |
| Snapshot schema version | `1` | Snapshot-level schema |
| Snapshot header size | `128` bytes | Fixed size |
| Snapshot trailer size | `16` bytes | Fixed size |
| Snapshot trailer magic | `0xC0FFEE42` | Trailer sentinel |
| Block format version | `2` | Block-level wire format |
| Block header size | `64` bytes | Fixed size |
| Block magic | `0xB10CC0DE` | Block header sentinel |
| Max block root bytes | `16384 - 64` | Compressed root directory limit (tile blocks); first-RTT prefetch budget |
| Max raw-grid section bytes | `4 << 20` | Compressed raw-grid section limit; exceeds the prefetch budget but the reader fallback handles it with one extra range request |
| Max block-table root bytes | `16384` | Compressed block-table root limit |
| File trailer size | `16` bytes | Fixed size |
| File trailer magic | `0xEEEFFFFF` | Logical-end sentinel |

CRC fields use CRC32C/Castagnoli.

## File Layout

The header is always at offset `0`. Everything else is located by offsets stored
in the header, snapshot header, block table, and block headers.

Fresh files normally reserve the first 64 KiB so a small snapshot can be written
right after the header:

```text
0                         256                     65536
+-------------------------+-----------------------+-------------------+
| Header, 256 B           | Snapshot, if it fits  | Blocks ...        |
+-------------------------+-----------------------+-------------------+
                                                                  ... trailer
```

If the snapshot does not fit in the cold-start window, blocks begin at `65536`
and the snapshot is appended after the blocks. The header's
`active_snapshot_offset` and `active_snapshot_length` are always authoritative.

Append creates this shape:

```text
old logical end - 16
        |
        v
+-------+------------------+----------------+----------------+----------+
| old   | new blocks       | new snapshot   | file trailer   | optional |
| file  |                  |                |                | slack    |
+-------+------------------+----------------+----------------+----------+
                                ^
                                |
                         header is rewritten to point here
```

The old file trailer is overwritten. Bytes after `file_logical_end` are not part
of the logical file and may be ignored by readers and validators.

## Header

The header is exactly 256 bytes at file offset `0`.

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 8 | bytes | Magic `WMTILES\0` |
| `8` | 2 | `u16` | `format_version`, currently `1` |
| `10` | 2 | `u16` | `flags` |
| `12` | 4 | `u32` | `header_crc32c` |
| `16` | 8 | `u64` | `active_snapshot_offset`, absolute file offset |
| `24` | 8 | `u64` | `active_snapshot_length` |
| `32` | 8 | `u64` | `previous_snapshot_offset`, absolute file offset |
| `40` | 8 | `u64` | `previous_snapshot_length` |
| `48` | 8 | `u64` | `file_logical_end`, byte offset just after the file trailer |
| `56` | 8 | `u64` | `snapshot_generation` |
| `64` | 1 | `u8` | `internal_compression` |
| `65` | 1 | `u8` | `tile_pixel_size_log2`; tile width/height is `1 << value` |
| `66` | 1 | `u8` | `min_zoom` |
| `67` | 1 | `u8` | `max_zoom` |
| `68` | 4 | `i32` | `bbox_lon_min_e7`, degrees times `1e7` |
| `72` | 4 | `i32` | `bbox_lat_min_e7`, degrees times `1e7` |
| `76` | 4 | `i32` | `bbox_lon_max_e7`, degrees times `1e7` |
| `80` | 4 | `i32` | `bbox_lat_max_e7`, degrees times `1e7` |
| `84` | 168 | bytes | Reserved, written as zero, ignored by readers |
| `252` | 4 | `u32` | Header tail magic `0xE7E7DEAD` |

The header CRC covers bytes `16..251`. It excludes the magic, version, flags,
CRC field, and tail magic. A reader can still inspect the parsed previous
snapshot pointer when the CRC fails; that is how crash fallback works.

### Header Flags

| Bit | Name | Meaning |
|---:|---|---|
| `0` | `cold_start_in_window` | Active snapshot lies within bytes `0..65535` |
| `1` | `has_previous_snapshot` | Previous snapshot fields are usable for fallback |
| `2` | `time_catalog_regular` | Time catalog uses the regular 20-byte layout |

Other bits are reserved. Writers should set them to zero.

### Internal Compression

`internal_compression` is used for catalogs, block tables, metadata, and
directories. Tile payload codecs are separate and are described later.

| ID | Compression |
|---:|---|
| `0` | none |
| `1` | gzip |
| `2` | zstd |

Regular time catalogs are not internally compressed, even when the file's
internal compression is zstd or gzip. Irregular time catalogs are compressed.

## Snapshot

A snapshot is a self-contained logical view. Its offsets are relative to the
start of the snapshot, not absolute file offsets.

```text
+----------------------+------------------+--------------+-------------+
| Snapshot header      | variable catalog | time catalog | block table |
| 128 B                |                  |              | root        |
+----------------------+------------------+--------------+-------------+
| optional block-table leaves | metadata JSON | snapshot trailer, 16 B |
+-----------------------------+---------------+------------------------+
```

The header points to the active snapshot with an absolute file offset and length.
Older snapshots may remain in the file, but only `previous_snapshot_*` is part of
the header fallback contract.

### Snapshot Header

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 8 | `u64` | `schema_version`, currently `1` |
| `8` | 8 | `u64` | `snapshot_generation` |
| `16` | 8 | `i64` | `creation_time_ms`, Unix milliseconds |
| `24` | 8 | `i64` | `reference_time_ms`, Unix milliseconds; `0` if unset |
| `32` | 2 | `u16` | `num_variables` |
| `34` | 4 | `u32` | `num_time_steps` |
| `38` | 2 | bytes | Reserved padding, written as zero |
| `40` | 8 | `u64` | `num_blocks` |
| `48` | 8 | `u64` | `variable_catalog_off` |
| `56` | 8 | `u64` | `variable_catalog_len` |
| `64` | 8 | `u64` | `time_catalog_off` |
| `72` | 8 | `u64` | `time_catalog_len` |
| `80` | 8 | `u64` | `block_table_root_off` |
| `88` | 8 | `u64` | `block_table_root_len` |
| `96` | 8 | `u64` | `block_table_leaves_off` |
| `104` | 8 | `u64` | `block_table_leaves_len` |
| `112` | 8 | `u64` | `metadata_off` |
| `120` | 8 | `u64` | `metadata_len` |

All section offsets and lengths refer to the on-disk bytes. For compressed
sections, the length is the compressed length.

### Snapshot Trailer

The snapshot trailer is the last 16 bytes of the snapshot.

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 4 | `u32` | Snapshot trailer magic `0xC0FFEE42` |
| `4` | 8 | `u64` | `snapshot_total_length`, including this trailer |
| `12` | 4 | `u32` | CRC32C of all snapshot bytes before the trailer |

A reader should reject a snapshot if the magic, total length, or CRC does not
match. If the active snapshot is bad and the header has a previous snapshot,
readers fall back to the previous snapshot.

## Variable Catalog

The variable catalog section is internally compressed. After decompression it is
a sequence of `num_variables` entries sorted by `variable_id`.

Each entry:

| Field | Type | Meaning |
|---|---|---|
| `variable_id` | `u16` | Stable numeric ID used by block-table keys |
| `name_len` | `u8` | Byte length of `name` |
| `name` | bytes | UTF-8 variable name, often GRIB shortName |
| `unit_len` | `u8` | Byte length of `unit` |
| `unit` | bytes | UTF-8 unit string |
| `default_dtype` | `u8` | Default storage dtype hint |
| `default_codec` | `u8` | Default tile codec hint |
| `default_precision_hint` | `f64` | Precision hint used when appending/inspecting |
| `colormap_len` | `u8` | Byte length of `colormap_hint` |
| `colormap_hint` | bytes | UTF-8 display hint |
| `value_min_observed_global` | `f64` | Global observed finite minimum, may be NaN |
| `value_max_observed_global` | `f64` | Global observed finite maximum, may be NaN |

The authoritative dtype, codec, scale, offset, and NoData value for a tile come
from that tile's block-table entry, not from the variable catalog defaults.

## Time Catalog

The time catalog maps `time_id` values to Unix millisecond timestamps.

If the header flag `time_catalog_regular` is set, the time catalog section is
exactly 20 uncompressed bytes:

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 8 | `i64` | `start_ms` |
| `8` | 8 | `i64` | `interval_ms` |
| `16` | 4 | `u32` | `count` |

The timestamp for `time_id = t` is:

```text
start_ms + t * interval_ms
```

If the flag is not set, the section is internally compressed. After
decompression:

| Field | Type | Meaning |
|---|---|---|
| `count` | `u32` | Number of timestamps |
| `first_timestamp` | zigzag LEB128 `i64` | Absolute Unix milliseconds, omitted if count is zero |
| `delta_1..delta_n` | zigzag LEB128 `i64` | Delta from the previous timestamp |

The irregular timestamp list should be chronological. The current appender
rejects out-of-order new timestamps.

Zigzag encoding maps signed `i64` to unsigned varints:

```text
zigzag(v)   = (v << 1) ^ (v >> 63)
unzigzag(u) = (u >> 1) ^ -(u & 1)
```

## Block Table

The block table maps `(variable_id, time_id)` to a block. It can be stored as a
single root table or as a root table whose entries point to leaf tables.

The root table section is internally compressed. Each leaf table is compressed
individually and concatenated in the snapshot's block-table leaves section.

After decompression, a block table is columnar:

```text
count
composite_key_deltas[count]
leaf_flags[count]
block_offsets[count]
block_lengths[count]
dtypes[count]
codecs[count]
scales[count]
offsets[count]
nodata[count]
value_mins[count]
value_maxs[count]
num_addressed_tiles[count]
num_directory_entries[count]
num_tile_contents[count]
```

### Block Table Columns

| Column | Encoding | Meaning |
|---|---|---|
| `count` | LEB128 `u64` | Number of entries |
| `composite_key_deltas` | LEB128 `u64` each | Delta-coded sorted keys |
| `leaf_flags` | `u8` each | `1` means entry points to a block-table leaf |
| `block_offsets` | LEB128 `u64` each | Absolute block offset, or leaf offset |
| `block_lengths` | LEB128 `u64` each | Block length, or compressed leaf length |
| `dtypes` | `u8` each | Quantized storage dtype |
| `codecs` | `u8` each | Default codec used for this block |
| `scales` | `f64` each | Dequantization scale |
| `offsets` | `f64` each | Dequantization offset |
| `nodata` | `u32` each | Encoded NoData sentinel |
| `value_mins` | `f64` each | Observed finite min for this block |
| `value_maxs` | `f64` each | Observed finite max for this block |
| `num_addressed_tiles` | LEB128 `u64` each | Number of tile addresses stored |
| `num_directory_entries` | LEB128 `u64` each | Number of directory rows before hierarchy partitioning |
| `num_tile_contents` | LEB128 `u64` each | Number of unique tile blobs after deduplication |

The composite key is:

```text
composite_key = (uint64(variable_id) << 32) | uint64(time_id)
```

Keys are sorted ascending and stored as deltas from the previous key. The first
delta is from zero.

For normal entries, `block_offset` is an absolute file offset and `block_length`
is the full byte length of the block. For leaf-pointer entries, `block_offset`
is relative to the snapshot's `block_table_leaves_off` and `block_length` is
the compressed leaf-table length. Other columns are present on disk but ignored.

Lookup algorithm:

1. Binary-search the root table for the target composite key.
2. If there is an exact non-leaf match, use it.
3. If there is an exact leaf pointer, or the previous entry is a leaf pointer,
   load that leaf and search it.
4. If the leaf has no exact non-leaf match, the block is absent.

Root leaf pointers use the first key contained in the leaf as their own key.

## Blocks

A block stores all tile blobs for one `(variable_id, time_id)` pair. Block
offsets in the block table are absolute file offsets. Offsets inside a block
header are relative to the start of that block.

```text
+--------------+--------------+--------------------+-----------+----------+
| Block header | root dir     | optional leaf dirs | tile data | optional |
| 64 B         | compressed   | compressed leaves  | blobs     | dict     |
+--------------+--------------+--------------------+-----------+----------+
```

### Block Header

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 4 | `u32` | Block magic `0xB10CC0DE` |
| `4` | 2 | `u16` | `block_format_version`, currently `2` |
| `6` | 2 | `u16` | `block_flags` |
| `8` | 8 | `u64` | `root_directory_offset`, block-relative |
| `16` | 4 | `u32` | `root_directory_length`, compressed length |
| `20` | 4 | `u32` | `dict_length`, compressed-dictionary byte length; `0` if absent |
| `24` | 8 | `u64` | `leaf_directories_offset`, block-relative; `0` if absent |
| `32` | 8 | `u64` | `leaf_directories_length` |
| `40` | 8 | `u64` | `tile_data_offset`, block-relative |
| `48` | 8 | `u64` | `tile_data_length` |
| `56` | 4 | `u32` | `num_addressed_tiles` |
| `60` | 4 | `u32` | `num_directory_entries` |

Block flag bits:

| Bit | Name | Meaning |
|---:|---|---|
| `0` | `has_leaf_directories` | The block has leaf directories |
| `1` | `has_dict` | The block carries a per-block zstd dictionary |
| `2` | `raw_grid` | Block stores a native source-grid raster instead of a Hilbert tile pyramid; see "Raw-Grid Blocks" below |

Other bits are reserved.

### Per-Block Zstd Dictionary

When `has_dict` is set, `dict_length` dictionary bytes are stored at the tail
of the block, immediately after `tile_data`. Absolute file offset is
`block_offset + tile_data_offset + tile_data_length`. The dictionary applies
to every zstd-based tile codec (`raw_zstd`, `bitshuffle_zstd`, `delta_zstd`,
`lorenzo_zstd`); only the inner zstd payload is dict-encoded — codec tags are
unchanged. The constant codec (`0x01`) is unaffected.

The dictionary may be a trained zstd dict (magic `0xEC30A437`) or a raw-content
prefix. libzstd's `ZSTD_dct_auto` selects the format on load.

The current writer places the root directory immediately after the block header,
then leaf-directory bytes, then tile data. Readers should use the offsets rather
than assuming that layout.

## Tile Directories

A directory maps Hilbert tile IDs to tile blobs. The root directory is compressed
with the file's internal compression. If the root would exceed the block root
limit, the writer partitions it into compressed leaf directories.

After decompression, a directory is columnar:

```text
count
tile_id_deltas[count]
run_lengths[count]
lengths[count]
offsets[count]
```

| Column | Encoding | Meaning |
|---|---|---|
| `count` | LEB128 `u64` | Number of entries |
| `tile_id_deltas` | LEB128 `u64` each | Delta-coded sorted tile IDs |
| `run_lengths` | LEB128 `u64` each | Number of consecutive tile IDs covered; `0` means leaf pointer |
| `lengths` | LEB128 `u64` each | Blob length, or compressed leaf length |
| `offsets` | LEB128 `u64` each | Offset encoding described below |

Offset encoding:

- If encoded offset is nonzero, decoded offset is `encoded - 1`.
- If encoded offset is zero, decoded offset is previous decoded offset plus
  previous length.
- The first entry cannot use encoded offset zero.

For data entries, `run_length >= 1`; `offset` and `length` are relative to the
block's tile-data region. A run covers tile IDs:

```text
[tile_id, tile_id + run_length)
```

Every tile ID in the run points to the same tile blob. This is used for
deduplicated identical tiles.

For leaf-pointer entries, `run_length == 0`; `offset` and `length` are relative
to the block's leaf-directory region. The pointed-to bytes are one compressed
directory. Current writers create one root level plus one leaf level.

Lookup algorithm:

1. Binary-search for the greatest directory `tile_id <= target`.
2. If the entry is a leaf pointer, load that leaf and repeat the search.
3. If it is a data entry and `target < tile_id + run_length`, read that blob.
4. Otherwise the tile is absent.

Absent tiles are legal in sparse blocks.

## Tile IDs

WMTiles uses one global tile-ID space across all zoom levels. Within a zoom,
tiles are ordered by a standard Hilbert curve.

```text
zoom_offset(z) = (4^z - 1) / 3
tile_id(z, x, y) = zoom_offset(z) + hilbert_xy_to_d(z, x, y)
```

`x` and `y` are zero-based XYZ tile coordinates and must satisfy:

```text
0 <= x < 2^z
0 <= y < 2^z
```

Verification vector:

```text
tile_id(12, 3423, 1763) = 19078479
```

Hilbert ordering is important because adjacent map tiles tend to be close in
file byte order, which makes viewport reads coalesce into fewer range requests.

## Quantized Values

Every block has its own quantization parameters in the block-table entry. Tile
payloads store quantized bytes; readers dequantize to `float32`.

### DTypes

| ID | Name | Bytes/sample | Valid finite codes | NoData |
|---:|---|---:|---|---|
| `0` | `u8` | 1 | `0..254` | `0xFF` |
| `1` | `u16` | 2 | `0..65534` | `0xFFFF` |
| `3` | `f32` | 4 | IEEE-754 `float32` | canonical quiet NaN `0x7FC00000` |

For `u8` and `u16`, dequantization is:

```text
value = q * scale + offset
```

If `q` equals the dtype's NoData sentinel, the value is `NaN`.

For `f32`, tile payload bytes are little-endian `float32` values. Writers
canonicalize all NaNs to `0x7FC00000` so identical missing-data tiles deduplicate
reliably.

### Encoder Parameter Selection

The current encoder chooses parameters from the finite observed value range of a
single block:

- If all finite values are equal, use `u16`, `scale = 0`, `offset = value`.
- If requested precision is positive, use that precision as `scale`.
- Use `u8` if `ceil((vmax - vmin) / precision) + 1 <= 255`.
- Otherwise use `u16` if the same expression is `<= 65535`.
- Otherwise fall back to `f32`.
- If requested precision is zero, use full-range `u16` with
  `scale = (vmax - vmin) / 65534`.

The maximum finite quantization error for `u8`/`u16` is approximately
`abs(scale) / 2`, plus normal `float32` representation slack after decode.

## Tile Codecs

Each tile blob starts with a one-byte codec tag. The block table has a default
codec field for statistics and hints, but the tag on each tile blob is
authoritative.

| Tag | Name | Payload |
|---:|---|---|
| `0x00` | reserved | invalid |
| `0x01` | constant | Four bytes containing the constant encoded sample |
| `0x02` | raw + zstd | zstd-compressed quantized byte stream |
| `0x03` | bitshuffle + zstd | zstd-compressed bitshuffled byte stream |
| `0x04` | vertical delta + zstd | zstd-compressed row-delta byte stream |
| `0x05` | Lorenzo predictor + zstd | zstd-compressed 2D residual byte stream |

The decoded quantized byte stream length must be:

```text
tile_pixel_count * dtype_bytes
```

where:

```text
tile_pixel_count = (1 << tile_pixel_size_log2)^2
```

### Constant (`0x01`)

The blob is always 5 bytes:

```text
tag, payload[4]
```

Only the first `dtype_bytes` payload bytes are significant. For `u16` and `f32`,
the value is little-endian.

### Raw Zstd (`0x02`)

The payload is zstd-compressed quantized bytes in row-major order.

### Bitshuffle Zstd (`0x03`)

The payload is zstd-compressed bitshuffle output. The uncompressed inner length
must be:

```text
8 * dtype_bytes * ceil(tile_pixel_count / 8)
```

Bitshuffle stores one bit plane at a time. Plane order is byte `0` bit `0`,
byte `0` bit `1`, ..., byte `0` bit `7`, then byte `1` bit `0`, and so on.
Within a plane byte, element bits are packed most-significant-bit first.

### Vertical Delta Zstd (`0x04`)

The payload is zstd-compressed vertical deltas. This codec is valid for `u8` and
`u16` square tiles.

- The first row is stored unchanged.
- Each following row stores `current - value_above`.
- Arithmetic wraps modulo `2^(8 * dtype_bytes)`.

### Raw-Grid Blocks

Files encoded with `--no-tiles` skip the Web-Mercator pyramid. Each
`(variable_id, time_id)` pair becomes one **raw-grid block** that stores the
native source lat-lon grid chunked into fixed-size sub-tiles. Point queries by
`(lat, lon)` are O(1) range requests; the file does not drive a slippy-map
viewer without on-the-fly resampling.

A raw-grid block reuses the 64-byte block header with `BlockFlagRawGrid`
(`1 << 2`) set in `block_flags`. Field interpretation changes:

- `root_directory_offset` and `root_directory_length` point at the compressed
  raw-grid section (the equivalent of the tile directory).
- `leaf_directories_offset` and `leaf_directories_length` are zero.
- `tile_data_offset` and `tile_data_length` describe the concatenated chunk
  payload region.
- `num_addressed_tiles` and `num_directory_entries` hold the total chunk count
  (`chunk_count_x * chunk_count_y`).
- `num_tile_contents` holds the deduplicated chunk count.

The block-table entry's `codec` is the dominant chunk codec ID for stats; each
chunk payload still carries its own one-byte codec tag.

#### Raw-Grid Section

The raw-grid section is internally compressed (same compression as the file's
catalogs and directories). After decompression:

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 1 | `u8` | `schema_version`, currently `1` |
| `1` | 1 | `u8` | `chunk_size_log2`; chunk side in source pixels = `1 << value` |
| `2` | 2 | bytes | Reserved, written as zero |
| `4` | 4 | `u32` | `nx`, source grid width in pixels |
| `8` | 4 | `u32` | `ny`, source grid height in pixels |
| `16` | 8 | `f64` | `lat0`, latitude at source row `0` |
| `24` | 8 | `f64` | `lon0`, longitude at source column `0` |
| `32` | 8 | `f64` | `dy`, latitude step per row (may be negative) |
| `40` | 8 | `f64` | `dx`, longitude step per column (may be negative) |
| `48` | 8 | `f64` | `missing_value`, source NoData sentinel (may be NaN) |
| `56` | 4 | `u32` | `chunk_count_x`, = `ceil(nx / chunk_size)` |
| `60` | 4 | `u32` | `chunk_count_y`, = `ceil(ny / chunk_size)` |
| `64` | varint × N | LEB128 `u64` each | `chunk_offsets`, one per chunk row-major |
| ... | varint × N | LEB128 `u64` each | `chunk_lengths`, one per chunk row-major |

`chunk_size_log2` must be in `[4, 12]`, allowing chunk sides of 16..4096
source pixels. `N = chunk_count_x * chunk_count_y`. Chunk index is
`cy * chunk_count_x + cx`, so chunks are stored in row-major order.

`chunk_offsets[i]` is a byte offset relative to the block's `tile_data_offset`.
`chunk_lengths[i]` is the chunk payload length. A `(0, 0)` pair signals
**absent chunk**; decoders fill all pixels with NaN without fetching anything.

#### Chunk Payloads

Each chunk payload is a normal tile blob: one codec tag byte followed by the
codec-specific stream. The current encoder uses `0x01` (constant) when all
quantised values in the chunk are identical, otherwise `0x03`
(bitshuffle + zstd). Lorenzo and delta codecs are not emitted for raw-grid
chunks because edge chunks at the right/bottom border may be non-square.

Chunk pixel count is `chunk_width(cx) * chunk_height(cy)` where:

```text
chunk_width(cx)  = min(chunk_size, nx - cx*chunk_size)
chunk_height(cy) = min(chunk_size, ny - cy*chunk_size)
```

Pixels inside a chunk are row-major: `chunk[row*chunk_width + col]` is the
quantised value at source coordinates `(cx*chunk_size + col, cy*chunk_size + row)`,
which corresponds to lat/lon `(lat0 + (cy*chunk_size + row)*dy, lon0 + (cx*chunk_size + col)*dx)`.

#### Point Sampling

To compute the value at `(lat, lon)`:

1. Compute source-grid coordinates `gx = (lon - lon0) / dx`,
   `gy = (lat - lat0) / dy`. NaN or out-of-range inputs return NaN.
2. Find the four neighbours `(x0, y0)`, `(x1, y0)`, `(x0, y1)`, `(x1, y1)` with
   `x0 = floor(gx)`, `y0 = floor(gy)`, `x1 = x0+1`, `y1 = y0+1` clamped to
   `[0, nx-1]` / `[0, ny-1]`.
3. For each neighbour, locate the chunk `cx = x / chunk_size`,
   `cy = y / chunk_size`, fetch and decode the chunk, then read the pixel.
4. Bilinearly interpolate: `wx = gx - x0`, `wy = gy - y0`,
   `v = ((1-wx)*v00 + wx*v10)*(1-wy) + ((1-wx)*v01 + wx*v11)*wy`.
   If any neighbour is NaN, the result is NaN.

For batched queries the reader unions the chunk indices touched by all points
(including the 2×2 bilinear neighbourhood) and coalesces adjacent chunk byte
ranges into shared HTTP-range requests, the same strategy as tile coalescing.

A raw-grid file's header records `min_zoom = 0`, `max_zoom = 0`, and the
authoritative signal that consumers should branch on is `BlockFlagRawGrid` on
each block header. Mixing tiled and raw-grid blocks in the same file is
permitted by the wire format but the current encoder does not produce mixed
files.

### Lorenzo Zstd (`0x05`)

The payload is zstd-compressed 2D Lorenzo residuals. This codec is valid for
`u8` and `u16` square tiles.

- Top-left sample is stored unchanged.
- Top row uses horizontal deltas.
- Left column uses vertical deltas.
- Interior samples store:

```text
residual(x, y) = q(x, y) - q(x-1, y) - q(x, y-1) + q(x-1, y-1)
```

Arithmetic wraps modulo `2^(8 * dtype_bytes)`, making decode exact in quantized
space.

## Metadata

The metadata section is internally compressed UTF-8 JSON. The format does not
require a particular schema, but current writers include keys such as:

- `generator`
- `creationTime`
- `forecastReferenceTime`
- `tilePixelSize`
- `minZoom`
- `maxZoom`
- `snapshotHistory`

Readers should tolerate unknown keys and missing metadata.

## File Trailer

The file trailer is normally located at:

```text
file_logical_end - 16
```

It is used by verification and append logic to identify the logical end.

| Offset | Size | Type | Field |
|---:|---:|---|---|
| `0` | 4 | `u32` | File trailer magic `0xEEEFFFFF` |
| `4` | 8 | `u64` | `file_logical_end` |
| `12` | 4 | bytes | Reserved, written as zero |

## Reading A Tile

To read `(variable_name, time_id, z, x, y)`:

1. Fetch the first 64 KiB.
2. Parse and validate the 256-byte header.
3. Load the active snapshot from the cold-start buffer if possible, otherwise by
   range request.
4. Validate the snapshot trailer and CRC.
5. Parse the variable catalog and find `variable_id`.
6. Search the block-table root for `(variable_id, time_id)`, loading a
   block-table leaf if needed.
7. Fetch the block prefix: `block_header_size + max_block_root_bytes`, capped by
   `block_length`.
8. Parse the block header and root directory.
9. Compute the Hilbert tile ID and search the root directory, loading a
   directory leaf if needed.
10. Fetch the tile blob at:

```text
block_offset + block_header.tile_data_offset + directory_entry.offset
```

11. Decode the tile blob according to its codec tag.
12. Dequantize with the block-table entry's `dtype`, `scale`, `offset`, and
    `nodata`.

If the header CRC fails, or the active snapshot fails validation, and
`has_previous_snapshot` is set, repeat snapshot loading with
`previous_snapshot_offset` and `previous_snapshot_length`.

## Append Safety

The local append protocol is:

1. Start writing at `file_logical_end - file_trailer_size`, replacing the old
   trailer.
2. Append new immutable blocks.
3. Append a full new immutable snapshot.
4. Append a new file trailer.
5. `fsync`.
6. Rewrite the 256-byte header with new snapshot pointers, generation, flags,
   logical end, CRC, and tail magic.
7. `fsync` again.
8. Truncate to the new logical end.

Readers either see the old generation or the new generation. A torn header write
is rejected by the header CRC or tail magic; if the previous snapshot pointer is
available, readers fall back automatically.

