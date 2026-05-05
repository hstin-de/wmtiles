package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/hstin-de/wmtiles/varint"
)

const SnapshotHeaderSize = 128

const SnapshotSchemaVersion uint64 = 1

const SnapshotTrailerSize = 16

const SnapshotTrailerMagic uint32 = 0xC0FFEE42

type SnapshotHeader struct {
	SchemaVersion       uint64
	SnapshotGeneration  uint64
	CreationTimeMs      int64
	ReferenceTimeMs     int64
	NumVariables        uint16
	NumTimeSteps        uint32
	NumBlocks           uint64
	VariableCatalogOff  uint64
	VariableCatalogLen  uint64
	TimeCatalogOff      uint64
	TimeCatalogLen      uint64
	BlockTableRootOff   uint64
	BlockTableRootLen   uint64
	BlockTableLeavesOff uint64
	BlockTableLeavesLen uint64
	MetadataOff         uint64
	MetadataLen         uint64
}

func MarshalSnapshotHeader(h *SnapshotHeader) []byte {
	b := make([]byte, SnapshotHeaderSize)
	binary.LittleEndian.PutUint64(b[0:], h.SchemaVersion)
	binary.LittleEndian.PutUint64(b[8:], h.SnapshotGeneration)
	binary.LittleEndian.PutUint64(b[16:], uint64(h.CreationTimeMs))
	binary.LittleEndian.PutUint64(b[24:], uint64(h.ReferenceTimeMs))
	binary.LittleEndian.PutUint16(b[32:], h.NumVariables)
	binary.LittleEndian.PutUint32(b[34:], h.NumTimeSteps)
	binary.LittleEndian.PutUint64(b[40:], h.NumBlocks)
	binary.LittleEndian.PutUint64(b[48:], h.VariableCatalogOff)
	binary.LittleEndian.PutUint64(b[56:], h.VariableCatalogLen)
	binary.LittleEndian.PutUint64(b[64:], h.TimeCatalogOff)
	binary.LittleEndian.PutUint64(b[72:], h.TimeCatalogLen)
	binary.LittleEndian.PutUint64(b[80:], h.BlockTableRootOff)
	binary.LittleEndian.PutUint64(b[88:], h.BlockTableRootLen)
	binary.LittleEndian.PutUint64(b[96:], h.BlockTableLeavesOff)
	binary.LittleEndian.PutUint64(b[104:], h.BlockTableLeavesLen)
	binary.LittleEndian.PutUint64(b[112:], h.MetadataOff)
	binary.LittleEndian.PutUint64(b[120:], h.MetadataLen)
	return b
}

func UnmarshalSnapshotHeader(b []byte) (*SnapshotHeader, error) {
	if len(b) < SnapshotHeaderSize {
		return nil, fmt.Errorf("snapshot header: need %d bytes, got %d", SnapshotHeaderSize, len(b))
	}
	return &SnapshotHeader{
		SchemaVersion:       binary.LittleEndian.Uint64(b[0:]),
		SnapshotGeneration:  binary.LittleEndian.Uint64(b[8:]),
		CreationTimeMs:      int64(binary.LittleEndian.Uint64(b[16:])),
		ReferenceTimeMs:     int64(binary.LittleEndian.Uint64(b[24:])),
		NumVariables:        binary.LittleEndian.Uint16(b[32:]),
		NumTimeSteps:        binary.LittleEndian.Uint32(b[34:]),
		NumBlocks:           binary.LittleEndian.Uint64(b[40:]),
		VariableCatalogOff:  binary.LittleEndian.Uint64(b[48:]),
		VariableCatalogLen:  binary.LittleEndian.Uint64(b[56:]),
		TimeCatalogOff:      binary.LittleEndian.Uint64(b[64:]),
		TimeCatalogLen:      binary.LittleEndian.Uint64(b[72:]),
		BlockTableRootOff:   binary.LittleEndian.Uint64(b[80:]),
		BlockTableRootLen:   binary.LittleEndian.Uint64(b[88:]),
		BlockTableLeavesOff: binary.LittleEndian.Uint64(b[96:]),
		BlockTableLeavesLen: binary.LittleEndian.Uint64(b[104:]),
		MetadataOff:         binary.LittleEndian.Uint64(b[112:]),
		MetadataLen:         binary.LittleEndian.Uint64(b[120:]),
	}, nil
}

type SnapshotTrailer struct {
	SnapshotTotalLength uint64
	CRC32C              uint32
}

func MarshalSnapshotTrailer(t *SnapshotTrailer) []byte {
	b := make([]byte, SnapshotTrailerSize)
	binary.LittleEndian.PutUint32(b[0:], SnapshotTrailerMagic)
	binary.LittleEndian.PutUint64(b[4:], t.SnapshotTotalLength)
	binary.LittleEndian.PutUint32(b[12:], t.CRC32C)
	return b
}

func UnmarshalSnapshotTrailer(b []byte) (*SnapshotTrailer, error) {
	if len(b) < SnapshotTrailerSize {
		return nil, fmt.Errorf("snapshot trailer: need %d bytes, got %d", SnapshotTrailerSize, len(b))
	}
	if got := binary.LittleEndian.Uint32(b[0:]); got != SnapshotTrailerMagic {
		return nil, fmt.Errorf("%w: got 0x%08X", ErrBadSnapshotMagic, got)
	}
	return &SnapshotTrailer{
		SnapshotTotalLength: binary.LittleEndian.Uint64(b[4:]),
		CRC32C:              binary.LittleEndian.Uint32(b[12:]),
	}, nil
}

type VariableEntry struct {
	VariableID             uint16
	Name                   string
	Unit                   string
	DefaultDType           uint8
	DefaultCodec           uint8
	DefaultPrecisionHint   float64
	ColormapHint           string
	ValueMinObservedGlobal float64
	ValueMaxObservedGlobal float64
}

func MarshalVariableCatalog(vars []VariableEntry) []byte {
	sort.Slice(vars, func(i, j int) bool { return vars[i].VariableID < vars[j].VariableID })

	var buf []byte
	for _, v := range vars {
		buf = append(buf, byte(v.VariableID), byte(v.VariableID>>8))
		buf = append(buf, byte(len(v.Name)))
		buf = append(buf, v.Name...)
		buf = append(buf, byte(len(v.Unit)))
		buf = append(buf, v.Unit...)
		buf = append(buf, v.DefaultDType)
		buf = append(buf, v.DefaultCodec)
		buf = appendF64(buf, v.DefaultPrecisionHint)
		buf = append(buf, byte(len(v.ColormapHint)))
		buf = append(buf, v.ColormapHint...)
		buf = appendF64(buf, v.ValueMinObservedGlobal)
		buf = appendF64(buf, v.ValueMaxObservedGlobal)
	}
	return buf
}

func UnmarshalVariableCatalog(buf []byte, count int) ([]VariableEntry, error) {
	out := make([]VariableEntry, 0, count)
	pos := 0
	for range count {
		if pos+2 > len(buf) {
			return nil, errors.New("catalog: truncated id")
		}
		v := VariableEntry{}
		v.VariableID = binary.LittleEndian.Uint16(buf[pos:])
		pos += 2
		if pos+1 > len(buf) {
			return nil, errors.New("catalog: truncated name length")
		}
		nl := int(buf[pos])
		pos++
		if pos+nl > len(buf) {
			return nil, errors.New("catalog: truncated name")
		}
		v.Name = string(buf[pos : pos+nl])
		pos += nl
		if pos+1 > len(buf) {
			return nil, errors.New("catalog: truncated unit length")
		}
		ul := int(buf[pos])
		pos++
		if pos+ul > len(buf) {
			return nil, errors.New("catalog: truncated unit")
		}
		v.Unit = string(buf[pos : pos+ul])
		pos += ul
		if pos+2+8+1 > len(buf) {
			return nil, errors.New("catalog: truncated dtype/codec/precision")
		}
		v.DefaultDType = buf[pos]
		v.DefaultCodec = buf[pos+1]
		pos += 2
		v.DefaultPrecisionHint = readF64(buf[pos:])
		pos += 8
		cl := int(buf[pos])
		pos++
		if pos+cl > len(buf) {
			return nil, errors.New("catalog: truncated colormap")
		}
		v.ColormapHint = string(buf[pos : pos+cl])
		pos += cl
		if pos+16 > len(buf) {
			return nil, errors.New("catalog: truncated min/max")
		}
		v.ValueMinObservedGlobal = readF64(buf[pos:])
		pos += 8
		v.ValueMaxObservedGlobal = readF64(buf[pos:])
		pos += 8
		out = append(out, v)
	}
	return out, nil
}

type TimeCatalog struct {
	Regular      bool
	StartMs      int64
	IntervalMs   int64
	Count        int64
	TimestampsMs []int64
}

func MarshalTimeCatalog(t *TimeCatalog) []byte {
	if t.Regular {
		buf := make([]byte, 20)
		binary.LittleEndian.PutUint64(buf[0:], uint64(t.StartMs))
		binary.LittleEndian.PutUint64(buf[8:], uint64(t.IntervalMs))
		binary.LittleEndian.PutUint32(buf[16:], uint32(t.Count))
		return buf
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(t.TimestampsMs)))
	if len(t.TimestampsMs) == 0 {
		return buf
	}
	// irregular times: zigzag + delta varint, so monotonic timestamps shrink to a few bytes each
	first := zigzag64(t.TimestampsMs[0])
	buf = varint.Append(buf, first)
	prev := t.TimestampsMs[0]
	for _, ts := range t.TimestampsMs[1:] {
		buf = varint.Append(buf, zigzag64(ts-prev))
		prev = ts
	}
	return buf
}

func UnmarshalTimeCatalog(buf []byte, regular bool) (*TimeCatalog, error) {
	t := &TimeCatalog{Regular: regular}
	if regular {
		if len(buf) < 20 {
			return nil, fmt.Errorf("time catalog: regular mode needs 20 bytes, got %d", len(buf))
		}
		t.StartMs = int64(binary.LittleEndian.Uint64(buf[0:]))
		t.IntervalMs = int64(binary.LittleEndian.Uint64(buf[8:]))
		t.Count = int64(binary.LittleEndian.Uint32(buf[16:]))
		return t, nil
	}
	if len(buf) < 4 {
		return nil, errors.New("time catalog: missing count")
	}
	n := binary.LittleEndian.Uint32(buf[:4])
	t.Count = int64(n)
	if n == 0 {
		return t, nil
	}
	t.TimestampsMs = make([]int64, n)
	pos := 4
	first, used, err := varint.Read(buf[pos:])
	if err != nil {
		return nil, err
	}
	pos += used
	t.TimestampsMs[0] = unzigzag64(first)
	prev := t.TimestampsMs[0]
	for i := uint32(1); i < n; i++ {
		v, used, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, err
		}
		pos += used
		prev += unzigzag64(v)
		t.TimestampsMs[i] = prev
	}
	return t, nil
}

func (tc *TimeCatalog) TimeAt(t uint32) int64 {
	if tc.Regular {
		return tc.StartMs + int64(t)*tc.IntervalMs
	}
	if int(t) < len(tc.TimestampsMs) {
		return tc.TimestampsMs[t]
	}
	return 0
}

func appendF64(b []byte, v float64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(v))
	return append(b, tmp[:]...)
}

func readF64(b []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

func zigzag64(v int64) uint64   { return uint64((v << 1) ^ (v >> 63)) }
func unzigzag64(v uint64) int64 { return int64((v >> 1) ^ -(v & 1)) }
