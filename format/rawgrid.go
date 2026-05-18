package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/hstin-de/wmtiles/varint"
)

// BlockFlagRawGrid marks a native source-grid raster block. Layout:
//
//	+--------------+--------------+-----------------+-----------+
//	| Block header | Root         | Fine-indices    | Chunks    |
//	| 64 B         | (compressed) | (per coarse     | (payload) |
//	|              |              |  cell, raw)     |           |
//	+--------------+--------------+-----------------+-----------+
//
// BlockHeader fields are reinterpreted: Root* points at the compressed
// RawGridSection (header + coarse table); LeafDirectories* covers the
// uncompressed fine-indices region; TileData* the chunk payloads.
const BlockFlagRawGrid uint16 = 1 << 2

const RawGridSchemaVersion uint8 = 1

const RawGridHeaderSize = 64

// Offsets are relative to the block's LeafDirectoriesOffset.
type CoarseEntry struct {
	Offset uint32
	Length uint32
}

// Root half (header + coarse table) only; chunk offsets/lengths live in the
// fine-indices region and are loaded on demand.
type RawGridSection struct {
	SchemaVersion uint8

	ChunkSizeLog2 uint8

	// side length in chunks (log2); encoder picks for ~64 coarse cells total.
	CoarseSizeLog2 uint8

	Nx uint32
	Ny uint32

	Lat0 float64
	Lon0 float64
	DY   float64
	DX   float64

	// source NoData sentinel (NaN = unset); encoder canonicalises to the
	// dtype's NoData code, so this is mostly informational on decode.
	MissingValue float64

	ChunkCountX uint32
	ChunkCountY uint32

	CoarseTable []CoarseEntry
}

func (s *RawGridSection) ChunkCount() int {
	return int(s.ChunkCountX) * int(s.ChunkCountY)
}

func (s *RawGridSection) ChunkSize() int { return 1 << s.ChunkSizeLog2 }

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

// side length in chunks, not pixels.
func (s *RawGridSection) CoarseSize() uint32 { return 1 << s.CoarseSizeLog2 }

func (s *RawGridSection) CoarseCountX() uint32 {
	cs := s.CoarseSize()
	return (s.ChunkCountX + cs - 1) / cs
}

func (s *RawGridSection) CoarseCountY() uint32 {
	cs := s.CoarseSize()
	return (s.ChunkCountY + cs - 1) / cs
}

func (s *RawGridSection) CoarseCount() int {
	return int(s.CoarseCountX()) * int(s.CoarseCountY())
}

// edge cells truncate.
func (s *RawGridSection) CoarseCellChunkExtent(coarseCx, coarseCy uint32) (w, h uint32) {
	cs := s.CoarseSize()
	w = min(cs, s.ChunkCountX-coarseCx*cs)
	h = min(cs, s.ChunkCountY-coarseCy*cs)
	return
}

func (s *RawGridSection) CoarseIndexOf(cx, cy uint32) (coarseIdx int, localIdx int) {
	cs := s.CoarseSize()
	coarseCx := cx / cs
	coarseCy := cy / cs
	cellW, _ := s.CoarseCellChunkExtent(coarseCx, coarseCy)
	localCx := cx - coarseCx*cs
	localCy := cy - coarseCy*cs
	coarseIdx = int(coarseCy)*int(s.CoarseCountX()) + int(coarseCx)
	localIdx = int(localCy)*int(cellW) + int(localCx)
	return
}

// caller compresses; returned slice is the block's root region.
func MarshalRawGridSectionRoot(s *RawGridSection) ([]byte, error) {
	if s.ChunkCountX == 0 || s.ChunkCountY == 0 {
		return nil, errors.New("raw grid: empty chunk grid")
	}
	if s.ChunkSizeLog2 < 4 || s.ChunkSizeLog2 > 12 {
		return nil, fmt.Errorf("raw grid: chunk size log2 %d out of [4, 12]", s.ChunkSizeLog2)
	}
	if s.CoarseSizeLog2 > 8 {
		return nil, fmt.Errorf("raw grid: coarse size log2 %d > 8", s.CoarseSizeLog2)
	}
	wantCoarse := s.CoarseCount()
	if len(s.CoarseTable) != wantCoarse {
		return nil, fmt.Errorf("raw grid: coarse table size mismatch (have=%d want=%d)",
			len(s.CoarseTable), wantCoarse)
	}

	buf := make([]byte, RawGridHeaderSize+8*wantCoarse)
	buf[0] = s.SchemaVersion
	buf[1] = s.ChunkSizeLog2
	buf[2] = s.CoarseSizeLog2
	// byte 3, bytes 12-15 reserved (zero)
	binary.LittleEndian.PutUint32(buf[4:], s.Nx)
	binary.LittleEndian.PutUint32(buf[8:], s.Ny)
	binary.LittleEndian.PutUint64(buf[16:], math.Float64bits(s.Lat0))
	binary.LittleEndian.PutUint64(buf[24:], math.Float64bits(s.Lon0))
	binary.LittleEndian.PutUint64(buf[32:], math.Float64bits(s.DY))
	binary.LittleEndian.PutUint64(buf[40:], math.Float64bits(s.DX))
	binary.LittleEndian.PutUint64(buf[48:], math.Float64bits(s.MissingValue))
	binary.LittleEndian.PutUint32(buf[56:], s.ChunkCountX)
	binary.LittleEndian.PutUint32(buf[60:], s.ChunkCountY)

	pos := RawGridHeaderSize
	for _, e := range s.CoarseTable {
		binary.LittleEndian.PutUint32(buf[pos:], e.Offset)
		binary.LittleEndian.PutUint32(buf[pos+4:], e.Length)
		pos += 8
	}
	return buf, nil
}

func UnmarshalRawGridSectionRoot(buf []byte) (*RawGridSection, error) {
	if len(buf) < RawGridHeaderSize {
		return nil, fmt.Errorf("raw grid: need %d bytes, got %d", RawGridHeaderSize, len(buf))
	}
	s := &RawGridSection{
		SchemaVersion:  buf[0],
		ChunkSizeLog2:  buf[1],
		CoarseSizeLog2: buf[2],
		Nx:             binary.LittleEndian.Uint32(buf[4:]),
		Ny:             binary.LittleEndian.Uint32(buf[8:]),
		Lat0:           math.Float64frombits(binary.LittleEndian.Uint64(buf[16:])),
		Lon0:           math.Float64frombits(binary.LittleEndian.Uint64(buf[24:])),
		DY:             math.Float64frombits(binary.LittleEndian.Uint64(buf[32:])),
		DX:             math.Float64frombits(binary.LittleEndian.Uint64(buf[40:])),
		MissingValue:   math.Float64frombits(binary.LittleEndian.Uint64(buf[48:])),
		ChunkCountX:    binary.LittleEndian.Uint32(buf[56:]),
		ChunkCountY:    binary.LittleEndian.Uint32(buf[60:]),
	}
	if s.SchemaVersion != RawGridSchemaVersion {
		return nil, fmt.Errorf("raw grid: unsupported schema version %d (want %d)",
			s.SchemaVersion, RawGridSchemaVersion)
	}
	if s.ChunkCountX == 0 || s.ChunkCountY == 0 {
		return nil, errors.New("raw grid: zero-sized chunk grid")
	}
	if s.CoarseSizeLog2 > 8 {
		return nil, fmt.Errorf("raw grid: coarse size log2 %d > 8", s.CoarseSizeLog2)
	}
	nCoarse := s.CoarseCount()
	if len(buf) < RawGridHeaderSize+8*nCoarse {
		return nil, fmt.Errorf("raw grid: root truncated (need %d, got %d)",
			RawGridHeaderSize+8*nCoarse, len(buf))
	}
	s.CoarseTable = make([]CoarseEntry, nCoarse)
	pos := RawGridHeaderSize
	for i := range nCoarse {
		s.CoarseTable[i] = CoarseEntry{
			Offset: binary.LittleEndian.Uint32(buf[pos:]),
			Length: binary.LittleEndian.Uint32(buf[pos+4:]),
		}
		pos += 8
	}
	return s, nil
}

// uncompressed varints; slices must be cell-row-major.
func MarshalFineIndex(chunkOffsets, chunkLengths []uint64) ([]byte, error) {
	if len(chunkOffsets) != len(chunkLengths) {
		return nil, fmt.Errorf("fine index: offsets/lengths mismatch (%d vs %d)",
			len(chunkOffsets), len(chunkLengths))
	}
	if len(chunkOffsets) == 0 {
		return nil, nil
	}
	out := make([]byte, 0, 4*len(chunkOffsets))
	for _, o := range chunkOffsets {
		out = varint.Append(out, o)
	}
	for _, l := range chunkLengths {
		out = varint.Append(out, l)
	}
	return out, nil
}

// expectedCount = cellW * cellH (use CoarseCellChunkExtent).
func UnmarshalFineIndex(buf []byte, expectedCount int) (chunkOffsets, chunkLengths []uint64, err error) {
	if expectedCount == 0 {
		return nil, nil, nil
	}
	chunkOffsets = make([]uint64, expectedCount)
	chunkLengths = make([]uint64, expectedCount)
	pos := 0
	for i := range expectedCount {
		v, used, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, nil, fmt.Errorf("fine index: chunk offset %d: %w", i, err)
		}
		pos += used
		chunkOffsets[i] = v
	}
	for i := range expectedCount {
		v, used, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, nil, fmt.Errorf("fine index: chunk length %d: %w", i, err)
		}
		pos += used
		chunkLengths[i] = v
	}
	return chunkOffsets, chunkLengths, nil
}
