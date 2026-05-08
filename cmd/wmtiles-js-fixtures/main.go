// emits Go-encoded lorenzo_zstd blobs + source bytes as JSON, so the JS
// decoder can be checked against the producer without a zstd encoder
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/quantize"
)

func main() {
	const w = 32

	// u16 smooth quadratic
	src16 := make([]byte, w*w*2)
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			v := uint16(1000 + x + y + (x*y)/16)
			i := y*w + x
			src16[2*i] = byte(v)
			src16[2*i+1] = byte(v >> 8)
		}
	}

	// u8 gradient
	src8 := make([]byte, w*w)
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			src8[y*w+x] = byte(x + y)
		}
	}

	enc, err := codec.NewEncoder(3)
	if err != nil {
		panic(err)
	}
	defer enc.Close()

	p16 := quantize.Params{DType: quantize.DTypeU16, Scale: 1, Offset: 0}
	p8 := quantize.Params{DType: quantize.DTypeU8, Scale: 1, Offset: 0}

	blob16, err := enc.EncodeWith(codec.IDLorenzoZstd, src16, p16, w*w)
	if err != nil {
		panic(err)
	}
	blob8, err := enc.EncodeWith(codec.IDLorenzoZstd, src8, p8, w*w)
	if err != nil {
		panic(err)
	}

	out := map[string]any{
		"w": w,
		"u8": map[string]string{
			"src":  base64.StdEncoding.EncodeToString(src8),
			"blob": base64.StdEncoding.EncodeToString(blob8),
		},
		"u16": map[string]string{
			"src":  base64.StdEncoding.EncodeToString(src16),
			"blob": base64.StdEncoding.EncodeToString(blob16),
		},
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if len(os.Args) >= 2 {
		os.WriteFile(os.Args[1], b, 0o644)
	} else {
		fmt.Println(string(b))
	}
}
