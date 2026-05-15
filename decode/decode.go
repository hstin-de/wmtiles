package decode

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

var (
	// ErrNotFound is returned when a requested block or tile is not present.
	ErrNotFound = reader.ErrNotFound

	// ErrUnknownVariable is returned when a variable name is not in the file.
	ErrUnknownVariable = reader.ErrUnknownVariable

	// ErrRawGridBlock is returned by ReadTile when the block was encoded with
	// --no-tiles; use Sample / Samples instead.
	ErrRawGridBlock = reader.ErrRawGridBlock

	// ErrNotRawGrid is returned by Sample / Samples when the block is a tile
	// pyramid; use ReadTile / ReadTiles instead.
	ErrNotRawGrid = reader.ErrNotRawGrid
)

// Bounds describes the geographic extent of a dataset in lon/lat degrees.
type Bounds struct {
	West  float64
	South float64
	East  float64
	North float64
}

type Variable struct {
	Name     string
	Unit     string
	Colormap string

	// Precision is the quantisation step recorded by the encoder.
	Precision float64

	// ValueMin and ValueMax are the observed value range across all blocks.
	ValueMin float64
	ValueMax float64
}

// TileCoord identifies a Web Mercator XYZ tile.
type TileCoord struct {
	Z    uint8
	X, Y uint32
}

func Coord(z uint8, x, y uint32) TileCoord {
	return TileCoord{Z: z, X: x, Y: y}
}

// ReadOptions controls multi-tile range coalescing. Zero values use defaults.
type ReadOptions struct {
	MaxGapBytes     uint64
	MaxRequestBytes uint64
}

// Decoder is an opened WMTiles file.
type Decoder struct {
	r *reader.Reader
}

func Open(path string) (*Decoder, error) {
	r, err := reader.Open(path)
	if err != nil {
		return nil, err
	}
	return &Decoder{r: r}, nil
}

func NewReader(src io.ReaderAt) (*Decoder, error) {
	r, err := reader.NewReader(src)
	if err != nil {
		return nil, err
	}
	return &Decoder{r: r}, nil
}

// Close closes the underlying file when Decoder was created with Open.
func (d *Decoder) Close() error {
	if d == nil || d.r == nil {
		return nil
	}
	return d.r.Close()
}

// PixelCount returns the number of float32 values in one tile.
func (d *Decoder) PixelCount() int { return d.r.PixelCount() }

// TileSize returns the tile width and height in pixels.
func (d *Decoder) TileSize() int { return 1 << d.r.Header.TilePixelSizeLog2 }

func (d *Decoder) ZoomRange() (minZoom, maxZoom uint8) {
	return d.r.Header.MinZoom, d.r.Header.MaxZoom
}

// Bounds returns the dataset bounds in lon/lat degrees.
func (d *Decoder) Bounds() Bounds {
	h := d.r.Header
	return Bounds{
		West:  float64(h.BBoxLonMinE7) / 1e7,
		South: float64(h.BBoxLatMinE7) / 1e7,
		East:  float64(h.BBoxLonMaxE7) / 1e7,
		North: float64(h.BBoxLatMaxE7) / 1e7,
	}
}

// Variables returns the variables in file order.
func (d *Decoder) Variables() []Variable {
	out := make([]Variable, len(d.r.Snapshot.Variables))
	for i, v := range d.r.Snapshot.Variables {
		out[i] = variableFromFormat(v)
	}
	return out
}

func (d *Decoder) Variable(name string) (Variable, bool) {
	v, ok := d.r.Variable(name)
	if !ok {
		return Variable{}, false
	}
	return variableFromFormat(v), true
}

// Times returns the full time axis in index order.
func (d *Decoder) Times() []time.Time {
	tc := d.r.Snapshot.TimeCat
	out := make([]time.Time, tc.Count)
	if tc.Regular {
		for i := range out {
			out[i] = time.UnixMilli(tc.StartMs + int64(i)*tc.IntervalMs).UTC()
		}
		return out
	}
	for i := range out {
		out[i] = time.UnixMilli(tc.TimestampsMs[i]).UTC()
	}
	return out
}

func (d *Decoder) Time(index int) (time.Time, bool) {
	if index < 0 {
		return time.Time{}, false
	}
	tc := d.r.Snapshot.TimeCat
	if int64(index) >= tc.Count {
		return time.Time{}, false
	}
	if tc.Regular {
		return time.UnixMilli(tc.StartMs + int64(index)*tc.IntervalMs).UTC(), true
	}
	if index >= len(tc.TimestampsMs) {
		return time.Time{}, false
	}
	return time.UnixMilli(tc.TimestampsMs[index]).UTC(), true
}

// TimeIndex returns the index for a timestamp. Comparison is at millisecond
// precision because that is the resolution stored in the file.
func (d *Decoder) TimeIndex(t time.Time) (int, bool) {
	return timeIndexInCatalog(d.r.Snapshot.TimeCat, t)
}

// Metadata returns a shallow copy of the snapshot metadata.
func (d *Decoder) Metadata() map[string]any {
	out := make(map[string]any, len(d.r.Snapshot.Metadata))
	for k, v := range d.r.Snapshot.Metadata {
		out[k] = v
	}
	return out
}

// NewTileBuffer allocates a float32 tile buffer sized for this dataset.
func (d *Decoder) NewTileBuffer() []float32 {
	return make([]float32, d.PixelCount())
}

// ReadTile reads one tile and returns a newly allocated pixel buffer.
func (d *Decoder) ReadTile(variable string, timeIndex int, coord TileCoord) ([]float32, error) {
	out := d.NewTileBuffer()
	if err := d.ReadTileInto(variable, timeIndex, coord, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadTileInto reads one tile into out. It is the zero-allocation variant for
// callers that reuse tile buffers.
func (d *Decoder) ReadTileInto(variable string, timeIndex int, coord TileCoord, out []float32) error {
	ti, err := checkedTimeIndex(timeIndex)
	if err != nil {
		return err
	}
	return d.r.ReadTile(variable, ti, coord.Z, coord.X, coord.Y, out)
}

// ReadTileAt reads one tile by timestamp and returns a newly allocated pixel
// buffer.
func (d *Decoder) ReadTileAt(variable string, t time.Time, coord TileCoord) ([]float32, error) {
	out := d.NewTileBuffer()
	if err := d.ReadTileAtInto(variable, t, coord, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *Decoder) ReadTileAtInto(variable string, t time.Time, coord TileCoord, out []float32) error {
	idx, ok := d.TimeIndex(t)
	if !ok {
		return fmt.Errorf("wmtiles/decode: time %s not found", t.UTC().Format(time.RFC3339Nano))
	}
	return d.ReadTileInto(variable, idx, coord, out)
}

// ReadTiles reads multiple tiles for one variable/time and returns newly
// allocated pixel buffers. Missing tiles are filled with NaN values, matching
// viewport-style reads.
func (d *Decoder) ReadTiles(variable string, timeIndex int, coords []TileCoord, opts ...ReadOptions) ([][]float32, error) {
	outs := make([][]float32, len(coords))
	for i := range outs {
		outs[i] = d.NewTileBuffer()
	}
	if err := d.ReadTilesInto(variable, timeIndex, coords, outs, opts...); err != nil {
		return nil, err
	}
	return outs, nil
}

// ReadTilesInto reads multiple tiles into outs. It is the zero-allocation
// variant for callers that reuse tile buffers.
func (d *Decoder) ReadTilesInto(variable string, timeIndex int, coords []TileCoord, outs [][]float32, opts ...ReadOptions) error {
	ti, err := checkedTimeIndex(timeIndex)
	if err != nil {
		return err
	}
	opt, err := readOptions(opts)
	if err != nil {
		return err
	}
	rcoords := make([]reader.TileCoord, len(coords))
	for i, c := range coords {
		rcoords[i] = reader.TileCoord{Z: c.Z, X: c.X, Y: c.Y}
	}
	return d.r.ReadTilesInBlock(variable, ti, rcoords, outs, reader.CoalesceOptions{
		MaxGapBytes:     opt.MaxGapBytes,
		MaxRequestBytes: opt.MaxRequestBytes,
	})
}

type SamplePoint struct {
	Lat, Lon float64
}

// Returns NaN for points outside the source grid; ErrNotRawGrid when the block is a tile pyramid.
func (d *Decoder) Sample(variable string, timeIndex int, lat, lon float64) (float32, error) {
	ti, err := checkedTimeIndex(timeIndex)
	if err != nil {
		return 0, err
	}
	return d.r.ReadSample(variable, ti, lat, lon)
}

func (d *Decoder) SampleAt(variable string, t time.Time, lat, lon float64) (float32, error) {
	idx, ok := d.TimeIndex(t)
	if !ok {
		return 0, fmt.Errorf("wmtiles/decode: time %s not found", t.UTC().Format(time.RFC3339Nano))
	}
	return d.Sample(variable, idx, lat, lon)
}

// Chunk fetches are coalesced so a viewport-sized batch stays network-cheap.
func (d *Decoder) Samples(variable string, timeIndex int, points []SamplePoint, opts ...ReadOptions) ([]float32, error) {
	ti, err := checkedTimeIndex(timeIndex)
	if err != nil {
		return nil, err
	}
	opt, err := readOptions(opts)
	if err != nil {
		return nil, err
	}
	rpoints := make([]reader.SamplePoint, len(points))
	for i, p := range points {
		rpoints[i] = reader.SamplePoint{Lat: p.Lat, Lon: p.Lon}
	}
	return d.r.ReadSamples(variable, ti, rpoints, reader.SampleCoalesceOptions{
		MaxGapBytes:     opt.MaxGapBytes,
		MaxRequestBytes: opt.MaxRequestBytes,
	})
}

func (d *Decoder) IsRawGridBlock(variable string, timeIndex int) (bool, error) {
	ti, err := checkedTimeIndex(timeIndex)
	if err != nil {
		return false, err
	}
	return d.r.IsRawGridBlock(variable, ti)
}

// SanityCheck validates the loaded header and snapshot.
func (d *Decoder) SanityCheck() error { return d.r.SanityCheck() }

func variableFromFormat(v format.VariableEntry) Variable {
	return Variable{
		Name:      v.Name,
		Unit:      v.Unit,
		Colormap:  v.ColormapHint,
		Precision: v.DefaultPrecisionHint,
		ValueMin:  v.ValueMinObservedGlobal,
		ValueMax:  v.ValueMaxObservedGlobal,
	}
}

func readOptions(opts []ReadOptions) (ReadOptions, error) {
	switch len(opts) {
	case 0:
		return ReadOptions{}, nil
	case 1:
		return opts[0], nil
	default:
		return ReadOptions{}, errors.New("wmtiles/decode: at most one ReadOptions value is allowed")
	}
}

func checkedTimeIndex(index int) (uint32, error) {
	if index < 0 {
		return 0, fmt.Errorf("wmtiles/decode: negative time index %d", index)
	}
	if uint64(index) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("wmtiles/decode: time index %d overflows uint32", index)
	}
	return uint32(index), nil
}

func timeIndexInCatalog(tc format.TimeCatalog, t time.Time) (int, bool) {
	ms := t.UnixMilli()
	if tc.Regular {
		if tc.Count == 0 {
			return 0, false
		}
		if tc.IntervalMs == 0 {
			if ms == tc.StartMs {
				return 0, true
			}
			return 0, false
		}
		delta := ms - tc.StartMs
		if delta < 0 || delta%tc.IntervalMs != 0 {
			return 0, false
		}
		idx := delta / tc.IntervalMs
		if idx < 0 || idx >= tc.Count {
			return 0, false
		}
		return int(idx), true
	}
	for i, ts := range tc.TimestampsMs {
		if ts == ms {
			return i, true
		}
	}
	return 0, false
}
