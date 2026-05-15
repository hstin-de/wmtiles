package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/hstin-de/wmtiles/varint"
)

// BlockFlagRawGrid marks a block whose payload is a native source-grid raster
// instead of a Hilbert tile pyramid. The on-disk shape is:
//
//	+--------------+--------------------+-----------+
//	| Block header | Raw-grid section   | Chunks    |
//	| 64 B         | (compressed)       | (payload) |
//	+--------------+--------------------+-----------+
//
// The block header is the same 64-byte struct as a tiled block; only the
// interpretation of a few fields changes:
//
//   - RootDirectoryOffset/Length point at the compressed raw-grid section.
//   - LeafDirectoriesOffset/Length are zero.
//   - TileDataOffset/Length describe the concatenated chunk payloads.
//   - NumAddressedTiles, NumDirectoryEntries hold the total chunk count.
//   - NumTileContents holds the deduplicated chunk count.
//
// The block-table entry's Codec field is the dominant chunk codec ID (stats
// hint only); each chunk payload still carries its own one-byte codec tag,
// which is authoritative for decode dispatch.
const BlockFlagRawGrid uint16 = 1 << 2

const RawGridSchemaVersion uint8 = 1

const RawGridHeaderSize = 64

// RawGridSection is the per-block source-grid descriptor, stored compressed
// at BlockHeader.RootDirectoryOffset.
type RawGridSection struct {
	SchemaVersion uint8

	// ChunkSizeLog2 fixes the square chunk side length in source pixels at
	// 1 << ChunkSizeLog2. Edge chunks may be smaller along the right/bottom
	// border; pixel count is always ChunkWidth(x) * ChunkHeight(y).
	ChunkSizeLog2 uint8

	Nx uint32
	Ny uint32

	Lat0 float64
	Lon0 float64
	DY   float64
	DX   float64

	// MissingValue is the source NoData sentinel; NaN means "not set". The
	// encoder canonicalises both to the dtype-specific NoData code, so this
	// field is mostly informational for decoders that want to surface it.
	MissingValue float64

	ChunkCountX uint32
	ChunkCountY uint32

	// ChunkOffsets[i] is the byte offset of chunk i within the block's
	// tile-data region. ChunkLengths[i] is the byte length. A zero/zero pair
	// means "chunk absent" (all-NoData); the decoder fills NaN without
	// fetching anything. Chunks are indexed row-major: i = cy*ChunkCountX + cx.
	ChunkOffsets []uint64
	ChunkLengths []uint64
}

func (s *RawGridSection) ChunkCount() int {
	return int(s.ChunkCountX) * int(s.ChunkCountY)
}

func (s *RawGridSection) ChunkSize() int { return 1 << s.ChunkSizeLog2 }

// Edge chunks may be smaller than 1<<ChunkSizeLog2 along the right/bottom border.
func (s *RawGridSection) ChunkWidth(cx uint32) int {
	cs := uint32(s.ChunkSize())
	return int(min((cx+1)*cs, s.Nx) - cx*cs)
}

func (s *RawGridSection) ChunkHeight(cy uint32) int {
	cs := uint32(s.ChunkSize())
	return int(min((cy+1)*cs, s.Ny) - cy*cs)
}

func (s *RawGridSection) ChunkPixelCount(cx, cy uint32) int {
	return s.ChunkWidth(cx) * s.ChunkHeight(cy)
}

func (s *RawGridSection) ChunkIndex(cx, cy uint32) int {
	return int(cy)*int(s.ChunkCountX) + int(cx)
}

// Caller is expected to internally compress the result.
func MarshalRawGridSection(s *RawGridSection) ([]byte, error) {
	if s.ChunkCountX == 0 || s.ChunkCountY == 0 {
		return nil, errors.New("raw grid: empty chunk grid")
	}
	if int64(s.ChunkCountX)*int64(s.ChunkCountY) != int64(len(s.ChunkOffsets)) ||
		int64(s.ChunkCountX)*int64(s.ChunkCountY) != int64(len(s.ChunkLengths)) {
		return nil, fmt.Errorf("raw grid: chunk dir size mismatch (X=%d Y=%d offs=%d lens=%d)",
			s.ChunkCountX, s.ChunkCountY, len(s.ChunkOffsets), len(s.ChunkLengths))
	}
	if s.ChunkSizeLog2 < 4 || s.ChunkSizeLog2 > 12 {
		return nil, fmt.Errorf("raw grid: chunk size log2 %d out of [4, 12]", s.ChunkSizeLog2)
	}

	buf := make([]byte, RawGridHeaderSize, RawGridHeaderSize+16*s.ChunkCount())
	buf[0] = s.SchemaVersion
	buf[1] = s.ChunkSizeLog2
	binary.LittleEndian.PutUint32(buf[4:], s.Nx)
	binary.LittleEndian.PutUint32(buf[8:], s.Ny)
	binary.LittleEndian.PutUint64(buf[16:], math.Float64bits(s.Lat0))
	binary.LittleEndian.PutUint64(buf[24:], math.Float64bits(s.Lon0))
	binary.LittleEndian.PutUint64(buf[32:], math.Float64bits(s.DY))
	binary.LittleEndian.PutUint64(buf[40:], math.Float64bits(s.DX))
	binary.LittleEndian.PutUint64(buf[48:], math.Float64bits(s.MissingValue))
	binary.LittleEndian.PutUint32(buf[56:], s.ChunkCountX)
	binary.LittleEndian.PutUint32(buf[60:], s.ChunkCountY)

	for _, o := range s.ChunkOffsets {
		buf = varint.Append(buf, o)
	}
	for _, l := range s.ChunkLengths {
		buf = varint.Append(buf, l)
	}
	return buf, nil
}

// Caller is expected to internally decompress before calling.
func UnmarshalRawGridSection(buf []byte) (*RawGridSection, error) {
	if len(buf) < RawGridHeaderSize {
		return nil, fmt.Errorf("raw grid: need %d bytes, got %d", RawGridHeaderSize, len(buf))
	}
	s := &RawGridSection{
		SchemaVersion: buf[0],
		ChunkSizeLog2: buf[1],
		Nx:            binary.LittleEndian.Uint32(buf[4:]),
		Ny:            binary.LittleEndian.Uint32(buf[8:]),
		Lat0:          math.Float64frombits(binary.LittleEndian.Uint64(buf[16:])),
		Lon0:          math.Float64frombits(binary.LittleEndian.Uint64(buf[24:])),
		DY:            math.Float64frombits(binary.LittleEndian.Uint64(buf[32:])),
		DX:            math.Float64frombits(binary.LittleEndian.Uint64(buf[40:])),
		MissingValue:  math.Float64frombits(binary.LittleEndian.Uint64(buf[48:])),
		ChunkCountX:   binary.LittleEndian.Uint32(buf[56:]),
		ChunkCountY:   binary.LittleEndian.Uint32(buf[60:]),
	}
	if s.SchemaVersion != RawGridSchemaVersion {
		return nil, fmt.Errorf("raw grid: unsupported schema version %d (want %d)",
			s.SchemaVersion, RawGridSchemaVersion)
	}
	if s.ChunkCountX == 0 || s.ChunkCountY == 0 {
		return nil, errors.New("raw grid: zero-sized chunk grid")
	}
	n := int(s.ChunkCountX) * int(s.ChunkCountY)
	s.ChunkOffsets = make([]uint64, n)
	s.ChunkLengths = make([]uint64, n)

	pos := RawGridHeaderSize
	for i := range n {
		v, used, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("raw grid: chunk offset %d: %w", i, err)
		}
		pos += used
		s.ChunkOffsets[i] = v
	}
	for i := range n {
		v, used, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("raw grid: chunk length %d: %w", i, err)
		}
		pos += used
		s.ChunkLengths[i] = v
	}
	return s, nil
}
