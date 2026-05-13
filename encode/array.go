package encode

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/hstin-de/wmtiles/parser"
)

// AddArray is the only entry point; AddFile/AddBytes reject this format.
const FormatArray Format = "array"

// Outside real GRIB2 discipline values so synthetic VarKeys can't collide
// with anything a parser would emit.
const arrayDisciplineMarker = 255

// Data is row-major: data[y*Nx + x] is the value at (Lat0 + y*DY, Lon0 + x*DX).
// DX or DY may be negative for flipped grids.
type GridSpec struct {
	Nx, Ny     int
	Lat0, Lon0 float64
	DX, DY     float64

	// Zero defaults to NaN.
	MissingValue float64
}

type ArrayInput struct {
	Variable      string
	Unit          string
	ReferenceTime time.Time
	Grid          GridSpec
	Data          []float32
}

// Same Variable across calls collapses to one time series; the Grid must
// match across those calls or the sampler will read garbage.
func (e *Encoder) AddArray(in ArrayInput) error {
	if e.finished {
		return errors.New("wmtiles/encode: Encoder already finished")
	}
	msg, err := buildArrayMessage(in, &e.arrayVarSeq)
	if err != nil {
		return err
	}
	e.inputs = append(e.inputs, input{
		name:   arrayInputName(in),
		format: FormatArray,
		msgs:   []parser.GRIBFile{msg},
	})
	return nil
}

func (a *Appender) AddArray(in ArrayInput) error {
	if a.finished {
		return errors.New("wmtiles/encode: Appender already finished")
	}
	msg, err := buildArrayMessage(in, &a.arrayVarSeq)
	if err != nil {
		return err
	}
	a.inputs = append(a.inputs, input{
		name:   arrayInputName(in),
		format: FormatArray,
		msgs:   []parser.GRIBFile{msg},
	})
	return nil
}

func arrayInputName(in ArrayInput) string {
	t := in.ReferenceTime.UTC().Format(time.RFC3339)
	if in.Variable == "" {
		return "array@" + t
	}
	return in.Variable + "@" + t
}

func buildArrayMessage(in ArrayInput, varSeq *map[string]int) (parser.GRIBFile, error) {
	if in.Variable == "" {
		return parser.GRIBFile{}, errors.New("wmtiles/encode: AddArray Variable is empty")
	}
	if in.ReferenceTime.IsZero() {
		return parser.GRIBFile{}, errors.New("wmtiles/encode: AddArray ReferenceTime is zero")
	}
	g := in.Grid
	if g.Nx <= 1 || g.Ny <= 1 {
		return parser.GRIBFile{}, fmt.Errorf("wmtiles/encode: AddArray grid must be at least 2x2, got %dx%d", g.Nx, g.Ny)
	}
	if g.DX == 0 || g.DY == 0 {
		return parser.GRIBFile{}, errors.New("wmtiles/encode: AddArray grid DX and DY must be non-zero")
	}
	if expected := g.Nx * g.Ny; len(in.Data) != expected {
		return parser.GRIBFile{}, fmt.Errorf("wmtiles/encode: AddArray data length %d != Nx*Ny %d", len(in.Data), expected)
	}
	if !math.IsNaN(g.Lat0) && (g.Lat0 < -90 || g.Lat0 > 90) {
		return parser.GRIBFile{}, fmt.Errorf("wmtiles/encode: AddArray Lat0 %g out of range", g.Lat0)
	}

	missing := g.MissingValue
	if missing == 0 {
		missing = math.NaN()
	}

	lats := make([]float64, g.Ny)
	for i := 0; i < g.Ny; i++ {
		lats[i] = g.Lat0 + float64(i)*g.DY
	}
	lons := make([]float64, g.Nx)
	for i := 0; i < g.Nx; i++ {
		lons[i] = g.Lon0 + float64(i)*g.DX
	}

	if *varSeq == nil {
		*varSeq = map[string]int{}
	}
	paramNum, ok := (*varSeq)[in.Variable]
	if !ok {
		paramNum = len(*varSeq)
		(*varSeq)[in.Variable] = paramNum
	}

	hdr := parser.GribHeader{
		ShortName:          in.Variable,
		Units:              in.Unit,
		Nx:                 g.Nx,
		Ny:                 g.Ny,
		La1:                lats[0],
		La2:                lats[g.Ny-1],
		Lo1:                lons[0],
		Lo2:                lons[g.Nx-1],
		DX:                 math.Abs(g.DX),
		DY:                 math.Abs(g.DY),
		Discipline:         arrayDisciplineMarker,
		ParameterCategory:  0,
		ParameterNumber:    paramNum,
		ReferenceTime:      in.ReferenceTime,
		MissingValue:       missing,
		DistinctLatitudes:  lats,
		DistinctLongitudes: lons,
	}

	data := make([]float32, len(in.Data))
	copy(data, in.Data)

	return parser.GRIBFile{Header: hdr, DataValues: data}, nil
}
