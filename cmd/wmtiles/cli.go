package main

import (
	"fmt"
	"strings"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/parser"
)

type CLI struct {
	Encode          encodeCmd          `cmd:"" help:"encode GRIB2 or HDF5 sources into a fresh .wmt"`
	Extend          extendCmd          `cmd:"" help:"append blocks for new (variable, time) pairs to an existing .wmt"`
	Compact         compactCmd         `cmd:"" help:"rewrite a file with the snapshot in the cold-start window"`
	SnapshotHistory snapshotHistoryCmd `cmd:"" name:"snapshot-history" help:"list active + previous snapshots"`
	Inspect         inspectCmd         `cmd:"" help:"dump header + catalog + stats"`
	Verify          verifyCmd          `cmd:"" help:"structural sanity check (incl. CRC validation)"`
	Compare         compareCmd         `cmd:"" help:"pixel by pixel fidelity report vs. source"`
	Serve           serveCmd           `cmd:"" help:"launch a web viewer"`
}

type encodeCmd struct {
	Output            string `short:"o" required:"" placeholder:"PATH" help:"output .wmt path"`
	Format            string `enum:"auto,grib2,hdf5" default:"auto" help:"override input-format auto-detection"`
	MinZoom           uint8  `default:"0" help:"minimum zoom level (ignored when --no-tiles is set)"`
	MaxZoom           uint8  `default:"5" help:"maximum zoom level (ignored when --no-tiles is set)"`
	TileSizeLog2      uint8  `name:"tile-size-log2" default:"8" help:"tile size as log2 of pixel count (7..10 -> 128..1024)"`
	NoTiles           bool   `name:"no-tiles" help:"skip the Web-Mercator pyramid; store source-grid chunks for point-query (lat/lon) API use. Output is not viewable on a slippy map without on-the-fly tiling"`
	RawChunkSizeLog2  uint8  `name:"raw-chunk-size-log2" default:"8" help:"source-pixel side of one raw-grid chunk as log2 (4..12 -> 16..4096). Only consulted with --no-tiles"`
	Filter            string `help:"comma-separated source variable shortNames to keep (default: all)"`
	Precision         string `placeholder:"NAME=K,..." help:"per-variable quantisation precision overrides (default: lookup table + auto-cap)"`
	DisableDeltaCodec bool   `help:"force bitshuffle-only encoding (faster, larger files)"`
	ZstdLevel         int    `default:"0" help:"libzstd compression level 1..22 (0 = encoder default 3)"`
	TileDict          bool   `help:"train a per-block zstd dictionary (slower encode, ~5-20% smaller blocks)"`

	CPUProfile string `name:"cpuprofile" placeholder:"FILE" help:"write CPU profile to FILE" hidden:""`
	MemProfile string `name:"memprofile" placeholder:"FILE" help:"write heap profile to FILE" hidden:""`
	Trace      string `placeholder:"FILE" help:"write execution trace to FILE" hidden:""`

	Inputs []string `arg:"" name:"input" help:"source files, globs, or directories"`
}

func (c *encodeCmd) Validate() error {
	if c.TileSizeLog2 < 7 || c.TileSizeLog2 > 10 {
		return fmt.Errorf("tile-size-log2 must be 7..10, got %d", c.TileSizeLog2)
	}
	if !c.NoTiles && c.MaxZoom < c.MinZoom {
		return fmt.Errorf("max-zoom %d < min-zoom %d", c.MaxZoom, c.MinZoom)
	}
	if c.NoTiles && (c.RawChunkSizeLog2 < 4 || c.RawChunkSizeLog2 > 12) {
		return fmt.Errorf("raw-chunk-size-log2 must be 4..12, got %d", c.RawChunkSizeLog2)
	}
	return nil
}

func (c *encodeCmd) Run() error { return runEncode(c) }

type extendCmd struct {
	File         string `arg:"" name:"file" help:"existing .wmt file"`
	Source       string `arg:"" name:"source" help:"GRIB2 or HDF5 source"`
	Filter       string `help:"comma-separated source variable shortNames to keep (default: all)"`
	AllowReplace bool   `help:"overwrite existing (variable, time) blocks instead of erroring"`
	Precision    string `placeholder:"NAME=K,..." help:"per-variable quantisation precision overrides"`
	Format       string `enum:"auto,grib2,hdf5" default:"auto" help:"override source-format auto-detection"`
}

func (c *extendCmd) Run() error { return runExtend(c) }

type compactCmd struct {
	Input  string `arg:"" name:"input" help:"existing .wmt file"`
	Output string `arg:"" name:"output" help:"compacted output .wmt path"`
}

func (c *compactCmd) Run() error { return runCompact(c) }

type snapshotHistoryCmd struct {
	File string `arg:"" name:"file" help:".wmt file"`
}

func (c *snapshotHistoryCmd) Run() error { return runSnapshotHistory(c.File) }

type inspectCmd struct {
	File string `arg:"" name:"file" help:".wmt file"`
}

func (c *inspectCmd) Run() error { return runInspect(c.File) }

type verifyCmd struct {
	File string `arg:"" name:"file" help:".wmt file"`
}

func (c *verifyCmd) Run() error { return runVerify(c.File) }

type compareCmd struct {
	Source    string  `arg:"" name:"source" help:"GRIB2 or HDF5 input"`
	File      string  `arg:"" name:"file" help:"existing .wmt"`
	Variable  string  `help:"only check this .wmt variable name (default: all)"`
	Zoom      int     `default:"-1" help:"only check tiles at this zoom level (default: all zooms)"`
	Tolerance float64 `help:"override per-variable tolerance (default: per-block scale/2 + f32 slack)"`
	Format    string  `enum:"auto,grib2,hdf5" default:"auto" help:"override source-format auto-detection"`
}

func (c *compareCmd) Run() error { return runCompare(c) }

type serveCmd struct {
	File string `arg:"" name:"file" help:".wmt file to serve"`
	Addr string `default:":8080" help:"listen address"`
}

func (c *serveCmd) Run() error { return runServe(c) }

// extend and compare take exactly one source so they skip the glob handling
// that the encode subcommand does. Auto-detect is just magic-bytes here.
func resolveInputFormat(formatOverride, srcPath string) (encode.Format, error) {
	switch strings.ToLower(formatOverride) {
	case "grib2":
		return encode.FormatGRIB2, nil
	case "hdf5":
		return encode.FormatHDF5, nil
	case "", "auto":
		if parser.IsHDF5File(srcPath) {
			return encode.FormatHDF5, nil
		}
		return encode.FormatGRIB2, nil
	}
	return "", fmt.Errorf("--format must be auto, grib2 or hdf5, got %q", formatOverride)
}
