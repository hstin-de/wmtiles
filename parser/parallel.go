package parser

/*
#cgo CFLAGS: -I/usr/include/x86_64-linux-gnu/
#cgo LDFLAGS: -leccodes
#include "wmt_eccodes.h"
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

// ParseMessage decodes a GRIB2 message, preferring the pure-Go fast path
// and falling back to eccodes for unsupported templates. scratch may be nil.
func ParseMessage(msg []byte, scratch []float32) (GRIBFile, []float32, error) {
	if len(msg) == 0 {
		return GRIBFile{}, scratch, errors.New("empty grib message")
	}
	if g, vals, ok, err := FastDecodeRegularLL(msg, scratch); err != nil {
		return GRIBFile{}, scratch, err
	} else if ok {
		return g, vals, nil
	}
	var sc C.wmt_scalars_t
	var got C.size_t
	values := scratch
	if cap(values) == 0 {
		values = make([]float32, 1<<20)
	} else {
		values = values[:cap(values)]
	}
	for {
		rc := C.wmt_decode_full(
			nil,
			unsafe.Pointer(&msg[0]), C.size_t(len(msg)),
			&sc, (*C.float)(unsafe.Pointer(&values[0])), C.size_t(cap(values)), &got,
		)
		if rc == 0 {
			break
		}
		if rc == -2 {
			n := int(sc.ni) * int(sc.nj)
			if n <= cap(values) || n <= 0 {
				return GRIBFile{}, scratch, fmt.Errorf("wmt_decode_full reported -2 but cap=%d, ni*nj=%d", cap(values), n)
			}
			values = make([]float32, n)
			continue
		}
		return GRIBFile{}, scratch, fmt.Errorf("wmt_decode_full failed: %d", int(rc))
	}
	h, err := scalarsToHeader(&sc)
	if err != nil {
		return GRIBFile{}, scratch, err
	}
	values = values[:int(got)]
	if err := attachDistinct(&h, msg); err != nil {
		return GRIBFile{}, scratch, err
	}
	return GRIBFile{Header: h, DataValues: values}, values, nil
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
