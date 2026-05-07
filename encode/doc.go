// Package encode converts one or more source-data inputs into a single WMTiles
// file.
//
// The encoder accepts both file paths and in-memory byte slices. It scans all
// pending inputs together when Finish is called, builds one merged variable/time
// catalog, then writes one fresh WMT file. It does not append or extend once per
// input.
//
// GRIB2 is currently the first implemented input format. The package is named
// encode rather than grib so later formats can use the same workflow.
package encode
