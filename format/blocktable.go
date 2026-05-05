package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/hstin-de/wmtiles/varint"
)

type BlockTableEntry struct {
	VariableID    uint16
	TimeID        uint32
	IsLeafPointer bool

	BlockOffset uint64
	BlockLength uint64

	DType  uint8
	Codec  uint8
	Scale  float64
	Offset float64
	NoData uint32

	ValueMin float64
	ValueMax float64

	NumAddressedTiles   uint64
	NumDirectoryEntries uint64
	NumTileContents     uint64
}

func (e *BlockTableEntry) CompositeKey() uint64 {
	return (uint64(e.VariableID) << 32) | uint64(e.TimeID)
}

func SplitCompositeKey(k uint64) (uint16, uint32) {
	return uint16(k >> 32), uint32(k & 0xFFFFFFFF)
}

// columnar layout: each field stored in its own contiguous run rather than
// row-by-row, so zstd sees long sequences of similar values per column
func MarshalBlockTable(entries []BlockTableEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CompositeKey() < entries[j].CompositeKey()
	})

	buf := make([]byte, 0, 64+len(entries)*48)
	buf = varint.Append(buf, uint64(len(entries)))

	var prev uint64
	for _, e := range entries {
		k := e.CompositeKey()
		buf = varint.Append(buf, k-prev)
		prev = k
	}

	for _, e := range entries {
		if e.IsLeafPointer {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	}

	for _, e := range entries {
		buf = varint.Append(buf, e.BlockOffset)
	}
	for _, e := range entries {
		buf = varint.Append(buf, e.BlockLength)
	}

	for _, e := range entries {
		buf = append(buf, e.DType)
	}
	for _, e := range entries {
		buf = append(buf, e.Codec)
	}
	for _, e := range entries {
		buf = appendF64(buf, e.Scale)
	}
	for _, e := range entries {
		buf = appendF64(buf, e.Offset)
	}
	for _, e := range entries {
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], e.NoData)
		buf = append(buf, tmp[:]...)
	}
	for _, e := range entries {
		buf = appendF64(buf, e.ValueMin)
	}
	for _, e := range entries {
		buf = appendF64(buf, e.ValueMax)
	}

	for _, e := range entries {
		buf = varint.Append(buf, e.NumAddressedTiles)
	}
	for _, e := range entries {
		buf = varint.Append(buf, e.NumDirectoryEntries)
	}
	for _, e := range entries {
		buf = varint.Append(buf, e.NumTileContents)
	}

	return buf
}

func UnmarshalBlockTable(buf []byte) ([]BlockTableEntry, error) {
	pos := 0
	count, n, err := varint.Read(buf[pos:])
	if err != nil {
		return nil, fmt.Errorf("blocktable: count: %w", err)
	}
	pos += n
	if count == 0 {
		return nil, nil
	}
	entries := make([]BlockTableEntry, count)

	var prev uint64
	for i := range count {
		delta, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: key %d: %w", i, err)
		}
		pos += n
		prev += delta
		entries[i].VariableID, entries[i].TimeID = SplitCompositeKey(prev)
	}

	if pos+int(count) > len(buf) {
		return nil, errors.New("blocktable: truncated leaf flags")
	}
	for i := range count {
		entries[i].IsLeafPointer = buf[pos+int(i)] != 0
	}
	pos += int(count)

	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: offset %d: %w", i, err)
		}
		pos += n
		entries[i].BlockOffset = v
	}
	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: length %d: %w", i, err)
		}
		pos += n
		entries[i].BlockLength = v
	}

	if pos+int(count)*2 > len(buf) {
		return nil, errors.New("blocktable: truncated dtype/codec")
	}
	for i := range count {
		entries[i].DType = buf[pos+int(i)]
	}
	pos += int(count)
	for i := range count {
		entries[i].Codec = buf[pos+int(i)]
	}
	pos += int(count)

	if pos+int(count)*8 > len(buf) {
		return nil, errors.New("blocktable: truncated scale")
	}
	for i := range count {
		entries[i].Scale = readF64(buf[pos+int(i)*8:])
	}
	pos += int(count) * 8

	if pos+int(count)*8 > len(buf) {
		return nil, errors.New("blocktable: truncated offset")
	}
	for i := range count {
		entries[i].Offset = readF64(buf[pos+int(i)*8:])
	}
	pos += int(count) * 8

	if pos+int(count)*4 > len(buf) {
		return nil, errors.New("blocktable: truncated nodata")
	}
	for i := range count {
		entries[i].NoData = binary.LittleEndian.Uint32(buf[pos+int(i)*4:])
	}
	pos += int(count) * 4

	if pos+int(count)*8 > len(buf) {
		return nil, errors.New("blocktable: truncated vmin")
	}
	for i := range count {
		entries[i].ValueMin = readF64(buf[pos+int(i)*8:])
	}
	pos += int(count) * 8

	if pos+int(count)*8 > len(buf) {
		return nil, errors.New("blocktable: truncated vmax")
	}
	for i := range count {
		entries[i].ValueMax = readF64(buf[pos+int(i)*8:])
	}
	pos += int(count) * 8

	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: addressed %d: %w", i, err)
		}
		pos += n
		entries[i].NumAddressedTiles = v
	}
	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: dir entries %d: %w", i, err)
		}
		pos += n
		entries[i].NumDirectoryEntries = v
	}
	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, fmt.Errorf("blocktable: contents %d: %w", i, err)
		}
		pos += n
		entries[i].NumTileContents = v
	}

	return entries, nil
}

func LookupBlock(entries []BlockTableEntry, variableID uint16, timeID uint32) (BlockTableEntry, bool) {
	if len(entries) == 0 {
		return BlockTableEntry{}, false
	}
	target := (uint64(variableID) << 32) | uint64(timeID)
	idx := sort.Search(len(entries), func(i int) bool {
		return entries[i].CompositeKey() >= target
	})
	if idx < len(entries) && entries[idx].CompositeKey() == target {
		return entries[idx], true
	}
	// no exact hit: a leaf pointer to the left covers a key range that may include target
	if idx > 0 && entries[idx-1].IsLeafPointer {
		return entries[idx-1], true
	}
	return BlockTableEntry{}, false
}

func IsValidQuantParams(scale, offset float64) bool {
	return !math.IsNaN(scale) && !math.IsNaN(offset)
}
