package codec

/*
// ZDICT symbols are linked from DataDog/zstd's bundled libzstd archive.
#include <stddef.h>

extern size_t ZDICT_trainFromBuffer(void* dictBuffer, size_t dictBufferCapacity,
                                    const void* samplesBuffer,
                                    const size_t* samplesSizes,
                                    unsigned nbSamples);
extern unsigned ZDICT_isError(size_t errorCode);
extern const char* ZDICT_getErrorName(size_t errorCode);
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// TrainDict runs ZDICT_trainFromBuffer over samples and returns the trained
// dict (prefixed with magic 0xEC30A437). dictCapacity caps the output; libzstd
// generally needs ~100 KB of samples for a useful 16–64 KB dict.
func TrainDict(samples [][]byte, dictCapacity int) ([]byte, error) {
	if len(samples) == 0 {
		return nil, errors.New("codec: TrainDict: no samples")
	}
	if dictCapacity <= 0 {
		dictCapacity = 64 * 1024
	}

	// flatten samples into one contiguous buffer plus a parallel sizes array
	total := 0
	for _, s := range samples {
		total += len(s)
	}
	if total == 0 {
		return nil, errors.New("codec: TrainDict: empty samples")
	}
	flat := make([]byte, total)
	sizes := make([]C.size_t, len(samples))
	off := 0
	for i, s := range samples {
		copy(flat[off:], s)
		off += len(s)
		sizes[i] = C.size_t(len(s))
	}

	dict := make([]byte, dictCapacity)

	var samplesPtr unsafe.Pointer
	if len(flat) > 0 {
		samplesPtr = unsafe.Pointer(&flat[0])
	}
	var sizesPtr *C.size_t
	if len(sizes) > 0 {
		sizesPtr = &sizes[0]
	}

	w := C.ZDICT_trainFromBuffer(
		unsafe.Pointer(&dict[0]),
		C.size_t(dictCapacity),
		samplesPtr,
		sizesPtr,
		C.unsigned(len(samples)),
	)
	if C.ZDICT_isError(w) != 0 {
		errName := C.GoString(C.ZDICT_getErrorName(w))
		return nil, fmt.Errorf("codec: ZDICT_trainFromBuffer: %s", errName)
	}
	return dict[:int(w)], nil
}
