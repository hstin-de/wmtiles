package encode

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/internal/scan"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/tiler"
)

// Format identifies an input data format.
type Format string

const (
	// FormatGRIB2 reads GRIB edition 2 messages through ecCodes.
	FormatGRIB2 Format = "grib2"
	// FormatHDF5 reads ODIM_H5 radar composites and CF-1.x HDF5/NetCDF4 files via
	// libhdf5. ODIM polar-stere grids are resampled to regular lat-lon at parse.
	FormatHDF5 Format = "hdf5"
)

// Options configures source-data-to-WMT encoding.
type Options struct {
	// TileSize is the tile width and height in pixels. The supported values are
	// 128, 256, 512 and 1024. The default is 256.
	TileSize int

	MinZoom uint8
	MaxZoom uint8

	// FilterVariables keeps only source variables listed here. For GRIB2 this
	// matches the message shortName. Empty means keep all variables.
	FilterVariables []string

	// Precision overrides quantisation precision by resolved WMTiles variable
	// name or by source variable name. For GRIB2 the source name is shortName. A
	// value of 0 forces full-range u16.
	Precision map[string]float64

	Metadata     map[string]any
	CreationTime time.Time

	// DisableDeltaCodec skips the slower delta/Lorenzo codec candidates.
	DisableDeltaCodec bool

	// ZstdLevel sets the per-tile libzstd level (1..22). 0 = encoder default (3).
	ZstdLevel int

	// Trades ~5-20% block size for encode CPU; rarely pays off on radar
	// where blocks are small. Honours the same heuristics as
	// encoder.Options.EnableTileDict.
	EnableTileDict bool

	// AllowDuplicateMessages lets Finish ignore repeated records/messages for
	// the same resolved variable and valid time. By default duplicates are an
	// error.
	AllowDuplicateMessages bool

	// Progress callbacks mirror encoder.Options; all are optional and nil-safe.
	OnInputScanned    func(name string, records int)
	OnScanComplete    func(stats ScanStats)
	OnFinishStats     func(plans []VariablePlan)
	OnTileSubmitted   func(submitted, skipped int64)
	OnBlockCompressed func(idx, total int, bytes uint64)
	OnBlockWritten    func(idx, total int, bytes uint64)
	OnPhase           func(stage string)
}

// ScanStats summarises what scanInputs found, fired right before tile work
// begins so the CLI can print variable count, time axis, and bbox up front.
type ScanStats struct {
	InputCount    int
	TotalMessages int
	KeptMessages  int
	VariableCount int
	TimeAxis      format.TimeCatalog
	BBox          [4]float64
	// Lets the CLI show a real denominator on the decode+tile phase
	// instead of a spinner with just a count.
	ExpectedTiles int64
}

// Aliased so library callers and the CLI agree on layout without depending
// on internal/scan directly.
type VariablePlan = scan.VariablePlan

// Encoder collects one or more source inputs and writes them as one fresh WMT
// file when Finish is called. Inputs can use different formats once this
// package supports them; currently FormatGRIB2 is implemented.
type Encoder struct {
	outPath  string
	opts     Options
	inputs   []input
	finished bool

	submitted     atomic.Int64
	skipped       atomic.Int64
	expectedTiles int64
}

// Progress returns the number of tiles submitted to and skipped by the active
// Finish call. Safe to call from any goroutine while Finish is in flight.
func (e *Encoder) Progress() (submitted, skipped int64) {
	return e.submitted.Load(), e.skipped.Load()
}

type input struct {
	name   string
	format Format
	path   string
	data   []byte

	// Cached by scanGRIBFast so streamGRIBFast doesn't re-read or re-parse;
	// cleared right after stream so multi-file encodes don't pin every
	// file's raw bytes.
	gribRanges  []parser.MessageRange
	gribHeaders []parser.GribHeader
	gribSkip    []bool
	gribData    []byte
}

// NewEncoder creates a source-data-to-WMT encoder. Inputs are added with AddFile
// and AddBytes; no WMT file is written until Finish.
func NewEncoder(outPath string, opts Options) (*Encoder, error) {
	if outPath == "" {
		return nil, errors.New("wmtiles/encode: output path is empty")
	}
	if _, err := tileSize(opts.TileSize); err != nil {
		return nil, err
	}
	if opts.MaxZoom < opts.MinZoom {
		return nil, fmt.Errorf("wmtiles/encode: max zoom %d < min zoom %d", opts.MaxZoom, opts.MinZoom)
	}
	for name, precision := range opts.Precision {
		if precision < 0 || math.IsNaN(precision) || math.IsInf(precision, 0) {
			return nil, fmt.Errorf("wmtiles/encode: precision %q must be finite and >= 0, got %g", name, precision)
		}
	}
	return &Encoder{outPath: outPath, opts: opts}, nil
}

// AddFile adds a source file path to the pending input set.
func (e *Encoder) AddFile(path string, format Format) error {
	if e.finished {
		return errors.New("wmtiles/encode: Encoder already finished")
	}
	if path == "" {
		return errors.New("wmtiles/encode: input path is empty")
	}
	if err := validateFormat(format); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	e.inputs = append(e.inputs, input{name: path, format: format, path: path})
	return nil
}

// AddBytes adds source bytes to the pending input set. The data slice must not be
// modified until Finish returns.
func (e *Encoder) AddBytes(name string, format Format, data []byte) error {
	if e.finished {
		return errors.New("wmtiles/encode: Encoder already finished")
	}
	if len(data) == 0 {
		return errors.New("wmtiles/encode: input bytes are empty")
	}
	if err := validateFormat(format); err != nil {
		return err
	}
	if name == "" {
		name = "memory." + string(format)
	}
	e.inputs = append(e.inputs, input{name: name, format: format, data: data})
	return nil
}

// Finish scans all pending input records, merges them into one variable/time
// catalog, tiles the matching records, and writes one fresh WMT file.
func (e *Encoder) Finish() error {
	if e.finished {
		return errors.New("wmtiles/encode: Encoder already finished")
	}
	e.finished = true
	if len(e.inputs) == 0 {
		return errors.New("wmtiles/encode: no inputs added")
	}

	bySig, times, bounds, _, kept, err := e.scanInputs()
	if err != nil {
		return err
	}
	if kept == 0 {
		if len(e.opts.FilterVariables) > 0 {
			return errors.New("wmtiles/encode: variable filter matched no records")
		}
		return errors.New("wmtiles/encode: no input records found")
	}
	if len(times) == 0 {
		return errors.New("wmtiles/encode: no valid times found")
	}

	specs := e.preliminaryVariableSpecs(bySig)
	timeIdxByTime := make(map[time.Time]uint32, len(times))
	for i, t := range times {
		timeIdxByTime[t] = uint32(i)
	}

	pixSize, err := tileSize(e.opts.TileSize)
	if err != nil {
		return err
	}
	tileSizeLog2, err := tileSizeLog2(pixSize)
	if err != nil {
		return err
	}
	if err := validateTimes(times); err != nil {
		return err
	}
	refTime := times[0]
	timeCat := timeCatalogFromTimes(times)
	bboxArr := [4]float64{bounds.West, bounds.South, bounds.East, bounds.North}

	if e.opts.OnScanComplete != nil {
		e.opts.OnScanComplete(ScanStats{
			InputCount:    len(e.inputs),
			TotalMessages: kept,
			KeptMessages:  kept,
			VariableCount: len(bySig),
			TimeAxis:      timeCat,
			BBox:          bboxArr,
			ExpectedTiles: e.expectedTiles,
		})
	}
	enc, err := encoder.NewStreamingEncoder(encoder.Options{
		TilePixelSizeLog2:     tileSizeLog2,
		MinZoom:               e.opts.MinZoom,
		MaxZoom:               e.opts.MaxZoom,
		ReferenceForecastTime: refTime,
		TimeCatalog:           timeCat,
		BBox:                  bboxArr,
		Variables:             specs,
		Metadata:              e.metadata(kept),
		CreationTime:          e.opts.CreationTime,
		OnPixelsConsumed:      tiler.PutTileBuf,
		DisableDeltaCodec:     e.opts.DisableDeltaCodec,
		ZstdLevel:             e.opts.ZstdLevel,
		EnableTileDict:        e.opts.EnableTileDict,
		OnBlockCompressed:     e.opts.OnBlockCompressed,
		OnBlockWritten:        e.opts.OnBlockWritten,
		OnPhase:               e.opts.OnPhase,
		// DirectWorker pumps quantize+codec inline on our parser
		// goroutines; the encoder's channel pipeline would add a
		// hop without buying anything here.
		SkipInternalWorkers: true,
	}, e.outPath)
	if err != nil {
		return fmt.Errorf("wmtiles/encode: encoder init: %w", err)
	}

	if err := e.streamTiles(bySig, timeIdxByTime, enc, pixSize); err != nil {
		enc.Close()
		return err
	}

	if e.opts.OnFinishStats != nil {
		e.opts.OnFinishStats(scan.FinalVariablePlans(bySig, e.opts.Precision))
	}

	if err := enc.Finish(); err != nil {
		return fmt.Errorf("wmtiles/encode: encode: %w", err)
	}
	return nil
}

func (e *Encoder) metadata(messageCount int) map[string]any {
	md := make(map[string]any, len(e.opts.Metadata)+3)
	for k, v := range e.opts.Metadata {
		md[k] = v
	}
	if _, ok := md["sources"]; !ok {
		names := make([]map[string]string, len(e.inputs))
		for i, in := range e.inputs {
			names[i] = map[string]string{"name": in.name, "format": string(in.format)}
		}
		md["sources"] = names
	}
	if _, ok := md["inputCount"]; !ok {
		md["inputCount"] = len(e.inputs)
	}
	if _, ok := md["messageCount"]; !ok {
		md["messageCount"] = messageCount
	}
	return md
}

type scanBounds struct {
	West, South, East, North float64
}

// boundsInit prevents a no-kept-message scan from leaving (180,90,-180,-90)
// sentinels in the published bbox.
type scanState struct {
	bySig            map[scan.VarKey]*scan.VarInfo
	timesSeen        map[time.Time]struct{}
	bounds           scanBounds
	boundsInit       bool
	totalSeen        int
	keptSeen         int
	expectedTiles    int64
	minZoom, maxZoom uint8
}

func (e *Encoder) scanInputs() (
	bySig map[scan.VarKey]*scan.VarInfo,
	allTimes []time.Time,
	bounds scanBounds,
	totalSeen, keptSeen int,
	err error,
) {
	filter := e.filterSet()
	st := &scanState{
		bySig:     map[scan.VarKey]*scan.VarInfo{},
		timesSeen: map[time.Time]struct{}{},
		minZoom:   e.opts.MinZoom,
		maxZoom:   e.opts.MaxZoom,
	}

	for i := range e.inputs {
		in := &e.inputs[i]
		beforeKept := st.keptSeen

		switch {
		case in.format == FormatGRIB2 && in.path != "":
			err = e.scanGRIBFast(in, filter, st)
		case in.format == FormatGRIB2 && in.data != nil:
			err = e.scanGRIBBytesFast(in, filter, st)
		default:
			err = e.scanInputSequential(in, filter, st)
		}
		if err != nil {
			return nil, nil, scanBounds{}, st.totalSeen, st.keptSeen, fmt.Errorf("wmtiles/encode: scan %s: %w", in.name, err)
		}
		if e.opts.OnInputScanned != nil {
			e.opts.OnInputScanned(in.name, st.keptSeen-beforeKept)
		}
	}

	scan.DisambiguateNames(st.bySig, func(k scan.VarKey) string {
		return fmt.Sprintf("_%d_%d_%d", k.Discipline, k.Category, k.Parameter)
	})
	scan.DisambiguateNames(st.bySig, func(k scan.VarKey) string {
		return fmt.Sprintf("_%s_%d_%d", k.LevelType, k.Level, k.BottomLevel)
	})

	allTimes = make([]time.Time, 0, len(st.timesSeen))
	for t := range st.timesSeen {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i].Before(allTimes[j]) })
	e.expectedTiles = st.expectedTiles
	return st.bySig, allTimes, st.bounds, st.totalSeen, st.keptSeen, nil
}

// HDF5 lives here permanently (libhdf5 is single-threaded); GRIB only falls
// back here when scanGRIBFast can't be used (byte buffers, etc).
func (e *Encoder) scanInputSequential(in *input, filter map[string]bool, st *scanState) error {
	keepShortName := func(shortName string) bool {
		st.totalSeen++
		if len(filter) == 0 {
			return true
		}
		return filter[shortName]
	}
	return in.forEachHeaderFiltered(keepShortName, func(h parser.GribHeader) error {
		st.keptSeen++
		st.accumulate(&h)
		return nil
	})
}

// Every scan path goes through here so bySig / bounds / timesSeen stay in
// sync no matter which decoder produced the header.
func (st *scanState) accumulate(h *parser.GribHeader) {
	k := scan.VarKeyOf(h)
	v, ok := st.bySig[k]
	if !ok {
		v = scan.NewVarInfoFor(h, k)
		st.bySig[k] = v
	}
	v.MessageCount++
	v.Times[h.ReferenceTime] = struct{}{}

	shell := parser.GRIBFile{Header: *h}
	west, south, east, north := tiler.GridBBox(&shell)
	if !st.boundsInit {
		st.bounds = scanBounds{West: west, South: south, East: east, North: north}
		st.boundsInit = true
	} else {
		if west < st.bounds.West {
			st.bounds.West = west
		}
		if south < st.bounds.South {
			st.bounds.South = south
		}
		if east > st.bounds.East {
			st.bounds.East = east
		}
		if north > st.bounds.North {
			st.bounds.North = north
		}
	}
	st.timesSeen[h.ReferenceTime] = struct{}{}
	for z := st.minZoom; z <= st.maxZoom; z++ {
		st.expectedTiles += int64(len(tiler.TilesIntersectingGrid(&shell, z)))
	}
}

func (e *Encoder) filterSet() map[string]bool {
	if len(e.opts.FilterVariables) == 0 {
		return nil
	}
	out := make(map[string]bool, len(e.opts.FilterVariables))
	for _, name := range e.opts.FilterVariables {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func (e *Encoder) preliminaryVariableSpecs(bySig map[scan.VarKey]*scan.VarInfo) []encoder.VariableSpec {
	infos := sortedVarInfos(bySig)
	specs := make([]encoder.VariableSpec, 0, len(infos))
	for _, v := range infos {
		var precision float64
		if p, ok := e.opts.Precision[v.Name]; ok {
			precision = p
		} else if p, ok := e.opts.Precision[v.ShortName]; ok {
			precision = p
		} else if p := scan.DefaultPrecisionFor(v.ShortName, v.Unit); p > 0 {
			precision = p
		}
		specs = append(specs, encoder.VariableSpec{
			Name:      v.Name,
			Unit:      v.Unit,
			Precision: precision,
		})
	}
	return specs
}

type tileWork struct {
	name string
	tIdx uint32
	z    uint8
	x, y uint32
	s    *tiler.Sampler
}

type vtKey struct {
	k    scan.VarKey
	tIdx uint32
}

// One interface so the same stream code feeds both fresh files
// (StreamingEncoder) and appends (AppendCtx).
type streamSink interface {
	DeclareBlock(encoder.BlockSpec) error
	NewDirectWorker() (*encoder.DirectWorker, error)
}

// Cached GRIB inputs go through the parallel fast path; HDF5 and byte
// buffers stay on the sequential eccodes path.
func (e *Encoder) streamTiles(
	bySig map[scan.VarKey]*scan.VarInfo,
	timeIdxByTime map[time.Time]uint32,
	sink streamSink,
	pixSize int,
) error {
	for i := range e.inputs {
		in := &e.inputs[i]
		var err error
		if in.format == FormatGRIB2 && in.gribData != nil {
			err = e.streamGRIBFast(in, bySig, timeIdxByTime, sink, pixSize, e.opts.MinZoom, e.opts.MaxZoom)
		} else {
			err = e.streamInputSequential(in, bySig, timeIdxByTime, sink, pixSize)
		}
		if err != nil {
			return fmt.Errorf("wmtiles/encode: stream %s: %w", in.name, err)
		}
	}
	return nil
}

// Parser is single-threaded here (libhdf5 isn't reentrant); the worker pool
// only fans out the per-tile codec stage.
func (e *Encoder) streamInputSequential(
	in *input,
	bySig map[scan.VarKey]*scan.VarInfo,
	timeIdxByTime map[time.Time]uint32,
	sink streamSink,
	pixSize int,
) error {
	numWorkers := max(runtime.GOMAXPROCS(0), 1)
	workCh := make(chan tileWork, numWorkers*4)
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	getErr := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr
	}

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer wg.Done()
			dw, dwErr := sink.NewDirectWorker()
			if dwErr != nil {
				setErr(dwErr)
				return
			}
			defer dw.Close()
			for w := range workCh {
				if getErr() != nil {
					continue
				}
				px := tiler.Tile(w.s, w.z, w.x, w.y, pixSize)
				if px == nil {
					e.skipped.Add(1)
					continue
				}
				if err := dw.SubmitDirect(encoder.Tile{
					Variable: w.name, TimeStep: w.tIdx,
					Z: w.z, X: w.x, Y: w.y, Pixels: px,
				}); err != nil {
					tiler.PutTileBuf(px)
					setErr(err)
					continue
				}
				e.submitted.Add(1)
			}
		}()
	}

	declared := map[vtKey]string{}
	parseErr := in.forEachMessageFiltered(
		func(h *parser.GribHeader) bool {
			_, ok := bySig[scan.VarKeyOf(h)]
			return ok
		},
		func(msg parser.GRIBFile) error {
			if err := getErr(); err != nil {
				return err
			}
			k := scan.VarKeyOf(&msg.Header)
			v, ok := bySig[k]
			if !ok {
				return nil
			}
			tIdx, ok := timeIdxByTime[msg.Header.ReferenceTime]
			if !ok {
				return fmt.Errorf("message time %s not in merged time axis", msg.Header.ReferenceTime)
			}
			vt := vtKey{k: k, tIdx: tIdx}
			if prev, dup := declared[vt]; dup {
				if e.opts.AllowDuplicateMessages {
					return nil
				}
				return fmt.Errorf("duplicate GRIB message for variable %q time %s in %s (already seen in %s)",
					v.Name, msg.Header.ReferenceTime.UTC().Format(time.RFC3339), in.name, prev)
			}

			vmin, vmax, hasFinite := scan.FiniteRange(msg.DataValues, msg.Header.MissingValue)
			if !hasFinite {
				declared[vt] = in.name
				return nil
			}
			if vmin < v.VMin {
				v.VMin = vmin
			}
			if vmax > v.VMax {
				v.VMax = vmax
			}
			v.HasFinite = true

			precision, src := scan.ResolveBlockPrecision(v, vmin, vmax, e.opts.Precision)
			if v.PrecSources == nil {
				v.PrecSources = map[string]struct{}{}
			}
			v.PrecSources[src] = struct{}{}
			if err := sink.DeclareBlock(encoder.BlockSpec{
				Variable:  v.Name,
				TimeStep:  tIdx,
				ValueMin:  vmin,
				ValueMax:  vmax,
				Precision: precision,
			}); err != nil {
				return fmt.Errorf("wmtiles/encode: declare block %q t=%d: %w", v.Name, tIdx, err)
			}
			declared[vt] = in.name

			msgCopy := msg
			s := tiler.NewSampler(&msgCopy)
			if s == nil {
				return fmt.Errorf("variable %q time %s in %s: malformed grid",
					v.Name, msg.Header.ReferenceTime.UTC().Format(time.RFC3339), in.name)
			}
			for z := e.opts.MinZoom; z <= e.opts.MaxZoom; z++ {
				for _, c := range tiler.TilesIntersectingGrid(&msgCopy, z) {
					workCh <- tileWork{
						name: v.Name,
						tIdx: tIdx,
						z:    z,
						x:    c.X,
						y:    c.Y,
						s:    s,
					}
				}
			}
			return nil
		})
	close(workCh)
	wg.Wait()
	if parseErr != nil {
		return parseErr
	}
	if err := getErr(); err != nil {
		return fmt.Errorf("wmtiles/encode: tile stream: %w", err)
	}
	return nil
}

func (in input) forEachHeaderFiltered(want func(shortName string) bool, fn func(parser.GribHeader) error) error {
	switch in.format {
	case FormatGRIB2:
		if in.path != "" {
			return parser.ForEachMessageHeaderFiltered(in.path, want, fn)
		}
		return parser.ForEachMessageHeaderBytesFiltered(in.data, want, fn)
	case FormatHDF5:
		if in.path != "" {
			return parser.ForEachHDF5HeaderFiltered(in.path, want, fn)
		}
		return parser.ForEachHDF5HeaderBytesFiltered(in.data, want, fn)
	default:
		return unsupportedFormatError(in.format)
	}
}

func (in input) forEachMessageFiltered(want func(*parser.GribHeader) bool, fn func(parser.GRIBFile) error) error {
	switch in.format {
	case FormatGRIB2:
		if in.path != "" {
			return parser.ForEachMessageFiltered(in.path, want, fn)
		}
		return parser.ForEachMessageBytesFiltered(in.data, want, fn)
	case FormatHDF5:
		if in.path != "" {
			return parser.ForEachHDF5MessageFiltered(in.path, want, fn)
		}
		return parser.ForEachHDF5MessageBytesFiltered(in.data, want, fn)
	default:
		return unsupportedFormatError(in.format)
	}
}

func sortedVarInfos(bySig map[scan.VarKey]*scan.VarInfo) []*scan.VarInfo {
	out := make([]*scan.VarInfo, 0, len(bySig))
	for _, v := range bySig {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func tileSize(size int) (int, error) {
	if size == 0 {
		return 256, nil
	}
	switch size {
	case 128, 256, 512, 1024:
		return size, nil
	default:
		return 0, fmt.Errorf("wmtiles/encode: unsupported tile size %d; use 128, 256, 512 or 1024", size)
	}
}

func validateFormat(format Format) error {
	switch format {
	case FormatGRIB2, FormatHDF5:
		return nil
	default:
		return unsupportedFormatError(format)
	}
}

func unsupportedFormatError(format Format) error {
	if format == "" {
		return errors.New("wmtiles/encode: input format is empty")
	}
	return fmt.Errorf("wmtiles/encode: unsupported input format %q", format)
}

