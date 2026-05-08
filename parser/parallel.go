package parser

/*
#include "eccodes.h"
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"
)

type MessageRange struct {
	Offset int
	Length int
}

// MessageRanges scans GRIB2 message boundaries without cgo, so the results
// can drive a parallel parser pool.
func MessageRanges(data []byte) ([]MessageRange, error) {
	if len(data) == 0 {
		return nil, errors.New("empty grib data")
	}
	out := make([]MessageRange, 0, 16)
	pos := 0
	for {
		i := bytes.Index(data[pos:], []byte("GRIB"))
		if i < 0 {
			if len(out) == 0 {
				return nil, errors.New("no GRIB message found")
			}
			return out, nil
		}
		start := pos + i
		if len(data)-start < 16 {
			return nil, fmt.Errorf("truncated GRIB message header at byte %d", start)
		}
		if edition := data[start+7]; edition != 2 {
			return nil, fmt.Errorf("unsupported GRIB edition %d at byte %d", edition, start)
		}
		msgLen := binary.BigEndian.Uint64(data[start+8 : start+16])
		if msgLen < 16 {
			return nil, fmt.Errorf("invalid GRIB message length %d at byte %d", msgLen, start)
		}
		if msgLen > uint64(len(data)-start) {
			return nil, fmt.Errorf("truncated GRIB message at byte %d: length %d, remaining %d",
				start, msgLen, len(data)-start)
		}
		out = append(out, MessageRange{Offset: start, Length: int(msgLen)})
		pos = start + int(msgLen)
	}
}

// ParseHeaderBytes decodes header scalars from a single GRIB2 message.
func ParseHeaderBytes(msg []byte) (GribHeader, error) {
	if len(msg) == 0 {
		return GribHeader{}, errors.New("empty grib message")
	}
	gid := C.codes_handle_new_from_message(
		C.codes_context_get_default(),
		unsafe.Pointer(&msg[0]),
		C.size_t(len(msg)),
	)
	if gid == nil {
		return GribHeader{}, errors.New("codes_handle_new_from_message returned nil")
	}
	defer C.codes_handle_delete(gid)
	return extractHeaderScalars(gid)
}

// ParseFullBytes decodes header and values from a single GRIB2 message.
func ParseFullBytes(msg []byte) (GRIBFile, error) {
	if len(msg) == 0 {
		return GRIBFile{}, errors.New("empty grib message")
	}
	gid := C.codes_handle_new_from_message(
		C.codes_context_get_default(),
		unsafe.Pointer(&msg[0]),
		C.size_t(len(msg)),
	)
	if gid == nil {
		return GRIBFile{}, errors.New("codes_handle_new_from_message returned nil")
	}
	defer C.codes_handle_delete(gid)
	return processHandle(gid)
}

// ParseValuesBytes decodes the values array from a GRIB2 message and attaches
// the supplied header, skipping the redundant header-scalar pass.
func ParseValuesBytes(msg []byte, h GribHeader) (GRIBFile, error) {
	if len(msg) == 0 {
		return GRIBFile{}, errors.New("empty grib message")
	}
	gid := C.codes_handle_new_from_message(
		C.codes_context_get_default(),
		unsafe.Pointer(&msg[0]),
		C.size_t(len(msg)),
	)
	if gid == nil {
		return GRIBFile{}, errors.New("codes_handle_new_from_message returned nil")
	}
	defer C.codes_handle_delete(gid)
	return finishProcessHandle(gid, h)
}
