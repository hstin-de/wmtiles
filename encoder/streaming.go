package encoder

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tileid"
	"github.com/zeebo/blake3"
)

const initialColdStartReserve = format.ColdStartBudget

type StreamingEncoder struct {
	opts       Options
	outPath    string
	pixelSize  int
	pixPerTile int
	nxyz       uint64

	variables    []format.VariableEntry
	idByName     map[string]uint16
	specByName   map[string]VariableSpec
	defaultDType map[string]uint8

	// blockMu guards blocks and declarations
	blockMu      sync.RWMutex
	blocks       map[blockKey]*blockBuilder
	declarations []blockKey

	jobCh chan submitMsg
	resCh chan encodedTile

	workerWg       sync.WaitGroup
	serializerDone chan struct{}

	out *os.File

	cursor uint64

	sharedSampler *codec.SharedSampler

	blockTable []format.BlockTableEntry

	globalMin map[uint16]float64
	globalMax map[uint16]float64

	// errMu guards firstErr; we only ever surface the first error from any worker
	errMu     sync.Mutex
	firstErr  error
	finishing sync.Once
	closed    bool
}

type blockKey struct {
	variableID uint16
	timeID     uint32
}

type submitMsg struct {
	block  *blockBuilder
	tid    uint64
	pixels []float32
}

func NewStreamingEncoder(opts Options, outPath string) (*StreamingEncoder, error) {
	defaults(&opts)
	pixelSize := 1 << opts.TilePixelSizeLog2
	pixPerTile := pixelSize * pixelSize

	if opts.TimeCatalog.Count == 0 && len(opts.TimeCatalog.TimestampsMs) == 0 && opts.TimeCatalog.IntervalMs == 0 {
		return nil, errors.New("time catalog count is zero")
	}
	if opts.MaxZoom < opts.MinZoom {
		return nil, fmt.Errorf("max-zoom %d < min-zoom %d", opts.MaxZoom, opts.MinZoom)
	}

	variables := make([]format.VariableEntry, 0, len(opts.Variables))
	idByName := make(map[string]uint16, len(opts.Variables))
	specByName := make(map[string]VariableSpec, len(opts.Variables))
	for i, v := range opts.Variables {
		id := uint16(i)
		variables = append(variables, format.VariableEntry{
			VariableID:             id,
			Name:                   v.Name,
			Unit:                   v.Unit,
			DefaultDType:           uint8(quantize.DTypeU16),
			DefaultCodec:           defaultCodec,
			DefaultPrecisionHint:   v.Precision,
			ColormapHint:           v.ColormapHint,
			ValueMinObservedGlobal: math.NaN(),
			ValueMaxObservedGlobal: math.NaN(),
		})
		idByName[v.Name] = id
		specByName[v.Name] = v
	}

	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	// pre-reserve the cold-start window so a small snapshot can land inside it later
	// (one-RTT cold start); blocks are written from cursor=ColdStartBudget onward
	if err := out.Truncate(int64(initialColdStartReserve)); err != nil {
		out.Close()
		os.Remove(outPath)
		return nil, fmt.Errorf("reserve cold-start window: %w", err)
	}

	se := &StreamingEncoder{
		opts:           opts,
		outPath:        outPath,
		pixelSize:      pixelSize,
		pixPerTile:     pixPerTile,
		nxyz:           tileid.NumXYZ(opts.MaxZoom),
		variables:      variables,
		idByName:       idByName,
		specByName:     specByName,
		defaultDType:   make(map[string]uint8),
		blocks:         make(map[blockKey]*blockBuilder),
		out:            out,
		cursor:         uint64(initialColdStartReserve),
		globalMin:      make(map[uint16]float64),
		globalMax:      make(map[uint16]float64),
		serializerDone: make(chan struct{}),
	}

	if opts.SkipInternalWorkers {
		se.sharedSampler = codec.NewSharedSampler()
		close(se.serializerDone)
	} else {
		numWorkers := max(runtime.GOMAXPROCS(0), 1)
		// *4 keeps workers fed across the producer's bursts without unbounded memory growth
		se.jobCh = make(chan submitMsg, numWorkers*4)
		se.resCh = make(chan encodedTile, numWorkers*4)

		// pipeline: Submit → jobCh → workers (quantize+codec) → resCh → serializer (per-block dedup+append)
		// shutdown is driven from Finish: close(jobCh) drains workers, then close(resCh) drains serializer
		se.workerWg.Add(numWorkers)
		for range numWorkers {
			go se.worker()
		}
		go se.serializer()
	}

	return se, nil
}

// RegisterVariable adds a variable lazily and returns its ID.
func (s *StreamingEncoder) RegisterVariable(spec VariableSpec) uint16 {
	s.blockMu.Lock()
	defer s.blockMu.Unlock()
	if id, ok := s.idByName[spec.Name]; ok {
		s.specByName[spec.Name] = spec
		return id
	}
	id := uint16(len(s.variables))
	s.variables = append(s.variables, format.VariableEntry{
		VariableID:             id,
		Name:                   spec.Name,
		Unit:                   spec.Unit,
		DefaultDType:           uint8(quantize.DTypeU16),
		DefaultCodec:           defaultCodec,
		DefaultPrecisionHint:   spec.Precision,
		ColormapHint:           spec.ColormapHint,
		ValueMinObservedGlobal: math.NaN(),
		ValueMaxObservedGlobal: math.NaN(),
	})
	s.idByName[spec.Name] = id
	s.specByName[spec.Name] = spec
	return id
}

func (s *StreamingEncoder) DeclareBlock(spec BlockSpec) error {
	s.blockMu.RLock()
	id, ok := s.idByName[spec.Variable]
	s.blockMu.RUnlock()
	if !ok {
		return fmt.Errorf("DeclareBlock: unknown variable %q", spec.Variable)
	}
	if !(spec.ValueMin <= spec.ValueMax) {
		return fmt.Errorf("DeclareBlock %q t=%d: invalid range [%g, %g]",
			spec.Variable, spec.TimeStep, spec.ValueMin, spec.ValueMax)
	}
	precision := spec.Precision
	if precision == 0 {
		precision = s.specByName[spec.Variable].Precision
	}
	params := fitParamsFor(spec.ValueMin, spec.ValueMax, precision)

	s.blockMu.Lock()
	defer s.blockMu.Unlock()
	k := blockKey{variableID: id, timeID: spec.TimeStep}
	if _, dup := s.blocks[k]; dup {
		return fmt.Errorf("DeclareBlock %q t=%d: already declared", spec.Variable, spec.TimeStep)
	}
	bb := newBlockBuilder(id, spec.Variable, spec.TimeStep, params, defaultCodec)
	bb.vmin = spec.ValueMin
	bb.vmax = spec.ValueMax
	s.blocks[k] = bb
	s.declarations = append(s.declarations, k)

	if cur, ok := s.globalMin[id]; !ok || spec.ValueMin < cur {
		s.globalMin[id] = spec.ValueMin
	}
	if cur, ok := s.globalMax[id]; !ok || spec.ValueMax > cur {
		s.globalMax[id] = spec.ValueMax
	}
	if _, ok := s.defaultDType[spec.Variable]; !ok {
		s.defaultDType[spec.Variable] = uint8(params.DType)
	}
	return nil
}

func (s *StreamingEncoder) worker() {
	defer s.workerWg.Done()
	tcEnc, err := codec.NewEncoderWithOpts(s.opts.ZstdLevel, !s.opts.DisableDeltaCodec)
	if err != nil {
		s.setErr(err)
		for range s.jobCh {
		}
		return
	}
	defer tcEnc.Close()

	hasher := blake3.New()
	var scratch []byte
	for msg := range s.jobCh {
		bb := msg.block
		stride := bb.params.DType.Bytes()
		quantBytes := s.pixPerTile * stride
		if cap(scratch) < quantBytes {
			scratch = make([]byte, quantBytes)
		}
		quant := scratch[:quantBytes]
		quantize.Encode(msg.pixels, bb.params, quant)
		if s.opts.OnPixelsConsumed != nil {
			s.opts.OnPixelsConsumed(msg.pixels)
		}

		hasher.Reset()
		hasher.Write(quant)
		var key [32]byte
		hasher.Sum(key[:0])

		blob := tcEnc.EncodeBestSampled(quant, bb.params, s.pixPerTile, bb.variable)
		s.resCh <- encodedTile{block: bb, tid: msg.tid, key: key, blob: blob}
	}
}

func (s *StreamingEncoder) serializer() {
	defer close(s.serializerDone)
	for et := range s.resCh {
		et.block.addEncoded(et.tid, et.key, et.blob)
	}
}

func (s *StreamingEncoder) Submit(t Tile) error {
	if err := s.checkErr(); err != nil {
		return err
	}
	if len(t.Pixels) != s.pixPerTile {
		return fmt.Errorf("tile %s/%d/(%d,%d,%d): pixel count %d, want %d",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, len(t.Pixels), s.pixPerTile)
	}
	id, ok := s.idByName[t.Variable]
	if !ok {
		return fmt.Errorf("Submit: unknown variable %q", t.Variable)
	}
	if t.Z < s.opts.MinZoom || t.Z > s.opts.MaxZoom {
		return fmt.Errorf("Submit %s/%d/(%d,%d,%d): zoom out of range [%d, %d]",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, s.opts.MinZoom, s.opts.MaxZoom)
	}
	if n := uint32(1) << t.Z; t.X >= n || t.Y >= n {
		return fmt.Errorf("Submit %s/%d/(%d,%d,%d): x/y out of range [0, %d) at z=%d",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, n, t.Z)
	}
	s.blockMu.RLock()
	bb, ok := s.blocks[blockKey{variableID: id, timeID: t.TimeStep}]
	s.blockMu.RUnlock()
	if !ok {
		return fmt.Errorf("Submit %s/%d: block was not declared", t.Variable, t.TimeStep)
	}
	tid := tileid.Encode3D(t.Z, t.X, t.Y)
	s.jobCh <- submitMsg{block: bb, tid: tid, pixels: t.Pixels}
	return nil
}

func (s *StreamingEncoder) Finish() error {
	var err error
	s.finishing.Do(func() {
		if !s.opts.SkipInternalWorkers {
			close(s.jobCh)
			s.workerWg.Wait()
			close(s.resCh)
			<-s.serializerDone
		}
		s.closed = true

		if e := s.checkErr(); e != nil {
			err = e
			s.cleanupOnErr()
			return
		}

		for _, k := range s.declarations {
			bb := s.blocks[k]
			if e := bb.finishBlock(s.opts.InternalCompression); e != nil {
				err = e
				s.cleanupOnErr()
				return
			}
		}

		if _, e := s.out.Seek(int64(s.cursor), 0); e != nil {
			err = fmt.Errorf("seek to block region: %w", e)
			s.cleanupOnErr()
			return
		}
		for _, k := range s.declarations {
			bb := s.blocks[k]
			off := s.cursor
			n, e := bb.writeBlockTo(s.out)
			if e != nil {
				err = fmt.Errorf("write block (var=%d t=%d): %w", k.variableID, k.timeID, e)
				s.cleanupOnErr()
				return
			}
			s.cursor += uint64(n)
			s.blockTable = append(s.blockTable, bb.blockTableEntry(off))
			bb.release()
		}

		for i := range s.variables {
			v := &s.variables[i]
			if mn, ok := s.globalMin[v.VariableID]; ok {
				v.ValueMinObservedGlobal = mn
			}
			if mx, ok := s.globalMax[v.VariableID]; ok {
				v.ValueMaxObservedGlobal = mx
			}
			if dt, ok := s.defaultDType[v.Name]; ok {
				v.DefaultDType = dt
			}
		}

		now := s.opts.CreationTime
		if now.IsZero() {
			now = time.Now()
		}
		plan := &snapshotPlan{
			creationTimeMs:  now.UnixMilli(),
			referenceTimeMs: msFromTime(s.opts.ReferenceForecastTime),
			generation:      0,
			variables:       s.variables,
			timeCatalog:     s.opts.TimeCatalog,
			blockTable:      s.blockTable,
			metadata:        buildMetadata(s.opts.Metadata, s.opts, 0, len(s.blockTable), now),
		}
		snap, regularTime, e := writeSnapshot(plan, s.opts.InternalCompression)
		if e != nil {
			err = e
			s.cleanupOnErr()
			return
		}

		// snapshot lands in the reserved window if it fits: that's what makes cold-start
		// a single Range request. Otherwise it gets appended past the blocks
		var snapOff uint64
		var coldStartFlag bool
		reservedSnapBytes := uint64(initialColdStartReserve - format.HeaderSize)
		if uint64(len(snap)) <= reservedSnapBytes {
			snapOff = uint64(format.HeaderSize)
			if _, e := s.out.WriteAt(snap, int64(snapOff)); e != nil {
				err = fmt.Errorf("write snapshot in reserve: %w", e)
				s.cleanupOnErr()
				return
			}
			coldStartFlag = true
		} else {
			snapOff = s.cursor
			if _, e := s.out.WriteAt(snap, int64(snapOff)); e != nil {
				err = fmt.Errorf("append snapshot: %w", e)
				s.cleanupOnErr()
				return
			}
			s.cursor += uint64(len(snap))
			coldStartFlag = false
		}

		trailerOff := s.cursor
		if !coldStartFlag {
			trailerOff = s.cursor
		} else {
			trailerOff = s.cursor
		}
		ftBytes := format.MarshalFileTrailer(&format.FileTrailer{FileLogicalEnd: trailerOff + format.FileTrailerSize})
		if _, e := s.out.WriteAt(ftBytes, int64(trailerOff)); e != nil {
			err = fmt.Errorf("write file trailer: %w", e)
			s.cleanupOnErr()
			return
		}
		fileEnd := trailerOff + uint64(format.FileTrailerSize)

		flags := uint16(0)
		if coldStartFlag {
			flags |= format.FlagColdStartInWindow
		}
		if regularTime {
			flags |= format.FlagTimeCatalogRegular
		}
		h := &format.Header{
			FormatVersion:        format.FormatVersion,
			Flags:                flags,
			ActiveSnapshotOffset: snapOff,
			ActiveSnapshotLength: uint64(len(snap)),
			FileLogicalEnd:       fileEnd,
			SnapshotGeneration:   0,
			InternalCompression:  s.opts.InternalCompression,
			TilePixelSizeLog2:    s.opts.TilePixelSizeLog2,
			MinZoom:              s.opts.MinZoom,
			MaxZoom:              s.opts.MaxZoom,
			BBoxLonMinE7:         int32(roundE7(s.opts.BBox[0])),
			BBoxLatMinE7:         int32(roundE7(s.opts.BBox[1])),
			BBoxLonMaxE7:         int32(roundE7(s.opts.BBox[2])),
			BBoxLatMaxE7:         int32(roundE7(s.opts.BBox[3])),
		}
		// header goes last: its CRC+magic-tail are the atomic publish; a crash before
		// this leaves the file with no/stale header but no torn-write hazard
		if _, e := s.out.WriteAt(format.MarshalHeader(h), 0); e != nil {
			err = fmt.Errorf("write header: %w", e)
			s.cleanupOnErr()
			return
		}
		if e := s.out.Sync(); e != nil {
			err = fmt.Errorf("fsync: %w", e)
			s.cleanupOnErr()
			return
		}
		if e := s.out.Truncate(int64(fileEnd)); e != nil {
			err = fmt.Errorf("truncate: %w", e)
			s.cleanupOnErr()
			return
		}
		if e := s.out.Close(); e != nil {
			err = e
			return
		}
	})
	return err
}

func (s *StreamingEncoder) Close() error {
	s.finishing.Do(func() {
		s.setErr(errors.New("streaming encoder closed without Finish"))
		if !s.opts.SkipInternalWorkers {
			close(s.jobCh)
			s.workerWg.Wait()
			close(s.resCh)
			<-s.serializerDone
		}
		s.closed = true
		s.cleanupOnErr()
	})
	return nil
}

func (s *StreamingEncoder) cleanupOnErr() {
	if s.out != nil {
		s.out.Close()
		os.Remove(s.outPath)
		s.out = nil
	}
}

func (s *StreamingEncoder) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.firstErr == nil {
		s.firstErr = err
	}
}

func (s *StreamingEncoder) checkErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.firstErr
}

func (s *StreamingEncoder) VariableID(name string) (uint16, bool) {
	id, ok := s.idByName[name]
	return id, ok
}

func (s *StreamingEncoder) DeclaredBlocks() []blockKey {
	s.blockMu.RLock()
	defer s.blockMu.RUnlock()
	out := make([]blockKey, len(s.declarations))
	copy(out, s.declarations)
	sort.Slice(out, func(i, j int) bool {
		if out[i].variableID != out[j].variableID {
			return out[i].variableID < out[j].variableID
		}
		return out[i].timeID < out[j].timeID
	})
	return out
}

func msFromTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func roundE7(v float64) int32 {
	r := math.Round(v * 1e7)
	switch {
	case r >= math.MaxInt32:
		return math.MaxInt32
	case r <= math.MinInt32:
		return math.MinInt32
	}
	return int32(r)
}
