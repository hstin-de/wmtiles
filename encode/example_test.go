package encode_test

import (
	"math"
	"time"

	"github.com/hstin-de/wmtiles/encode"
)

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

func ExampleEncoder_AddArray() {
	enc, err := encode.NewEncoder("custom.wmt", encode.Options{
		TileSize:  256,
		MinZoom:   0,
		MaxZoom:   5,
		Precision: map[string]float64{"t2m": 0.05},
	})
	if err != nil {
		panic(err)
	}

	const nx, ny = 720, 361
	values := make([]float32, nx*ny)
	// fill values[y*nx + x] with the sample at (Lat0 + y*DY, Lon0 + x*DX)

	if err := enc.AddArray(encode.ArrayInput{
		Variable:      "t2m",
		Unit:          "K",
		ReferenceTime: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		Grid: encode.GridSpec{
			Nx: nx, Ny: ny,
			Lon0: -180, Lat0: -90,
			DX: 0.5, DY: 0.5,
			MissingValue: math.NaN(),
		},
		Data: values,
	}); err != nil {
		panic(err)
	}

	// Repeat AddArray with the same Variable and the same Grid for further
	// timesteps; distinct Variable names produce separate time series.

	if err := enc.Finish(); err != nil {
		panic(err)
	}
}
