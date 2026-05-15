package encode

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/hstin-de/wmtiles/encoder"
	"github.com/hstin-de/wmtiles/internal/scan"
)

type AppenderOptions struct {
	// Without AllowReplace, hitting an existing (variable, time) block is
	// an error; useful as a default so reruns don't silently clobber.
	AllowReplace bool

	FilterVariables []string

	Precision map[string]float64

	DisableDeltaCodec bool
	ZstdLevel         int
	EnableTileDict    bool

	// Off by default so reruns over the same source surface explicitly.
	AllowDuplicateMessages bool

	OnInputScanned    func(name string, records int)
	OnScanComplete    func(stats ScanStats)
	OnFinishStats     func(plans []VariablePlan)
	OnBlockCompressed func(idx, total int, bytes uint64)
	OnBlockWritten    func(idx, total int, bytes uint64)
	OnPhase           func(stage string)
	// Per existing-block collision; the tally also lives in Stats().AlreadyPresent.
	OnDuplicate func(name string)
}

type Appender struct {
	wmtPath  string
	opts     AppenderOptions
	inputs   []input
	finished bool

	// See Encoder.arrayVarSeq.
	arrayVarSeq map[string]int

	// Finish parks the shadow Encoder here so Progress() can read the live
	// stream counters without aliasing atomics across structs.
	live atomic.Pointer[Encoder]

	submitted      atomic.Int64
	skipped        atomic.Int64
	alreadyPresent atomic.Int64
	expectedTiles  int64
}

type AppendStats struct {
	Submitted      int64
	Skipped        int64
	AlreadyPresent int64
}

func (a *Appender) Progress() (submitted, skipped int64) {
	if s := a.live.Load(); s != nil {
		return s.submitted.Load(), s.skipped.Load()
	}
	return a.submitted.Load(), a.skipped.Load()
}

func (a *Appender) Stats() AppendStats {
	sub, sk := a.Progress()
	return AppendStats{Submitted: sub, Skipped: sk, AlreadyPresent: a.alreadyPresent.Load()}
}

// encoder.OpenForAppend is deferred to Finish so AddFile errors don't tie up the lock.
func NewAppender(wmtPath string, opts AppenderOptions) (*Appender, error) {
	if wmtPath == "" {
		return nil, errors.New("wmtiles/encode: append path is empty")
	}
	if _, err := os.Stat(wmtPath); err != nil {
		return nil, err
	}
	return &Appender{wmtPath: wmtPath, opts: opts}, nil
}

func (a *Appender) AddFile(path string, format Format) error {
	if a.finished {
		return errors.New("wmtiles/encode: Appender already finished")
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
	a.inputs = append(a.inputs, input{name: path, format: format, path: path})
	return nil
}

func (a *Appender) AddBytes(name string, format Format, data []byte) error {
	if a.finished {
		return errors.New("wmtiles/encode: Appender already finished")
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
	a.inputs = append(a.inputs, input{name: name, format: format, data: data})
	return nil
}

func (a *Appender) Finish() error {
	if a.finished {
		return errors.New("wmtiles/encode: Appender already finished")
	}
	a.finished = true
	if len(a.inputs) == 0 {
		return errors.New("wmtiles/encode: no inputs added")
	}
	defer func() { releaseGribCaches(a.inputs) }()

	// Borrow the fresh-Encoder's parallel scan so append and fresh stay in
	// lockstep on variable naming + bbox math.
	scanOpts := Options{
		// Real values pulled from the existing file below.
		MinZoom:                0,
		MaxZoom:                0,
		FilterVariables:        a.opts.FilterVariables,
		Precision:              a.opts.Precision,
		AllowDuplicateMessages: a.opts.AllowDuplicateMessages,
		OnInputScanned:         a.opts.OnInputScanned,
	}
	shadow := &Encoder{outPath: "", opts: scanOpts, inputs: a.inputs}
	bySig, allTimes, _, _, kept, err := shadow.scanInputs()
	if err != nil {
		return err
	}
	if kept == 0 {
		if len(a.opts.FilterVariables) > 0 {
			return errors.New("wmtiles/encode: variable filter matched no records")
		}
		return errors.New("wmtiles/encode: no input records found")
	}
	if len(allTimes) == 0 {
		return errors.New("wmtiles/encode: no valid times found")
	}
	// scanInputs filled per-input GRIB caches on shadow.inputs; reuse them.
	a.inputs = shadow.inputs
	a.expectedTiles = shadow.expectedTiles

	ctx, err := encoder.OpenForAppend(a.wmtPath, encoder.AppendOptions{
		AllowReplace:        a.opts.AllowReplace,
		ZstdLevel:           a.opts.ZstdLevel,
		DisableDeltaCodec:   a.opts.DisableDeltaCodec,
		EnableTileDict:      a.opts.EnableTileDict,
		SkipInternalWorkers: true,
		OnBlockCompressed:   a.opts.OnBlockCompressed,
		OnBlockWritten:      a.opts.OnBlockWritten,
		OnPhase:             a.opts.OnPhase,
	})
	if err != nil {
		return fmt.Errorf("wmtiles/encode: open for append: %w", err)
	}

	// Tile geometry must match the existing file or readers can't index.
	shadow.opts.MinZoom = ctx.MinZoom()
	shadow.opts.MaxZoom = ctx.MaxZoom()
	pixSize := ctx.PixelSize()

	for _, v := range bySig {
		spec := encoder.VariableSpec{Name: v.Name, Unit: v.Unit}
		if p, ok := a.opts.Precision[v.Name]; ok {
			spec.Precision = p
		} else if p, ok := a.opts.Precision[v.ShortName]; ok {
			spec.Precision = p
		} else if p := scan.DefaultPrecisionFor(v.ShortName, v.Unit); p > 0 {
			spec.Precision = p
		}
		if _, err := ctx.RegisterVariable(spec); err != nil {
			ctx.Close()
			return fmt.Errorf("wmtiles/encode: register variable %q: %w", v.Name, err)
		}
	}

	timeIdxByTime := make(map[time.Time]uint32, len(allTimes))
	for _, t := range allTimes {
		idx, err := ctx.RegisterTimeStep(t.UnixMilli())
		if err != nil {
			ctx.Close()
			return fmt.Errorf("wmtiles/encode: register time %s: %w", t, err)
		}
		timeIdxByTime[t] = idx
	}

	if a.opts.OnScanComplete != nil {
		a.opts.OnScanComplete(ScanStats{
			InputCount:    len(a.inputs),
			TotalMessages: kept,
			KeptMessages:  kept,
			VariableCount: len(bySig),
			ExpectedTiles: shadow.expectedTiles,
		})
	}

	// Collisions on (var, time) are common (reruns, partial backfills); the
	// wrapper turns "already exists" into a counter instead of an error so
	// the rest of the source still streams.
	sink := &appendSinkWrap{ctx: ctx, already: &a.alreadyPresent, onDup: a.opts.OnDuplicate}

	a.live.Store(shadow)
	streamErr := shadow.streamTiles(bySig, timeIdxByTime, sink, pixSize)
	a.submitted.Store(shadow.submitted.Load())
	a.skipped.Store(shadow.skipped.Load())
	a.live.Store(nil)

	if streamErr != nil {
		ctx.Close()
		return streamErr
	}

	if a.opts.OnFinishStats != nil {
		a.opts.OnFinishStats(scan.FinalVariablePlans(bySig, a.opts.Precision))
	}

	if err := ctx.Finish(); err != nil {
		return fmt.Errorf("wmtiles/encode: commit append: %w", err)
	}
	return nil
}

type appendSinkWrap struct {
	ctx     *encoder.AppendCtx
	already *atomic.Int64
	onDup   func(name string)
}

func (a *appendSinkWrap) DeclareBlock(spec encoder.BlockSpec) error {
	if err := a.ctx.DeclareBlock(spec); err != nil {
		if isAlreadyExists(err) {
			a.already.Add(1)
			if a.onDup != nil {
				a.onDup(spec.Variable)
			}
			return nil
		}
		return err
	}
	return nil
}

func (a *appendSinkWrap) NewDirectWorker() (*encoder.DirectWorker, error) {
	return a.ctx.NewDirectWorker()
}

func (a *appendSinkWrap) FlushPendingBlocks() error {
	return a.ctx.FlushPendingBlocks()
}

func (a *appendSinkWrap) EncodeRawGridBlock(spec encoder.RawGridSpec, values []float32) error {
	if err := a.ctx.EncodeRawGridBlock(spec, values); err != nil {
		if isAlreadyExists(err) {
			a.already.Add(1)
			if a.onDup != nil {
				a.onDup(spec.Variable)
			}
			return nil
		}
		return err
	}
	return nil
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for i := 0; i+14 <= len(s); i++ {
		if s[i:i+14] == "already exists" {
			return true
		}
	}
	return false
}
