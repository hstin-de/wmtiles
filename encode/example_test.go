package encode_test

import "github.com/hstin-de/wmtiles/encode"

func ExampleNewEncoder() {
	enc, err := encode.NewEncoder("forecast.wmt", encode.Options{
		TileSize:        256,
		MinZoom:         0,
		MaxZoom:         5,
		FilterVariables: []string{"2t", "10u", "10v"},
		Precision: map[string]float64{
			"2t":  0.05,
			"10u": 0.1,
			"10v": 0.1,
		},
	})
	if err != nil {
		panic(err)
	}

	if err := enc.AddFile("gfs-f000.grib2", encode.FormatGRIB2); err != nil {
		panic(err)
	}
	if err := enc.AddFile("gfs-f001.grib2", encode.FormatGRIB2); err != nil {
		panic(err)
	}

	// AddBytes keeps a reference to the slice until Finish returns.
	extraGRIB2 := []byte{ /* GRIB2 messages */ }
	if err := enc.AddBytes("extra.grib2", encode.FormatGRIB2, extraGRIB2); err != nil {
		panic(err)
	}

	// Finish scans all inputs together, builds one merged variable/time catalog,
	// and writes one fresh WMT file. It does not call append/extend per input.
	if err := enc.Finish(); err != nil {
		panic(err)
	}
}
