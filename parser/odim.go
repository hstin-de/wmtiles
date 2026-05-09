package parser

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
)

func isODIM(f *h5File) bool {
	conv, ok := f.attrStr("", "Conventions")
	if !ok {
		return false
	}
	return strings.HasPrefix(conv, "ODIM_H5")
}

type odimWhere struct {
	ProjDef                    string
	XScale, YScale             float64
	XSize, YSize               int
	LLLat, LLLon, LRLat, LRLon float64
	ULLat, ULLon, URLat, URLon float64
}

func readODIMWhere(f *h5File) (odimWhere, error) {
	var w odimWhere
	pd, ok := f.attrStr("/where", "projdef")
	if !ok {
		return w, fmt.Errorf("ODIM: missing /where/projdef")
	}
	w.ProjDef = pd
	mustF := func(attr string) (float64, error) {
		v, ok := f.attrF64("/where", attr)
		if !ok {
			return 0, fmt.Errorf("ODIM: missing /where/%s", attr)
		}
		return v, nil
	}
	mustI := func(attr string) (int, error) {
		v, ok := f.attrI64("/where", attr)
		if !ok {
			return 0, fmt.Errorf("ODIM: missing /where/%s", attr)
		}
		return int(v), nil
	}
	var err error
	if w.XScale, err = mustF("xscale"); err != nil {
		return w, err
	}
	if w.YScale, err = mustF("yscale"); err != nil {
		return w, err
	}
	if w.XSize, err = mustI("xsize"); err != nil {
		return w, err
	}
	if w.YSize, err = mustI("ysize"); err != nil {
		return w, err
	}
	for _, attr := range []string{"LL_lat", "LL_lon", "LR_lat", "LR_lon", "UL_lat", "UL_lon", "UR_lat", "UR_lon"} {
		v, ok := f.attrF64("/where", attr)
		if !ok {
			return w, fmt.Errorf("ODIM: missing /where/%s", attr)
		}
		switch attr {
		case "LL_lat":
			w.LLLat = v
		case "LL_lon":
			w.LLLon = v
		case "LR_lat":
			w.LRLat = v
		case "LR_lon":
			w.LRLon = v
		case "UL_lat":
			w.ULLat = v
		case "UL_lon":
			w.ULLon = v
		case "UR_lat":
			w.URLat = v
		case "UR_lon":
			w.URLon = v
		}
	}
	return w, nil
}

type odimWhat struct {
	Date, Time string
	Object     string
}

func readODIMWhat(f *h5File) odimWhat {
	var w odimWhat
	w.Date, _ = f.attrStr("/what", "date")
	w.Time, _ = f.attrStr("/what", "time")
	w.Object, _ = f.attrStr("/what", "object")
	return w
}

// parses ODIM's "YYYYMMDD" + "HHMMSS" string pair; returns fallback on missing.
func readODIMTime(date, t string, fallback time.Time) (time.Time, error) {
	if date == "" || t == "" {
		return fallback, nil
	}
	if len(date) < 8 || len(t) < 6 {
		return fallback, fmt.Errorf("ODIM: bad date/time %q %q", date, t)
	}
	year := atoiSafe(date[0:4])
	mon := atoiSafe(date[4:6])
	day := atoiSafe(date[6:8])
	hour := atoiSafe(t[0:2])
	min := atoiSafe(t[2:4])
	sec := atoiSafe(t[4:6])
	return time.Date(year, time.Month(mon), day, hour, min, sec, 0, time.UTC), nil
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func listSubgroupsWithPrefix(f *h5File, parent, prefix string) ([]string, error) {
	names, err := f.listLinks(parent)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, nil
}

// 0.01° ≈ 1.1 km lat (~0.7 km lon at 50°N): light oversampling of the typical
// 1 km ODIM source grid so the regular lat-lon raster keeps full source detail.
const targetResolutionDeg = 0.01

// parseODIM yields one GRIBFile per /dataset{N}/data{M} pair. withValues=false
// skips the U16 read and stere reprojection — the encoder's catalog scan uses
// that path to keep parsing off the hot loop.
func parseODIM(f *h5File, sourceName string, withValues bool) ([]GRIBFile, error) {
	where, err := readODIMWhere(f)
	if err != nil {
		return nil, err
	}
	stereParams, err := ParseStereProj(where.ProjDef)
	if err != nil {
		return nil, fmt.Errorf("ODIM: parse projdef: %w", err)
	}
	// project the UL corner once to anchor the source grid in metres; pixel
	// (col, row) center sits at (X0 + (col+0.5)*XScale, Y0 - (row+0.5)*YScale).
	ulX, ulY := stereParams.Forward(where.ULLat*math.Pi/180, where.ULLon*math.Pi/180)
	srcGrid := stereGrid{
		Cols:   where.XSize,
		Rows:   where.YSize,
		XScale: where.XScale,
		YScale: where.YScale,
		X0:     ulX - where.XScale/2,
		Y0:     ulY + where.YScale/2,
		Params: stereParams,
	}

	// target bbox = axis-aligned hull of the four projected corners
	minLat := math.Min(math.Min(where.LLLat, where.LRLat), math.Min(where.ULLat, where.URLat))
	maxLat := math.Max(math.Max(where.LLLat, where.LRLat), math.Max(where.ULLat, where.URLat))
	minLon := math.Min(math.Min(where.LLLon, where.LRLon), math.Min(where.ULLon, where.URLon))
	maxLon := math.Max(math.Max(where.LLLon, where.LRLon), math.Max(where.ULLon, where.URLon))

	dLat := targetResolutionDeg
	dLon := targetResolutionDeg
	dstNy := int(math.Ceil((maxLat-minLat)/dLat)) + 1
	dstNx := int(math.Ceil((maxLon-minLon)/dLon)) + 1
	la1 := minLat
	lo1 := minLon
	if dstNx <= 1 || dstNy <= 1 {
		return nil, fmt.Errorf("ODIM: degenerate target grid %dx%d", dstNx, dstNy)
	}

	what := readODIMWhat(f)
	fallbackTime, _ := readODIMTime(what.Date, what.Time, time.Time{})

	datasets, err := listSubgroupsWithPrefix(f, "/", "dataset")
	if err != nil {
		return nil, fmt.Errorf("ODIM: list datasets: %w", err)
	}
	if len(datasets) == 0 {
		return nil, fmt.Errorf("ODIM: no /dataset* groups found")
	}

	out := make([]GRIBFile, 0, len(datasets))
	var srcBuf []uint16
	if withValues {
		srcBuf = make([]uint16, where.XSize*where.YSize)
	}
	const outMissing = float32(9999)
	for _, dsName := range datasets {
		dsPath := "/" + dsName
		dsDate, _ := f.attrStr(dsPath+"/what", "startdate")
		dsTime, _ := f.attrStr(dsPath+"/what", "starttime")
		refTime, _ := readODIMTime(dsDate, dsTime, fallbackTime)
		if refTime.IsZero() {
			refTime = time.Now().UTC()
		}

		dataNames, err := listSubgroupsWithPrefix(f, dsPath, "data")
		if err != nil {
			return nil, fmt.Errorf("ODIM: list %s: %w", dsName, err)
		}
		for _, dataName := range dataNames {
			dPath := dsPath + "/" + dataName
			whatPath := dPath + "/what"
			quantity, ok := f.attrStr(whatPath, "quantity")
			if !ok || quantity == "" {
				continue
			}

			info := resolveODIMQuantity(quantity, "")
			latitudes := make([]float64, dstNy)
			for i := 0; i < dstNy; i++ {
				latitudes[i] = la1 + float64(i)*dLat
			}
			longitudes := make([]float64, dstNx)
			for i := 0; i < dstNx; i++ {
				longitudes[i] = lo1 + float64(i)*dLon
			}

			h := GribHeader{
				ShortName:          info.ShortName,
				Units:              info.Units,
				Nx:                 dstNx,
				Ny:                 dstNy,
				La1:                la1,
				La2:                la1 + float64(dstNy-1)*dLat,
				Lo1:                lo1,
				Lo2:                lo1 + float64(dstNx-1)*dLon,
				DX:                 dLon,
				DY:                 dLat,
				Discipline:         info.D,
				ParameterCategory:  info.C,
				ParameterNumber:    info.P,
				ReferenceTime:      refTime,
				ForecastTime:       0,
				EndStep:            0,
				MissingValue:       float64(outMissing),
				TypeOfLevel:        "surface",
				Level:              0,
				BottomLevel:        0,
				DistinctLatitudes:  latitudes,
				DistinctLongitudes: longitudes,
			}

			var values []float32
			if withValues {
				gain, _ := f.attrF64(whatPath, "gain")
				offsetA, _ := f.attrF64(whatPath, "offset")
				nodata, _ := f.attrF64(whatPath, "nodata")
				undetect, _ := f.attrF64(whatPath, "undetect")
				if gain == 0 {
					gain = 1
				}
				if err := f.readU16(dPath+"/data", srcBuf); err != nil {
					return nil, fmt.Errorf("ODIM: read %s: %w", dPath+"/data", err)
				}
				// dequantise to float32 with NaN at nodata/undetect; the
				// resampler maps any NaN it lands on to outMissing.
				srcF := make([]float32, len(srcBuf))
				for i, v := range srcBuf {
					switch float64(v) {
					case nodata, undetect:
						srcF[i] = float32(math.NaN())
					default:
						srcF[i] = float32(float64(v)*gain + offsetA)
					}
				}
				values = resampleStereToLatLonNaN(srcF, srcGrid, dstNx, dstNy,
					la1, lo1, dLat, dLon, outMissing)
			}
			out = append(out, GRIBFile{Header: h, DataValues: values})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ODIM: no /dataset*/data*/data fields found")
	}
	_ = sourceName
	return out, nil
}

// nearest-neighbour resample from a stere grid into a regular lat-lon raster.
// rows are split across GOMAXPROCS — the inner stere.Forward call dominates
// per-file wall time, and each row is independent.
func resampleStereToLatLonNaN(src []float32, srcGrid stereGrid,
	dstNx, dstNy int, lat1, lon1, dLat, dLon float64,
	outMissing float32) []float32 {
	out := make([]float32, dstNx*dstNy)
	cols := srcGrid.Cols
	rows := srcGrid.Rows
	resampleRow := func(j int) {
		latRad := (lat1 + float64(j)*dLat) * math.Pi / 180
		base := j * dstNx
		for i := 0; i < dstNx; i++ {
			lonRad := (lon1 + float64(i)*dLon) * math.Pi / 180
			x, y := srcGrid.Params.Forward(latRad, lonRad)
			c, r := srcGrid.pixelAtProjXY(x, y)
			ci := int(math.Round(c))
			ri := int(math.Round(r))
			if ci < 0 || ci >= cols || ri < 0 || ri >= rows {
				out[base+i] = outMissing
				continue
			}
			v := src[ri*cols+ci]
			if v != v { // NaN
				out[base+i] = outMissing
				continue
			}
			out[base+i] = v
		}
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > dstNy {
		workers = dstNy
	}
	if workers <= 1 {
		for j := 0; j < dstNy; j++ {
			resampleRow(j)
		}
		return out
	}
	rowsPerWorker := (dstNy + workers - 1) / workers
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		j0 := w * rowsPerWorker
		j1 := j0 + rowsPerWorker
		if j1 > dstNy {
			j1 = dstNy
		}
		if j0 >= j1 {
			wg.Done()
			continue
		}
		go func(j0, j1 int) {
			defer wg.Done()
			for j := j0; j < j1; j++ {
				resampleRow(j)
			}
		}(j0, j1)
	}
	wg.Wait()
	return out
}
