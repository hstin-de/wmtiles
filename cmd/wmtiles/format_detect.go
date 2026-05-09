package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hstin-de/wmtiles/encode"
	"github.com/hstin-de/wmtiles/parser"
)

// detectFormat resolves the source format for `wmtiles encode`. precedence:
// explicit --format flag, magic bytes on the first existing positional, then
// extension. Returns the args with --format stripped so the chosen runner can
// reparse the rest with its own flag set.
func detectFormat(args []string) (encode.Format, []string, error) {
	stripped := make([]string, 0, len(args))
	var explicit string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--format" || a == "-format":
			if i+1 >= len(args) {
				return "", nil, errors.New("--format requires a value (grib2|hdf5)")
			}
			explicit = args[i+1]
			i++
			continue
		case strings.HasPrefix(a, "--format="):
			explicit = strings.TrimPrefix(a, "--format=")
			continue
		case strings.HasPrefix(a, "-format="):
			explicit = strings.TrimPrefix(a, "-format=")
			continue
		}
		stripped = append(stripped, a)
	}
	if explicit != "" {
		f := encode.Format(strings.ToLower(explicit))
		switch f {
		case encode.FormatGRIB2, encode.FormatHDF5:
			return f, stripped, nil
		default:
			return "", nil, errors.New("--format must be grib2 or hdf5, got " + explicit)
		}
	}

	candidate := firstPositional(stripped)
	if candidate == "" {
		// no file — defer to the GRIB runner so usage errors keep their wording
		return encode.FormatGRIB2, stripped, nil
	}
	resolved := candidate
	if matches, err := filepath.Glob(candidate); err == nil && len(matches) > 0 {
		resolved = matches[0]
	}
	if f, ok := detectFormatFromFile(resolved); ok {
		return f, stripped, nil
	}
	if f, ok := detectFormatFromExt(resolved); ok {
		return f, stripped, nil
	}
	if _, err := os.Stat(resolved); err == nil {
		return "", nil, fmt.Errorf(
			"cannot determine input format of %s: not GRIB2 (missing 'GRIB' magic) "+
				"and not HDF5 (missing \\x89HDF magic); pass --format grib2|hdf5 to override",
			resolved)
	}
	return encode.FormatGRIB2, stripped, nil
}

func firstPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if strings.Contains(a, "=") {
			continue
		}
		// the encode flag set has -o, --filter, --precision etc. that take a
		// value as the next token; conservatively step past it unless that
		// token is itself a flag.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return ""
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
