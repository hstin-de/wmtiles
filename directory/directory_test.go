package directory

import (
	"reflect"
	"testing"
)

func TestEncodeDecodeEmpty(t *testing.T) {
	buf := Encode(nil)
	got, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d entries", len(got))
	}
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	entries := []Entry{
		{TileID: 0, RunLength: 1, Length: 100, Offset: 0},
		{TileID: 1, RunLength: 3, Length: 50, Offset: 100},
		{TileID: 4, RunLength: 1, Length: 200, Offset: 150},
		{TileID: 10, RunLength: 1, Length: 75, Offset: 9999},
		{TileID: 20, RunLength: 0, Length: 1024, Offset: 0},
	}
	buf := Encode(entries)
	got, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entries, got) {
		t.Errorf("roundtrip mismatch\nwant: %+v\ngot:  %+v", entries, got)
	}
}

func TestPlusOneZeroOffsetTrick(t *testing.T) {
	entries := []Entry{
		{TileID: 0, RunLength: 1, Length: 100, Offset: 0},
		{TileID: 1, RunLength: 1, Length: 50, Offset: 100},
	}
	buf := Encode(entries)
	got, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Offset != 100 {
		t.Errorf("contiguous offset reconstruction failed: %d", got[1].Offset)
	}

	entries[1].Offset = 999
	buf = Encode(entries)
	got, err = Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Offset != 999 {
		t.Errorf("non-contiguous offset reconstruction failed: %d", got[1].Offset)
	}
}

func TestFindTile(t *testing.T) {
	entries := []Entry{
		{TileID: 0, RunLength: 1, Length: 100, Offset: 0},
		{TileID: 1, RunLength: 3, Length: 50, Offset: 100},
		{TileID: 10, RunLength: 1, Length: 75, Offset: 200},
	}
	cases := []struct {
		target uint64
		hit    bool
		offset uint64
	}{
		{0, true, 0},
		{1, true, 100},
		{2, true, 100},
		{3, true, 100},
		{4, false, 0},
		{9, false, 0},
		{10, true, 200},
		{11, false, 0},
		{100, false, 0},
	}
	for _, c := range cases {
		e, ok := FindTile(entries, c.target)
		if ok != c.hit {
			t.Errorf("FindTile(%d): got hit=%v, want %v", c.target, ok, c.hit)
		}
		if c.hit && e.Offset != c.offset {
			t.Errorf("FindTile(%d): offset %d, want %d", c.target, e.Offset, c.offset)
		}
	}
}

func TestBuilderRLECoalesces(t *testing.T) {
	var b Builder
	b.Append(10, 100, 5000)
	b.Append(11, 100, 5000)
	b.Append(12, 100, 5000)
	if b.Len() != 1 {
		t.Errorf("expected RLE coalescing into 1 entry, got %d", b.Len())
	}
	e := b.Entries()[0]
	if e.RunLength != 3 {
		t.Errorf("expected RunLength=3, got %d", e.RunLength)
	}
}

func TestBuilderRLEBreaksOnDifferentBlob(t *testing.T) {
	var b Builder
	b.Append(0, 100, 0)
	b.Append(1, 100, 100)
	if b.Len() != 2 {
		t.Errorf("expected 2 entries, got %d", b.Len())
	}
}

func TestBuilderRLEBreaksOnGap(t *testing.T) {
	var b Builder
	b.Append(0, 100, 0)
	b.Append(2, 100, 0)
	if b.Len() != 2 {
		t.Errorf("expected 2 entries (gap breaks RLE), got %d", b.Len())
	}
}

func TestLargeDirectory(t *testing.T) {
	const n = 100_000
	entries := make([]Entry, n)
	off := uint64(0)
	for i := range entries {
		entries[i] = Entry{
			TileID:    uint64(i),
			RunLength: 1,
			Length:    100,
			Offset:    off,
		}
		off += 100
	}
	buf := Encode(entries)
	got, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("decoded %d, want %d", len(got), n)
	}
	if len(buf) > 5*n {
		t.Errorf("directory raw size too big: %d bytes for %d entries", len(buf), n)
	}
}
