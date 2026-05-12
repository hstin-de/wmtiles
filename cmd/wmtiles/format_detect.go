package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/parser"
)

// Magic bytes beat extension because users sometimes rename files; the
// extension fallback covers HDF5 directories where opening every file just
// to peek is wasteful.
func detectFormatFromPath(path string) (encode.Format, error) {
	if matches, err := filepath.Glob(path); err == nil && len(matches) > 0 {
		path = matches[0]
	}
	if f, ok := detectFormatFromFile(path); ok {
		return f, nil
	}
	if f, ok := detectFormatFromExt(path); ok {
		return f, nil
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf(
			"cannot determine input format of %s: not GRIB2 (missing 'GRIB' magic) "+
				"and not HDF5 (missing \\x89HDF magic); pass --format grib2|hdf5",
			path)
	}
	return "", fmt.Errorf("input not found: %s", path)
}

func detectFormatFromFile(path string) (encode.Format, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return "", false
	}
	if parser.IsHDF5File(path) {
		return encode.FormatHDF5, true
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var head [4]byte
	if _, err := f.Read(head[:]); err != nil {
		return "", false
	}
	if string(head[:]) == "GRIB" {
		return encode.FormatGRIB2, true
	}
	return "", false
}

func detectFormatFromExt(path string) (encode.Format, bool) {
	low := strings.ToLower(path)
	switch {
	case strings.HasSuffix(low, ".grib2"),
		strings.HasSuffix(low, ".grib"),
		strings.HasSuffix(low, ".grb2"),
		strings.HasSuffix(low, ".grb"):
		return encode.FormatGRIB2, true
	case strings.HasSuffix(low, ".h5"),
		strings.HasSuffix(low, ".hdf5"),
		strings.HasSuffix(low, "-hd5"),
		strings.HasSuffix(low, ".nc"),
		strings.HasSuffix(low, ".nc4"):
		return encode.FormatHDF5, true
	}
	return "", false
}
