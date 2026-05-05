package format

import (
	"encoding/binary"
	"fmt"
)

const BlockHeaderSize = 64

const BlockMagic uint32 = 0xB10CC0DE

const BlockFormatVersion uint16 = 1

const (
	BlockFlagHasLeafDirectories uint16 = 1 << 0
)

type BlockHeader struct {
	BlockFormatVersion uint16
	BlockFlags         uint16

	RootDirectoryOffset   uint64
	RootDirectoryLength   uint32
	LeafDirectoriesOffset uint64
	LeafDirectoriesLength uint64
	TileDataOffset        uint64
	TileDataLength        uint64

	NumAddressedTiles   uint32
	NumDirectoryEntries uint32
	NumTileContents     uint32
}

func MarshalBlockHeader(h *BlockHeader) []byte {
	b := make([]byte, BlockHeaderSize)
	binary.LittleEndian.PutUint32(b[0:], BlockMagic)
	binary.LittleEndian.PutUint16(b[4:], h.BlockFormatVersion)
	binary.LittleEndian.PutUint16(b[6:], h.BlockFlags)
	binary.LittleEndian.PutUint64(b[8:], h.RootDirectoryOffset)
	binary.LittleEndian.PutUint32(b[16:], h.RootDirectoryLength)
	binary.LittleEndian.PutUint64(b[24:], h.LeafDirectoriesOffset)
	binary.LittleEndian.PutUint64(b[32:], h.LeafDirectoriesLength)
	binary.LittleEndian.PutUint64(b[40:], h.TileDataOffset)
	binary.LittleEndian.PutUint64(b[48:], h.TileDataLength)
	binary.LittleEndian.PutUint32(b[56:], h.NumAddressedTiles)
	binary.LittleEndian.PutUint32(b[60:], h.NumDirectoryEntries)
	return b
}

func UnmarshalBlockHeader(b []byte) (*BlockHeader, error) {
	if len(b) < BlockHeaderSize {
		return nil, fmt.Errorf("block header: need %d bytes, got %d", BlockHeaderSize, len(b))
	}
	if got := binary.LittleEndian.Uint32(b[0:]); got != BlockMagic {
		return nil, fmt.Errorf("%w: got 0x%08X", ErrBadBlockMagic, got)
	}
	return &BlockHeader{
		BlockFormatVersion:    binary.LittleEndian.Uint16(b[4:]),
		BlockFlags:            binary.LittleEndian.Uint16(b[6:]),
		RootDirectoryOffset:   binary.LittleEndian.Uint64(b[8:]),
		RootDirectoryLength:   binary.LittleEndian.Uint32(b[16:]),
		LeafDirectoriesOffset: binary.LittleEndian.Uint64(b[24:]),
		LeafDirectoriesLength: binary.LittleEndian.Uint64(b[32:]),
		TileDataOffset:        binary.LittleEndian.Uint64(b[40:]),
		TileDataLength:        binary.LittleEndian.Uint64(b[48:]),
		NumAddressedTiles:     binary.LittleEndian.Uint32(b[56:]),
		NumDirectoryEntries:   binary.LittleEndian.Uint32(b[60:]),
	}, nil
}
