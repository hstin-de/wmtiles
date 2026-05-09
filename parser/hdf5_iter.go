package parser

// HDF5 shims matching the GRIB2 ForEachMessage* signatures so encode/extend/
// compare can dispatch on input format without caring about the underlying
// reader. Header iterators take the cheap metadata-only parse.

func ForEachHDF5HeaderFiltered(path string, want func(shortName string) bool,
	fn func(GribHeader) error) error {
	files, err := ParseHDF5Headers(path)
	if err != nil {
		return err
	}
	for _, gf := range files {
		if want != nil && !want(gf.Header.ShortName) {
			continue
		}
		if err := fn(gf.Header); err != nil {
			return err
		}
	}
	return nil
}

func ForEachHDF5HeaderBytesFiltered(data []byte, want func(shortName string) bool,
	fn func(GribHeader) error) error {
	files, err := ParseHDF5HeadersBytes(data)
	if err != nil {
		return err
	}
	for _, gf := range files {
		if want != nil && !want(gf.Header.ShortName) {
			continue
		}
		if err := fn(gf.Header); err != nil {
			return err
		}
	}
	return nil
}

func ForEachHDF5MessageFiltered(path string, want func(*GribHeader) bool,
	fn func(GRIBFile) error) error {
	files, err := ParseHDF5File(path)
	if err != nil {
		return err
	}
	for _, gf := range files {
		if want != nil && !want(&gf.Header) {
			continue
		}
		if err := fn(gf); err != nil {
			return err
		}
	}
	return nil
}

func ForEachHDF5MessageBytesFiltered(data []byte, want func(*GribHeader) bool,
	fn func(GRIBFile) error) error {
	files, err := ParseHDF5Bytes(data)
	if err != nil {
		return err
	}
	for _, gf := range files {
		if want != nil && !want(&gf.Header) {
			continue
		}
		if err := fn(gf); err != nil {
			return err
		}
	}
	return nil
}
