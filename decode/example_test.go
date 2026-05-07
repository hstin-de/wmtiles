package decode_test

import "github.com/hstin-de/wmtiles/decode"

func ExampleOpen() {
	wmt, err := decode.Open("forecast.wmt")
	if err != nil {
		panic(err)
	}
	defer wmt.Close()

	variables := wmt.Variables()
	times := wmt.Times()
	bounds := wmt.Bounds()

	pixels, err := wmt.ReadTile("2t", 12, decode.Coord(5, 16, 11))
	if err != nil {
		panic(err)
	}

	_, _, _, _ = variables, times, bounds, pixels
}

func ExampleDecoder_ReadTiles() {
	wmt, err := decode.Open("forecast.wmt")
	if err != nil {
		panic(err)
	}
	defer wmt.Close()

	coords := []decode.TileCoord{
		decode.Coord(5, 16, 11),
		decode.Coord(5, 17, 11),
		decode.Coord(5, 18, 11),
	}
	outs, err := wmt.ReadTiles("2t", 12, coords)
	if err != nil {
		panic(err)
	}
	_ = outs
}
