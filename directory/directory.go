package directory

import (
	"errors"
	"sort"

	"github.com/hstin-de/wmtiles/varint"
)

type Entry struct {
	TileID    uint64
	RunLength uint32
	Length    uint32
	Offset    uint64
}

// RunLength==0 doubles as the leaf-pointer flag; data entries always have runs ≥ 1
func (e Entry) IsLeafPointer() bool { return e.RunLength == 0 }

func Encode(entries []Entry) []byte {
	var buf []byte

	buf = varint.Append(buf, uint64(len(entries)))

	var prevID uint64
	for _, e := range entries {
		delta := e.TileID - prevID
		buf = varint.Append(buf, delta)
		prevID = e.TileID
	}

	for _, e := range entries {
		buf = varint.Append(buf, uint64(e.RunLength))
	}

	for _, e := range entries {
		buf = varint.Append(buf, uint64(e.Length))
	}

	// implicit-offset compression: 0 means "right after previous entry", saving a varint
	// per dense run; otherwise store offset+1 so 0 stays distinguishable
	for i, e := range entries {
		if i > 0 && e.Offset == entries[i-1].Offset+uint64(entries[i-1].Length) {
			buf = varint.Append(buf, 0)
		} else {
			buf = varint.Append(buf, e.Offset+1)
		}
	}

	return buf
}

func Decode(buf []byte) ([]Entry, error) {
	pos := 0
	count, n, err := varint.Read(buf[pos:])
	if err != nil {
		return nil, err
	}
	pos += n
	if count == 0 {
		return nil, nil
	}
	entries := make([]Entry, count)

	var prevID uint64
	for i := range count {
		delta, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		entries[i].TileID = prevID + delta
		prevID = entries[i].TileID
	}

	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		entries[i].RunLength = uint32(v)
	}

	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		entries[i].Length = uint32(v)
	}

	for i := range count {
		v, n, err := varint.Read(buf[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		if v == 0 {
			if i == 0 {
				return nil, errors.New("directory: first offset cannot be implicit")
			}
			entries[i].Offset = entries[i-1].Offset + uint64(entries[i-1].Length)
		} else {
			entries[i].Offset = v - 1
		}
	}

	return entries, nil
}

func FindTile(entries []Entry, target uint64) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	idx := sort.Search(len(entries), func(i int) bool {
		return entries[i].TileID > target
	}) - 1
	if idx < 0 {
		return Entry{}, false
	}
	e := entries[idx]
	if e.IsLeafPointer() {
		return e, true
	}
	if target < e.TileID+uint64(e.RunLength) {
		return e, true
	}
	return Entry{}, false
}

type Builder struct {
	entries []Entry
}

func (b *Builder) Append(tileID uint64, length uint32, offset uint64) {
	b.AppendRun(tileID, 1, length, offset)
}

func (b *Builder) AppendRun(tileID uint64, runLength uint32, length uint32, offset uint64) {
	if runLength == 0 {
		return
	}
	// dedup-friendly: adjacent tiles pointing at the same blob (Hilbert order keeps
	// these clustered) get folded into one run instead of N entries
	if n := len(b.entries); n > 0 {
		last := &b.entries[n-1]
		if !last.IsLeafPointer() &&
			last.Offset == offset &&
			last.Length == length &&
			last.TileID+uint64(last.RunLength) == tileID {
			last.RunLength += runLength
			return
		}
	}
	b.entries = append(b.entries, Entry{
		TileID:    tileID,
		RunLength: runLength,
		Length:    length,
		Offset:    offset,
	})
}

func (b *Builder) Entries() []Entry { return b.entries }

func (b *Builder) Len() int { return len(b.entries) }
