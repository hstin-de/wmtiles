package bitshuffle

// scatter element bits across elemSize*8 planes: quantised data tends to share
// high-order bits, so each plane runs long and zstd compresses it better than the
// interleaved byte stream would

func Encode(src []byte, elemSize, elemCount int, dst []byte) {
	bytesPerPlane := (elemCount + 7) / 8
	blockEnd := (elemCount / 8) * 8
	if blockEnd != elemCount {
		tailByte := blockEnd / 8
		for plane := 0; plane < elemSize*8; plane++ {
			dst[plane*bytesPerPlane+tailByte] = 0
		}
	}

	for elem0 := 0; elem0 < blockEnd; elem0 += 8 {
		byteOff := elem0 / 8
		for b := 0; b < elemSize; b++ {
			x := uint64(src[(elem0+0)*elemSize+b]) |
				uint64(src[(elem0+1)*elemSize+b])<<8 |
				uint64(src[(elem0+2)*elemSize+b])<<16 |
				uint64(src[(elem0+3)*elemSize+b])<<24 |
				uint64(src[(elem0+4)*elemSize+b])<<32 |
				uint64(src[(elem0+5)*elemSize+b])<<40 |
				uint64(src[(elem0+6)*elemSize+b])<<48 |
				uint64(src[(elem0+7)*elemSize+b])<<56

			y := transpose8x8(x)
			y = reverseBitsPerByte(y)

			planeBase := b * 8
			for k := 0; k < 8; k++ {
				dst[(planeBase+k)*bytesPerPlane+byteOff] = byte(y >> (8 * k))
			}
		}
	}

	for elem := blockEnd; elem < elemCount; elem++ {
		byteOff := elem / 8
		bitOff := uint(7 - (elem % 8))
		for b := 0; b < elemSize; b++ {
			v := src[elem*elemSize+b]
			if v == 0 {
				continue
			}
			for k := 0; k < 8; k++ {
				if (v>>uint(k))&1 == 0 {
					continue
				}
				dst[(b*8+k)*bytesPerPlane+byteOff] |= byte(1) << bitOff
			}
		}
	}
}

func Decode(src []byte, elemSize, elemCount int, dst []byte) {
	bytesPerPlane := (elemCount + 7) / 8
	blockEnd := (elemCount / 8) * 8
	// only the tail uses |= on dst; the fast path overwrites everything else
	if blockEnd != elemCount {
		for elem := blockEnd; elem < elemCount; elem++ {
			base := elem * elemSize
			for b := 0; b < elemSize; b++ {
				dst[base+b] = 0
			}
		}
	}

	for elem0 := 0; elem0 < blockEnd; elem0 += 8 {
		byteOff := elem0 / 8
		for b := 0; b < elemSize; b++ {
			planeBase := b * 8
			y := uint64(src[(planeBase+0)*bytesPerPlane+byteOff]) |
				uint64(src[(planeBase+1)*bytesPerPlane+byteOff])<<8 |
				uint64(src[(planeBase+2)*bytesPerPlane+byteOff])<<16 |
				uint64(src[(planeBase+3)*bytesPerPlane+byteOff])<<24 |
				uint64(src[(planeBase+4)*bytesPerPlane+byteOff])<<32 |
				uint64(src[(planeBase+5)*bytesPerPlane+byteOff])<<40 |
				uint64(src[(planeBase+6)*bytesPerPlane+byteOff])<<48 |
				uint64(src[(planeBase+7)*bytesPerPlane+byteOff])<<56

			y = reverseBitsPerByte(y)
			x := transpose8x8(y)

			for r := 0; r < 8; r++ {
				dst[(elem0+r)*elemSize+b] = byte(x >> (8 * r))
			}
		}
	}

	for bitIndex := 0; bitIndex < elemSize*8; bitIndex++ {
		b := bitIndex / 8
		k := bitIndex % 8
		planeBase := bitIndex * bytesPerPlane
		for elem := blockEnd; elem < elemCount; elem++ {
			byteOff := elem / 8
			bitOff := uint(7 - (elem % 8))
			if (src[planeBase+byteOff]>>bitOff)&1 == 1 {
				dst[elem*elemSize+b] |= byte(1) << uint(k)
			}
		}
	}
}

func EncodedLen(elemSize, elemCount int) int {
	return 8 * elemSize * ((elemCount + 7) / 8)
}

// 8x8 bit-matrix transpose, Hacker's Delight §7-3: treat x as 8 rows of 8 bits, output is 8 columns
func transpose8x8(x uint64) uint64 {
	t := (x ^ (x >> 7)) & 0x00AA00AA00AA00AA
	x ^= t ^ (t << 7)
	t = (x ^ (x >> 14)) & 0x0000CCCC0000CCCC
	x ^= t ^ (t << 14)
	t = (x ^ (x >> 28)) & 0x00000000F0F0F0F0
	x ^= t ^ (t << 28)
	return x
}

// reverse bits within each of 8 bytes independently: needed because the per-byte
// bit indexing in the slow path runs MSB-first while transpose8x8 is LSB-first
func reverseBitsPerByte(x uint64) uint64 {
	x = ((x >> 1) & 0x5555555555555555) | ((x & 0x5555555555555555) << 1)
	x = ((x >> 2) & 0x3333333333333333) | ((x & 0x3333333333333333) << 2)
	x = ((x >> 4) & 0x0F0F0F0F0F0F0F0F) | ((x & 0x0F0F0F0F0F0F0F0F) << 4)
	return x
}
