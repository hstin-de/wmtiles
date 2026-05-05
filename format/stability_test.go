package format_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/reader"
)

func TestStabilityVectors(t *testing.T) {
	cases := []struct {
		file        string
		variables   []string
		generation  uint64
		hasPrev     bool
		coldInWin   bool
		variableMin map[string]float64
	}{
		{
			file:       "minimal.wmt",
			variables:  []string{"temp"},
			generation: 0,
			hasPrev:    false,
			coldInWin:  true,
		},
		{
			file:       "extended.wmt",
			variables:  []string{"temp", "wind", "precip"},
			generation: 2,
			hasPrev:    true,
			coldInWin:  false,
		},
		{
			file:       "compacted.wmt",
			variables:  []string{"temp", "wind", "precip"},
			generation: 0,
			hasPrev:    false,
			coldInWin:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join("testdata", c.file)
			r, err := reader.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer r.Close()

			if r.Header.FormatVersion != format.FormatVersion {
				t.Errorf("format version: got %d, want %d", r.Header.FormatVersion, format.FormatVersion)
			}
			if r.Header.SnapshotGeneration != c.generation {
				t.Errorf("generation: got %d, want %d", r.Header.SnapshotGeneration, c.generation)
			}
			if got := r.Header.Flags&format.FlagHasPreviousSnapshot != 0; got != c.hasPrev {
				t.Errorf("hasPrev: got %v, want %v", got, c.hasPrev)
			}
			if got := r.Header.Flags&format.FlagColdStartInWindow != 0; got != c.coldInWin {
				t.Errorf("coldInWin: got %v, want %v", got, c.coldInWin)
			}
			for _, want := range c.variables {
				if _, ok := r.VariableID(want); !ok {
					t.Errorf("variable %q not in catalog", want)
				}
			}
			out := make([]float32, r.PixelCount())
			for _, name := range c.variables {
				if err := r.ReadTile(name, 0, 0, 0, 0, out); err != nil {
					t.Errorf("read tile for %s: %v", name, err)
				}
			}
		})
	}
}

func TestCRCCorruptedFallback(t *testing.T) {
	r, err := reader.Open(filepath.Join("testdata", "crc_corrupted.wmt"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if _, ok := r.VariableID("temp"); !ok {
		t.Errorf("temp should still be readable from previous snapshot")
	}
	if _, ok := r.VariableID("wind"); !ok {
		t.Errorf("wind should still be readable from previous snapshot")
	}
	if _, ok := r.VariableID("precip"); ok {
		t.Errorf("precip was added by the corrupted append; previous snapshot must not see it")
	}
}

func TestCorruptedSnapshotErrorPath(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "no_prev.wmt")
	src, err := os.ReadFile(filepath.Join("testdata", "minimal.wmt"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.UnmarshalHeader(src[:format.HeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	src[h.ActiveSnapshotOffset+h.ActiveSnapshotLength/2] ^= 0xFF
	if err := os.WriteFile(tmp, src, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := reader.Open(tmp); err == nil {
		t.Errorf("expected error on corrupted snapshot with no previous fallback")
	} else if !errors.Is(err, format.ErrBadSnapshotCRC) {
		t.Logf("got error (file rejected, but not specifically ErrBadSnapshotCRC): %v", err)
	}
}
