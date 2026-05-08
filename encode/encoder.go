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
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/tiler"
)

// Format identifies an input data format.
type Format string

const (
	// FormatGRIB2 reads GRIB edition 2 messages through ecCodes.
	FormatGRIB2 Format = "grib2"
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

	// AllowDuplicateMessages lets Finish ignore repeated records/messages for
	// the same resolved variable and valid time. By default duplicates are an
	// error.
	AllowDuplicateMessages bool
}

// Encoder collects one or more source inputs and writes them as one fresh WMT
// file when Finish is called. Inputs can use different formats once this
// package supports them; currently FormatGRIB2 is implemented.
type Encoder struct {
	outPath  string
	opts     Options
	inputs   []input
	finished bool
}

type input struct {
	name   string
	format Format
	path   string
	data   []byte
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
	enc, err := encoder.NewStreamingEncoder(encoder.Options{
		TilePixelSizeLog2:     tileSizeLog2,
		MinZoom:               e.opts.MinZoom,
		MaxZoom:               e.opts.MaxZoom,
		ReferenceForecastTime: refTime,
		TimeCatalog:           timeCatalogFromTimes(times),
		BBox:                  [4]float64{bounds.West, bounds.South, bounds.East, bounds.North},
		Variables:             specs,
		Metadata:              e.metadata(kept),
		CreationTime:          e.opts.CreationTime,
		OnPixelsConsumed:      tiler.PutTileBuf,
		DisableDeltaCodec:     e.opts.DisableDeltaCodec,
	}, e.outPath)
	if err != nil {
		return fmt.Errorf("wmtiles/encode: encoder init: %w", err)
	}

	if err := e.streamTiles(bySig, timeIdxByTime, enc, pixSize); err != nil {
		enc.Close()
		return err
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

type varKey struct {
	d, c, p     int
	levelType   string
	level       int
	bottomLevel int
}

func varKeyOf(h *parser.GribHeader) varKey {
	return varKey{
		d:           h.Discipline,
		c:           h.ParameterCategory,
		p:           h.ParameterNumber,
		levelType:   h.TypeOfLevel,
		level:       h.Level,
		bottomLevel: h.BottomLevel,
	}
}

type varInfo struct {
	name         string
	shortName    string
	unit         string
	vmin         float64
	vmax         float64
	hasFinite    bool
	messageCount int
	times        map[time.Time]struct{}
}

type scanBounds struct {
	West, South, East, North float64
}

func (e *Encoder) scanInputs() (
	bySig map[varKey]*varInfo,
	allTimes []time.Time,
	bounds scanBounds,
	totalSeen, keptSeen int,
	err error,
) {
	filter := e.filterSet()
	keepShortName := func(shortName string) bool {
		totalSeen++
		if len(filter) == 0 {
			return true
		}
		return filter[shortName]
	}

	bySig = map[varKey]*varInfo{}
	timesSeen := map[time.Time]struct{}{}
	boundsInit := false
	for _, in := range e.inputs {
		err = in.forEachHeaderFiltered(keepShortName, func(h parser.GribHeader) error {
			keptSeen++

			k := varKeyOf(&h)
			v, ok := bySig[k]
			if !ok {
				base := h.ShortName
				if base == "" || base == "unknown" {
					base = fmt.Sprintf("param_%d_%d_%d", k.d, k.c, k.p)
				}
				v = &varInfo{
					name:      base + levelSuffix(k.levelType, k.level, k.bottomLevel),
					shortName: h.ShortName,
					unit:      h.Units,
					vmin:      math.Inf(+1),
					vmax:      math.Inf(-1),
					times:     map[time.Time]struct{}{},
				}
				bySig[k] = v
			}
			v.messageCount++
			v.times[h.ReferenceTime] = struct{}{}

			shell := parser.GRIBFile{Header: h}
			west, south, east, north := tiler.GridBBox(&shell)
			if !boundsInit {
				bounds = scanBounds{West: west, South: south, East: east, North: north}
				boundsInit = true
			} else {
				if west < bounds.West {
					bounds.West = west
				}
				if south < bounds.South {
					bounds.South = south
				}
				if east > bounds.East {
					bounds.East = east
				}
				if north > bounds.North {
					bounds.North = north
				}
			}

			timesSeen[h.ReferenceTime] = struct{}{}
			return nil
		})
		if err != nil {
			return nil, nil, scanBounds{}, totalSeen, keptSeen, fmt.Errorf("wmtiles/encode: scan %s: %w", in.name, err)
		}
	}

	disambiguate := func(suffix func(varKey) string) {
		counts := map[string]int{}
		for _, v := range bySig {
			counts[v.name]++
		}
		for k, v := range bySig {
			if counts[v.name] > 1 {
				v.name += suffix(k)
			}
		}
	}
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%d_%d_%d", k.d, k.c, k.p)
	})
	disambiguate(func(k varKey) string {
		return fmt.Sprintf("_%s_%d_%d", k.levelType, k.level, k.bottomLevel)
	})

	allTimes = make([]time.Time, 0, len(timesSeen))
	for t := range timesSeen {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i].Before(allTimes[j]) })
	return bySig, allTimes, bounds, totalSeen, keptSeen, nil
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

func (e *Encoder) preliminaryVariableSpecs(bySig map[varKey]*varInfo) []encoder.VariableSpec {
	infos := sortedVarInfos(bySig)
	specs := make([]encoder.VariableSpec, 0, len(infos))
	for _, v := range infos {
		var precision float64
		if p, ok := e.opts.Precision[v.name]; ok {
			precision = p
		} else if p, ok := e.opts.Precision[v.shortName]; ok {
			precision = p
		} else if p := defaultPrecisionFor(v.shortName, v.unit); p > 0 {
			precision = p
		}
		specs = append(specs, encoder.VariableSpec{
			Name:      v.name,
			Unit:      v.unit,
			Precision: precision,
		})
	}
	return specs
}

const autoPrecisionSteps = 1024

type tileWork struct {
	name string
	tIdx uint32
	z    uint8
	x, y uint32
	s    *tiler.Sampler
}

type vtKey struct {
	k    varKey
	tIdx uint32
}

// streamTiles parses each message once, computes (vmin, vmax) from the values
// array, declares the block just-in-time, then queues tiles. DeclareBlock can
// safely interleave with Submit: the streaming encoder's blockMu serialises
// declarations against the read locks Submit takes.
func (e *Encoder) streamTiles(
	bySig map[varKey]*varInfo,
	timeIdxByTime map[time.Time]uint32,
	enc *encoder.StreamingEncoder,
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
			for w := range workCh {
				if getErr() != nil {
					continue
				}
				px := tiler.Tile(w.s, w.z, w.x, w.y, pixSize)
				if px == nil {
					continue
				}
				if err := enc.Submit(encoder.Tile{
					Variable: w.name, TimeStep: w.tIdx,
					Z: w.z, X: w.x, Y: w.y, Pixels: px,
				}); err != nil {
					tiler.PutTileBuf(px)
					setErr(err)
					continue
				}
			}
		}()
	}

	declared := map[vtKey]string{}
	var parseErr error
	for _, in := range e.inputs {
		if getErr() != nil {
			break
		}
		err := in.forEachMessageFiltered(
			func(h *parser.GribHeader) bool {
				_, ok := bySig[varKeyOf(h)]
				return ok
			},
			func(msg parser.GRIBFile) error {
				if err := getErr(); err != nil {
					return err
				}
				k := varKeyOf(&msg.Header)
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
						v.name, msg.Header.ReferenceTime.UTC().Format(time.RFC3339), in.name, prev)
				}

				vmin, vmax, hasFinite := finiteRange(msg.DataValues, msg.Header.MissingValue)
				if !hasFinite {
					declared[vt] = in.name
					return nil
				}
				if vmin < v.vmin {
					v.vmin = vmin
				}
				if vmax > v.vmax {
					v.vmax = vmax
				}
				v.hasFinite = true

				precision := e.resolveBlockPrecision(v, vmin, vmax)
				if err := enc.DeclareBlock(encoder.BlockSpec{
					Variable:  v.name,
					TimeStep:  tIdx,
					ValueMin:  vmin,
					ValueMax:  vmax,
					Precision: precision,
				}); err != nil {
					return fmt.Errorf("wmtiles/encode: declare block %q t=%d: %w", v.name, tIdx, err)
				}
				declared[vt] = in.name

				msgCopy := msg
				s := tiler.NewSampler(&msgCopy)
				if s == nil {
					return fmt.Errorf("variable %q time %s in %s: malformed grid",
						v.name, msg.Header.ReferenceTime.UTC().Format(time.RFC3339), in.name)
				}
				for z := e.opts.MinZoom; z <= e.opts.MaxZoom; z++ {
					for _, c := range tiler.TilesIntersectingGrid(&msgCopy, z) {
						workCh <- tileWork{
							name: v.name,
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
		if err != nil {
			parseErr = fmt.Errorf("wmtiles/encode: stream %s: %w", in.name, err)
			break
		}
	}
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

func (e *Encoder) resolveBlockPrecision(v *varInfo, vmin, vmax float64) float64 {
	if p, ok := e.opts.Precision[v.name]; ok {
		return p
	}
	if p, ok := e.opts.Precision[v.shortName]; ok {
		return p
	}
	if p := defaultPrecisionFor(v.shortName, v.unit); p > 0 {
		return p
	}
	if vmax > vmin {
		return (vmax - vmin) / autoPrecisionSteps
	}
	return 0
}

func finiteRange(values []float64, missing float64) (vmin, vmax float64, ok bool) {
	vmin = math.Inf(+1)
	vmax = math.Inf(-1)
	for _, v := range values {
		if v != v || v == missing {
			continue
		}
		if v < vmin {
			vmin = v
		}
		if v > vmax {
			vmax = v
		}
		ok = true
	}
	return
}

func (in input) forEachHeaderFiltered(want func(shortName string) bool, fn func(parser.GribHeader) error) error {
	switch in.format {
	case FormatGRIB2:
		if in.path != "" {
			return parser.ForEachMessageHeaderFiltered(in.path, want, fn)
		}
		return parser.ForEachMessageHeaderBytesFiltered(in.data, want, fn)
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
	default:
		return unsupportedFormatError(in.format)
	}
}

func sortedVarInfos(bySig map[varKey]*varInfo) []*varInfo {
	out := make([]*varInfo, 0, len(bySig))
	for _, v := range bySig {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
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
	case FormatGRIB2:
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

func levelSuffix(levelType string, level, bottomLevel int) string {
	switch levelType {
	case "", "surface":
		return ""
	case "heightAboveGround":
		return fmt.Sprintf("_%dm", level)
	case "isobaricInhPa":
		return fmt.Sprintf("_%dhpa", level)
	case "isobaricInPa":
		return fmt.Sprintf("_%dpa", level)
	case "meanSea":
		return "_msl"
	case "entireAtmosphere":
		return "_atm"
	case "atmosphereSingleLayer":
		return "_atm"
	case "nominalTop":
		return "_ntat"
	case "depthBelowLandLayer":
		if bottomLevel > level {
			return fmt.Sprintf("_%d-%dcm", level, bottomLevel)
		}
		return fmt.Sprintf("_%dcm", level)
	case "depthBelowLand":
		return fmt.Sprintf("_%dcm", level)
	case "lowCloudLayer":
		return "_lowcld"
	case "middleCloudLayer":
		return "_midcld"
	case "highCloudLayer":
		return "_highcld"
	case "cloudBase":
		return "_cldbase"
	case "cloudTop":
		return "_cldtop"
	default:
		if bottomLevel != 0 && bottomLevel != level {
			return fmt.Sprintf("_%s_%d_%d", sanitizeName(levelType), level, bottomLevel)
		}
		if level != 0 {
			return fmt.Sprintf("_%s_%d", sanitizeName(levelType), level)
		}
		return "_" + sanitizeName(levelType)
	}
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}
