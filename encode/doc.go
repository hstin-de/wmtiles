// Package encode converts one or more source-data inputs into a single WMTiles
// file.
//
// The encoder accepts file paths (AddFile), in-memory byte slices (AddBytes),
// and raw float32 arrays on a regular lat-lon grid (AddArray). It scans all
// pending inputs together when Finish is called, builds one merged variable/time
// catalog, then writes one fresh WMT file. It does not append or extend once per
// input.
//
// AddFile and AddBytes parse GRIB2 (via ecCodes) and HDF5 (ODIM_H5 and
// CF-1.x/NetCDF4 via libhdf5). AddArray takes pre-decoded values and skips the
// parser entirely; it is the right entry point when the data already lives in
// Go memory, e.g. from a custom reader or an in-process model.
package encode
