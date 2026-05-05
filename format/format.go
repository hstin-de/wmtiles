package format

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/klauspost/compress/zstd"
)

var Magic = [8]byte{'W', 'M', 'T', 'I', 'L', 'E', 'S', 0}

const FormatVersion uint16 = 1

const HeaderSize = 256

// last 4 bytes of the header: torn-write sentinel, must survive a fsync
const HeaderMagicTail uint32 = 0xE7E7DEAD

// first-RTT prefetch window: header + (usually) the active snapshot in one Range request
const ColdStartBudget = 65536

const MaxBlockRootBytes = 16384 - BlockHeaderSize

const MaxBlockTableRootBytes = 16384

// Castagnoli: hardware-accelerated on SSE4.2/ARMv8, matches iSCSI/Btrfs
var CRC32CTable = crc32.MakeTable(crc32.Castagnoli)

func CRC32C(b []byte) uint32 { return crc32.Checksum(b, CRC32CTable) }

type InternalCompression uint8

const (
	CompNone InternalCompression = 0
	CompGzip InternalCompression = 1
	CompZstd InternalCompression = 2
)

const (
	FlagColdStartInWindow   uint16 = 1 << 0
	FlagHasPreviousSnapshot uint16 = 1 << 1
	FlagTimeCatalogRegular  uint16 = 1 << 2
)

type Header struct {
	FormatVersion uint16
	Flags         uint16
	HeaderCRC     uint32

	ActiveSnapshotOffset   uint64
	ActiveSnapshotLength   uint64
	PreviousSnapshotOffset uint64
	PreviousSnapshotLength uint64

	FileLogicalEnd uint64

	SnapshotGeneration uint64

	InternalCompression InternalCompression
	TilePixelSizeLog2   uint8
	MinZoom             uint8
	MaxZoom             uint8

	BBoxLonMinE7 int32
	BBoxLatMinE7 int32
	BBoxLonMaxE7 int32
	BBoxLatMaxE7 int32
}

func MarshalHeader(h *Header) []byte {
	b := make([]byte, HeaderSize)
	copy(b[0:8], Magic[:])
	binary.LittleEndian.PutUint16(b[8:], h.FormatVersion)
	binary.LittleEndian.PutUint16(b[10:], h.Flags)
	binary.LittleEndian.PutUint64(b[16:], h.ActiveSnapshotOffset)
	binary.LittleEndian.PutUint64(b[24:], h.ActiveSnapshotLength)
	binary.LittleEndian.PutUint64(b[32:], h.PreviousSnapshotOffset)
	binary.LittleEndian.PutUint64(b[40:], h.PreviousSnapshotLength)
	binary.LittleEndian.PutUint64(b[48:], h.FileLogicalEnd)
	binary.LittleEndian.PutUint64(b[56:], h.SnapshotGeneration)
	b[64] = byte(h.InternalCompression)
	b[65] = h.TilePixelSizeLog2
	b[66] = h.MinZoom
	b[67] = h.MaxZoom
	binary.LittleEndian.PutUint32(b[68:], uint32(h.BBoxLonMinE7))
	binary.LittleEndian.PutUint32(b[72:], uint32(h.BBoxLatMinE7))
	binary.LittleEndian.PutUint32(b[76:], uint32(h.BBoxLonMaxE7))
	binary.LittleEndian.PutUint32(b[80:], uint32(h.BBoxLatMaxE7))
	binary.LittleEndian.PutUint32(b[252:], HeaderMagicTail)

	// covers everything between the CRC slot and the tail magic; both are excluded
	crc := CRC32C(b[16:252])
	binary.LittleEndian.PutUint32(b[12:], crc)
	return b
}

func UnmarshalHeader(b []byte) (*Header, error) {
	if len(b) < HeaderSize {
		return nil, fmt.Errorf("header: need %d bytes, got %d", HeaderSize, len(b))
	}
	if !bytes.Equal(b[0:8], Magic[:]) {
		return nil, ErrBadMagic
	}
	tail := binary.LittleEndian.Uint32(b[252:])
	if tail != HeaderMagicTail {
		return nil, fmt.Errorf("%w: got 0x%08X", ErrBadHeaderTail, tail)
	}
	storedCRC := binary.LittleEndian.Uint32(b[12:])
	h := &Header{
		FormatVersion:          binary.LittleEndian.Uint16(b[8:]),
		Flags:                  binary.LittleEndian.Uint16(b[10:]),
		HeaderCRC:              storedCRC,
		ActiveSnapshotOffset:   binary.LittleEndian.Uint64(b[16:]),
		ActiveSnapshotLength:   binary.LittleEndian.Uint64(b[24:]),
		PreviousSnapshotOffset: binary.LittleEndian.Uint64(b[32:]),
		PreviousSnapshotLength: binary.LittleEndian.Uint64(b[40:]),
		FileLogicalEnd:         binary.LittleEndian.Uint64(b[48:]),
		SnapshotGeneration:     binary.LittleEndian.Uint64(b[56:]),
		InternalCompression:    InternalCompression(b[64]),
		TilePixelSizeLog2:      b[65],
		MinZoom:                b[66],
		MaxZoom:                b[67],
		BBoxLonMinE7:           int32(binary.LittleEndian.Uint32(b[68:])),
		BBoxLatMinE7:           int32(binary.LittleEndian.Uint32(b[72:])),
		BBoxLonMaxE7:           int32(binary.LittleEndian.Uint32(b[76:])),
		BBoxLatMaxE7:           int32(binary.LittleEndian.Uint32(b[80:])),
	}
	if want := CRC32C(b[16:252]); want != storedCRC {
		// return the partial header anyway so callers can still find previous_snapshot
		return h, fmt.Errorf("%w: stored=0x%08X computed=0x%08X", ErrBadHeaderCRC, storedCRC, want)
	}
	return h, nil
}

type FileTrailer struct {
	FileLogicalEnd uint64
}

const FileTrailerMagic uint32 = 0xEEEFFFFF

const FileTrailerSize = 16

func MarshalFileTrailer(t *FileTrailer) []byte {
	b := make([]byte, FileTrailerSize)
	binary.LittleEndian.PutUint32(b[0:], FileTrailerMagic)
	binary.LittleEndian.PutUint64(b[4:], t.FileLogicalEnd)
	return b
}

func UnmarshalFileTrailer(b []byte) (*FileTrailer, error) {
	if len(b) < FileTrailerSize {
		return nil, fmt.Errorf("file trailer: need %d bytes, got %d", FileTrailerSize, len(b))
	}
	if got := binary.LittleEndian.Uint32(b[0:]); got != FileTrailerMagic {
		return nil, fmt.Errorf("file trailer: bad magic 0x%08X", got)
	}
	return &FileTrailer{
		FileLogicalEnd: binary.LittleEndian.Uint64(b[4:]),
	}, nil
}

func Compress(data []byte, comp InternalCompression) ([]byte, error) {
	switch comp {
	case CompNone:
		return data, nil
	case CompGzip:
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case CompZstd:
		zw, err := zstd.NewWriter(nil)
		if err != nil {
			return nil, err
		}
		defer zw.Close()
		return zw.EncodeAll(data, nil), nil
	}
	return nil, fmt.Errorf("unknown internal compression %d", comp)
}

func Decompress(data []byte, comp InternalCompression) ([]byte, error) {
	switch comp {
	case CompNone:
		return data, nil
	case CompGzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case CompZstd:
		zr, err := zstd.NewReader(nil)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return zr.DecodeAll(data, nil)
	}
	return nil, fmt.Errorf("unknown internal compression %d", comp)
}

var (
	ErrBadMagic         = errors.New("header: bad magic")
	ErrBadHeaderTail    = errors.New("header: bad magic tail")
	ErrBadHeaderCRC     = errors.New("header: CRC mismatch")
	ErrBadSnapshotMagic = errors.New("snapshot: bad trailer magic")
	ErrBadSnapshotCRC   = errors.New("snapshot: CRC mismatch")
	ErrBadBlockMagic    = errors.New("block: bad magic")
)
