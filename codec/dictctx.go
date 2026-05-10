package codec

/*
// Symbols come from DataDog/zstd's bundled libzstd. Declared directly so a
// caller can hold one CCtx across many Compress calls; DataDog's
// BulkProcessor allocates a fresh CCtx per call.
#include <stddef.h>

extern void* ZSTD_createCCtx(void);
extern size_t ZSTD_freeCCtx(void* cctx);
extern void* ZSTD_createCDict(const void* dictBuffer, size_t dictSize, int compressionLevel);
extern size_t ZSTD_freeCDict(void* cdict);
extern size_t ZSTD_compress_usingCDict(void* cctx,
                                       void* dst, size_t dstCapacity,
                                       const void* src, size_t srcSize,
                                       const void* cdict);
extern size_t ZSTD_compressBound(size_t srcSize);
extern unsigned ZSTD_isError(size_t code);
extern const char* ZSTD_getErrorName(size_t code);
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// DictCCtx pairs one libzstd CCtx with one digested CDict for repeated
// dict-aware compression. Not safe for concurrent use.
type DictCCtx struct {
	cctx  unsafe.Pointer
	cdict unsafe.Pointer
}

// NewDictCCtx digests dictBytes into a CDict at the given level and pairs it
// with a fresh CCtx. Both are freed by the runtime finalizer; callers that
// need deterministic teardown should call Free.
func NewDictCCtx(dictBytes []byte, level int) (*DictCCtx, error) {
	if len(dictBytes) == 0 {
		return nil, errors.New("codec: NewDictCCtx: empty dict")
	}
	if level == 0 {
		level = 3
	}
	cdict := C.ZSTD_createCDict(unsafe.Pointer(&dictBytes[0]), C.size_t(len(dictBytes)), C.int(level))
	if cdict == nil {
		return nil, errors.New("codec: ZSTD_createCDict failed")
	}
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		C.ZSTD_freeCDict(cdict)
		return nil, errors.New("codec: ZSTD_createCCtx failed")
	}
	d := &DictCCtx{cctx: cctx, cdict: cdict}
	runtime.SetFinalizer(d, func(d *DictCCtx) { d.Free() })
	return d, nil
}

// Free releases the CCtx and CDict. Idempotent.
func (d *DictCCtx) Free() {
	if d.cctx != nil {
		C.ZSTD_freeCCtx(d.cctx)
		d.cctx = nil
	}
	if d.cdict != nil {
		C.ZSTD_freeCDict(d.cdict)
		d.cdict = nil
	}
	runtime.SetFinalizer(d, nil)
}

// Compress writes the dict-aware compression of src into dst (reused if its
// capacity is at least zstd's CompressBound) and returns the populated
// prefix. Reuses the held CCtx — no per-call allocation in libzstd.
func (d *DictCCtx) Compress(dst, src []byte) ([]byte, error) {
	bound := int(C.ZSTD_compressBound(C.size_t(len(src))))
	if cap(dst) < bound {
		dst = make([]byte, bound)
	} else {
		dst = dst[:bound]
	}
	var srcPtr unsafe.Pointer
	if len(src) > 0 {
		srcPtr = unsafe.Pointer(&src[0])
	}
	w := C.ZSTD_compress_usingCDict(
		d.cctx,
		unsafe.Pointer(&dst[0]),
		C.size_t(len(dst)),
		srcPtr,
		C.size_t(len(src)),
		d.cdict,
	)
	if C.ZSTD_isError(w) != 0 {
		return nil, fmt.Errorf("codec: ZSTD_compress_usingCDict: %s",
			C.GoString(C.ZSTD_getErrorName(w)))
	}
	return dst[:int(w)], nil
}
