package parser

// thin cgo wrapper over libhdf5; entry point for ODIM_H5 and CF/NetCDF4 readers

/*
#cgo LDFLAGS: -lhdf5
#include "wmt_hdf5.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"unsafe"
)

type h5File struct {
	id C.int64_t
}

func openH5(path string) (*h5File, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	id := C.wmt_h5_open_ro(cp)
	if id < 0 {
		return nil, fmt.Errorf("hdf5: open %s failed", path)
	}
	return &h5File{id: id}, nil
}

func (f *h5File) close() {
	if f != nil && f.id >= 0 {
		C.wmt_h5_close_file(f.id)
		f.id = -1
	}
}

func (f *h5File) exists(path string) bool {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	return C.wmt_h5_link_exists(f.id, cp) == 1
}

func (f *h5File) attrStr(objPath, attr string) (string, bool) {
	op := C.CString(objPath)
	ap := C.CString(attr)
	defer C.free(unsafe.Pointer(op))
	defer C.free(unsafe.Pointer(ap))
	const cap = 1024
	buf := make([]byte, cap)
	rc := C.wmt_h5_attr_str(f.id, op, ap, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(cap))
	if rc != 0 {
		return "", false
	}
	n := 0
	for n < cap && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), true
}

func (f *h5File) attrF64(objPath, attr string) (float64, bool) {
	op := C.CString(objPath)
	ap := C.CString(attr)
	defer C.free(unsafe.Pointer(op))
	defer C.free(unsafe.Pointer(ap))
	var v C.double
	if C.wmt_h5_attr_f64(f.id, op, ap, &v) != 0 {
		return 0, false
	}
	return float64(v), true
}

func (f *h5File) attrI64(objPath, attr string) (int64, bool) {
	op := C.CString(objPath)
	ap := C.CString(attr)
	defer C.free(unsafe.Pointer(op))
	defer C.free(unsafe.Pointer(ap))
	var v C.int64_t
	if C.wmt_h5_attr_i64(f.id, op, ap, &v) != 0 {
		return 0, false
	}
	return int64(v), true
}

func (f *h5File) datasetShape(path string) ([]int, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	const maxRank = 8
	dims := make([]C.hsize_t, maxRank)
	var nd C.int
	rc := C.wmt_h5_dataset_shape(f.id, cp, &nd, &dims[0], C.int(maxRank))
	if rc != 0 {
		return nil, fmt.Errorf("hdf5: shape %s failed", path)
	}
	out := make([]int, int(nd))
	for i := range out {
		out[i] = int(dims[i])
	}
	return out, nil
}

func (f *h5File) readU16(path string, buf []uint16) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if len(buf) == 0 {
		return errors.New("hdf5: empty u16 buffer")
	}
	rc := C.wmt_h5_read_u16(f.id, cp,
		(*C.uint16_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if rc != 0 {
		return fmt.Errorf("hdf5: read_u16 %s failed", path)
	}
	return nil
}

func (f *h5File) readF32(path string, buf []float32) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if len(buf) == 0 {
		return errors.New("hdf5: empty f32 buffer")
	}
	rc := C.wmt_h5_read_f32(f.id, cp,
		(*C.float)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if rc != 0 {
		return fmt.Errorf("hdf5: read_f32 %s failed", path)
	}
	return nil
}

func (f *h5File) readF64(path string, buf []float64) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if len(buf) == 0 {
		return errors.New("hdf5: empty f64 buffer")
	}
	rc := C.wmt_h5_read_f64(f.id, cp,
		(*C.double)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if rc != 0 {
		return fmt.Errorf("hdf5: read_f64 %s failed", path)
	}
	return nil
}

func (f *h5File) listLinks(path string) ([]string, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	const cap = 16384
	const maxNames = 1024
	buf := make([]byte, cap)
	var used C.size_t
	var count C.size_t
	rc := C.wmt_h5_list_links(f.id, cp,
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(cap),
		&used, &count, C.size_t(maxNames))
	if rc != 0 {
		return nil, fmt.Errorf("hdf5: list_links %s failed", path)
	}
	out := make([]string, 0, int(count))
	start := 0
	for i := 0; i < int(used); i++ {
		if buf[i] == 0 {
			out = append(out, string(buf[start:i]))
			start = i + 1
			if len(out) >= int(count) {
				break
			}
		}
	}
	return out, nil
}

var HDF5SignaturePrefix = []byte{0x89, 'H', 'D', 'F', '\r', '\n', 0x1a, '\n'}

func HasHDF5Signature(b []byte) bool {
	if len(b) < len(HDF5SignaturePrefix) {
		return false
	}
	for i, c := range HDF5SignaturePrefix {
		if b[i] != c {
			return false
		}
	}
	return true
}

func IsHDF5File(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [8]byte
	if _, err := f.Read(buf[:]); err != nil {
		return false
	}
	return HasHDF5Signature(buf[:])
}

// ParseHDF5File parses an ODIM_H5 or CF-1.x file into one GRIBFile per
// (variable, time) record. ODIM grids are reprojected to regular lat-lon.
func ParseHDF5File(path string) ([]GRIBFile, error) {
	return parseHDF5(path, true)
}

// ParseHDF5Headers is the metadata-only sibling of ParseHDF5File: skips the U16
// read + stere reprojection (ODIM) and the float32 read + scale/offset apply
// (CF). DataValues is nil. Used by the encoder's catalog scan so it doesn't pay
// the resampling cost twice.
func ParseHDF5Headers(path string) ([]GRIBFile, error) {
	return parseHDF5(path, false)
}

func parseHDF5(path string, withValues bool) ([]GRIBFile, error) {
	f, err := openH5(path)
	if err != nil {
		return nil, err
	}
	defer f.close()

	if isODIM(f) {
		return parseODIM(f, path, withValues)
	}
	if isCF(f) {
		return parseCF(f, path, withValues)
	}
	return nil, fmt.Errorf("hdf5: %s is neither ODIM_H5 nor CF-1.x", path)
}

// ParseHDF5Bytes spills b to a temp file before opening it; libhdf5 has no
// portable in-memory open without a custom Virtual File Driver.
func ParseHDF5Bytes(b []byte) ([]GRIBFile, error) {
	return parseHDF5Bytes(b, true)
}

func ParseHDF5HeadersBytes(b []byte) ([]GRIBFile, error) {
	return parseHDF5Bytes(b, false)
}

func parseHDF5Bytes(b []byte, withValues bool) ([]GRIBFile, error) {
	if len(b) == 0 {
		return nil, errors.New("hdf5: empty bytes")
	}
	tmp, err := os.CreateTemp("", "wmtiles-h5-*.h5")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return parseHDF5(tmp.Name(), withValues)
}
