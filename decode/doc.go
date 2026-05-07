// Package decode reads WMTiles files.
//
// Use Open for files, NewReader for custom io.ReaderAt sources, then read
// metadata and float32 value tiles through Decoder methods such as Variables,
// Times, ReadTile and ReadTiles.
package decode
