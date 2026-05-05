package format

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestHeaderRoundtrip(t *testing.T) {
	h := &Header{
		FormatVersion:          FormatVersion,
		Flags:                  FlagColdStartInWindow | FlagTimeCatalogRegular,
		ActiveSnapshotOffset:   256,
		ActiveSnapshotLength:   2048,
		PreviousSnapshotOffset: 0,
		PreviousSnapshotLength: 0,
		FileLogicalEnd:         1 << 30,
		SnapshotGeneration:     7,
		InternalCompression:    CompZstd,
		TilePixelSizeLog2:      8,
		MinZoom:                0,
		MaxZoom:                6,
		BBoxLonMinE7:           -1800000000,
		BBoxLatMinE7:           -850000000,
		BBoxLonMaxE7:           1800000000,
		BBoxLatMaxE7:           850000000,
	}
	buf := MarshalHeader(h)
	if len(buf) != HeaderSize {
		t.Fatalf("header size %d, want %d", len(buf), HeaderSize)
	}
	got, err := UnmarshalHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	h.HeaderCRC = got.HeaderCRC
	if !reflect.DeepEqual(h, got) {
		t.Errorf("header roundtrip mismatch\nwant %+v\ngot  %+v", h, got)
	}
}

func TestHeaderBadMagic(t *testing.T) {
	buf := make([]byte, HeaderSize)
	copy(buf, []byte("BADMAGIC"))
	if _, err := UnmarshalHeader(buf); !errors.Is(err, ErrBadMagic) {
		t.Errorf("expected ErrBadMagic, got %v", err)
	}
}

func TestHeaderBadTail(t *testing.T) {
	h := &Header{FormatVersion: FormatVersion}
	buf := MarshalHeader(h)
	buf[252] ^= 0xFF
	if _, err := UnmarshalHeader(buf); !errors.Is(err, ErrBadHeaderTail) {
		t.Errorf("expected ErrBadHeaderTail, got %v", err)
	}
}

func TestHeaderBadCRC(t *testing.T) {
	h := &Header{FormatVersion: FormatVersion, ActiveSnapshotOffset: 256}
	buf := MarshalHeader(h)
	buf[100] ^= 0xFF
	if _, err := UnmarshalHeader(buf); !errors.Is(err, ErrBadHeaderCRC) {
		t.Errorf("expected ErrBadHeaderCRC, got %v", err)
	}
}

func TestCompressionRoundtrip(t *testing.T) {
	data := bytes.Repeat([]byte("compressible "), 100)
	for _, comp := range []InternalCompression{CompNone, CompGzip, CompZstd} {
		c, err := Compress(data, comp)
		if err != nil {
			t.Fatalf("Compress(%d): %v", comp, err)
		}
		d, err := Decompress(c, comp)
		if err != nil {
			t.Fatalf("Decompress(%d): %v", comp, err)
		}
		if !bytes.Equal(data, d) {
			t.Errorf("comp %d roundtrip mismatch", comp)
		}
	}
}

func TestVariableCatalogRoundtrip(t *testing.T) {
	in := []VariableEntry{
		{VariableID: 0, Name: "wind_v", Unit: "m/s",
			DefaultDType: 1, DefaultCodec: 0x03, DefaultPrecisionHint: 0.01,
			ColormapHint:           "RdBu",
			ValueMinObservedGlobal: -100, ValueMaxObservedGlobal: 100},
		{VariableID: 1, Name: "air_temperature", Unit: "K",
			DefaultDType: 1, DefaultCodec: 0x03, DefaultPrecisionHint: 0.002,
			ColormapHint:           "viridis",
			ValueMinObservedGlobal: 200, ValueMaxObservedGlobal: 330},
	}
	buf := MarshalVariableCatalog(in)
	got, err := UnmarshalVariableCatalog(buf, len(in))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].VariableID != 0 || got[0].Name != "wind_v" {
		t.Errorf("insertion order broken: id=%d name=%s", got[0].VariableID, got[0].Name)
	}
	if got[1].VariableID != 1 || got[1].Name != "air_temperature" {
		t.Errorf("insertion order broken: id=%d name=%s", got[1].VariableID, got[1].Name)
	}
	if got[1].DefaultPrecisionHint != 0.002 {
		t.Errorf("precision hint mismatch: got %g", got[1].DefaultPrecisionHint)
	}
}

func TestTimeCatalogRegular(t *testing.T) {
	in := &TimeCatalog{
		Regular:    true,
		StartMs:    1714694400000,
		IntervalMs: 3600_000,
		Count:      168,
	}
	buf := MarshalTimeCatalog(in)
	if len(buf) != 20 {
		t.Errorf("regular size %d, want 20", len(buf))
	}
	got, err := UnmarshalTimeCatalog(buf, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartMs != in.StartMs || got.IntervalMs != in.IntervalMs || got.Count != in.Count {
		t.Errorf("regular roundtrip mismatch: got %+v", got)
	}
	if got.TimeAt(5) != in.StartMs+5*in.IntervalMs {
		t.Errorf("TimeAt(5) wrong")
	}
}

func TestTimeCatalogIrregular(t *testing.T) {
	timestamps := []int64{}
	t0 := int64(1714694400000)
	for range 121 {
		timestamps = append(timestamps, t0)
		t0 += 3600_000
	}
	for i := 0; i < 80; i++ {
		t0 += 3 * 3600_000
		timestamps = append(timestamps, t0)
	}
	in := &TimeCatalog{Regular: false, Count: int64(len(timestamps)), TimestampsMs: timestamps}
	buf := MarshalTimeCatalog(in)
	got, err := UnmarshalTimeCatalog(buf, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.TimestampsMs, timestamps) {
		t.Errorf("irregular roundtrip mismatch")
	}
}

func TestSnapshotHeaderRoundtrip(t *testing.T) {
	in := &SnapshotHeader{
		SchemaVersion:       SnapshotSchemaVersion,
		SnapshotGeneration:  7,
		CreationTimeMs:      1714694400000,
		ReferenceTimeMs:     1714680000000,
		NumVariables:        5,
		NumTimeSteps:        168,
		NumBlocks:           840,
		VariableCatalogOff:  128,
		VariableCatalogLen:  400,
		TimeCatalogOff:      528,
		TimeCatalogLen:      20,
		BlockTableRootOff:   548,
		BlockTableRootLen:   40000,
		BlockTableLeavesOff: 0,
		BlockTableLeavesLen: 0,
		MetadataOff:         40548,
		MetadataLen:         500,
	}
	buf := MarshalSnapshotHeader(in)
	if len(buf) != SnapshotHeaderSize {
		t.Fatalf("size %d want %d", len(buf), SnapshotHeaderSize)
	}
	got, err := UnmarshalSnapshotHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Errorf("snapshot header mismatch\nwant %+v\ngot  %+v", in, got)
	}
}

func TestSnapshotTrailerRoundtrip(t *testing.T) {
	in := &SnapshotTrailer{SnapshotTotalLength: 41064, CRC32C: 0xDEADBEEF}
	buf := MarshalSnapshotTrailer(in)
	got, err := UnmarshalSnapshotTrailer(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Errorf("snapshot trailer mismatch")
	}
}

func TestSnapshotTrailerBadMagic(t *testing.T) {
	in := &SnapshotTrailer{SnapshotTotalLength: 100, CRC32C: 0}
	buf := MarshalSnapshotTrailer(in)
	buf[0] ^= 0xFF
	if _, err := UnmarshalSnapshotTrailer(buf); !errors.Is(err, ErrBadSnapshotMagic) {
		t.Errorf("expected ErrBadSnapshotMagic, got %v", err)
	}
}

func TestBlockHeaderRoundtrip(t *testing.T) {
	in := &BlockHeader{
		BlockFormatVersion:    BlockFormatVersion,
		BlockFlags:            BlockFlagHasLeafDirectories,
		RootDirectoryOffset:   64,
		RootDirectoryLength:   1234,
		LeafDirectoriesOffset: 1298,
		LeafDirectoriesLength: 5000,
		TileDataOffset:        6298,
		TileDataLength:        1 << 24,
		NumAddressedTiles:     5461,
		NumDirectoryEntries:   3000,
	}
	buf := MarshalBlockHeader(in)
	if len(buf) != BlockHeaderSize {
		t.Fatalf("size %d want %d", len(buf), BlockHeaderSize)
	}
	got, err := UnmarshalBlockHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Errorf("block header mismatch\nwant %+v\ngot  %+v", in, got)
	}
}

func TestBlockHeaderBadMagic(t *testing.T) {
	in := &BlockHeader{BlockFormatVersion: BlockFormatVersion}
	buf := MarshalBlockHeader(in)
	buf[0] ^= 0xFF
	if _, err := UnmarshalBlockHeader(buf); !errors.Is(err, ErrBadBlockMagic) {
		t.Errorf("expected ErrBadBlockMagic, got %v", err)
	}
}

func TestBlockTableRoundtrip(t *testing.T) {
	in := []BlockTableEntry{
		{VariableID: 0, TimeID: 0,
			BlockOffset: 4096, BlockLength: 1_000_000,
			DType: 1, Codec: 0x03, Scale: 0.01, Offset: -100, NoData: 0xFFFF,
			ValueMin: -50, ValueMax: 80,
			NumAddressedTiles: 5461, NumDirectoryEntries: 3000, NumTileContents: 2000},
		{VariableID: 0, TimeID: 1,
			BlockOffset: 1_004_096, BlockLength: 1_100_000,
			DType: 1, Codec: 0x03, Scale: 0.01, Offset: -100, NoData: 0xFFFF,
			ValueMin: -45, ValueMax: 82,
			NumAddressedTiles: 5461, NumDirectoryEntries: 3100, NumTileContents: 2100},
		{VariableID: 1, TimeID: 0,
			BlockOffset: 2_104_096, BlockLength: 800_000,
			DType: 0, Codec: 0x03, Scale: 1, Offset: 0, NoData: 0xFF,
			ValueMin: 0, ValueMax: 200,
			NumAddressedTiles: 5461, NumDirectoryEntries: 2500, NumTileContents: 1900},
	}
	buf := MarshalBlockTable(in)
	got, err := UnmarshalBlockTable(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("count mismatch: %d vs %d", len(got), len(in))
	}
	for i := range in {
		if !reflect.DeepEqual(in[i], got[i]) {
			t.Errorf("entry %d mismatch\nwant %+v\ngot  %+v", i, in[i], got[i])
		}
	}
}

func TestBlockTableLookup(t *testing.T) {
	entries := []BlockTableEntry{
		{VariableID: 0, TimeID: 0, BlockOffset: 100},
		{VariableID: 0, TimeID: 5, BlockOffset: 200},
		{VariableID: 1, TimeID: 0, BlockOffset: 300},
		{VariableID: 1, TimeID: 3, BlockOffset: 400},
	}
	if e, ok := LookupBlock(entries, 0, 5); !ok || e.BlockOffset != 200 {
		t.Errorf("lookup (0,5) failed: ok=%v off=%d", ok, e.BlockOffset)
	}
	if e, ok := LookupBlock(entries, 1, 3); !ok || e.BlockOffset != 400 {
		t.Errorf("lookup (1,3) failed")
	}
	if _, ok := LookupBlock(entries, 0, 1); ok {
		t.Errorf("lookup (0,1) should miss")
	}
	if _, ok := LookupBlock(entries, 2, 0); ok {
		t.Errorf("lookup (2,0) should miss")
	}
}

func TestFileTrailerRoundtrip(t *testing.T) {
	in := &FileTrailer{FileLogicalEnd: 1 << 30}
	buf := MarshalFileTrailer(in)
	got, err := UnmarshalFileTrailer(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.FileLogicalEnd != in.FileLogicalEnd {
		t.Errorf("file trailer mismatch")
	}
	buf[0] ^= 0xFF
	if _, err := UnmarshalFileTrailer(buf); err == nil {
		t.Errorf("expected error on corrupted trailer magic")
	}
}

func TestQuantParamsValidation(t *testing.T) {
	if !IsValidQuantParams(0.01, -100) {
		t.Errorf("expected valid")
	}
	if IsValidQuantParams(math.NaN(), 0) {
		t.Errorf("NaN scale should be invalid")
	}
}
