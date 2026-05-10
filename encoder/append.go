package encoder

import (
	"encoding/json"
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
)

type AppendOptions struct {
	ZstdLevel int

	AllowReplace bool

	CreationTime time.Time

	DisableDeltaCodec bool

	// EnableTileDict — see encoder.Options.EnableTileDict.
	EnableTileDict bool

	// SkipInternalWorkers disables the channel-based quantize+codec pool;
	// callers must drive the context via NewDirectAppendWorker.
	SkipInternalWorkers bool
}

type AppendCtx struct {
	path string
	out  *os.File

	header   *format.Header
	snapshot *loadedSnapshot

	pixelSize  int
	pixPerTile int

	zstdLevel int

	allowDelta          bool
	enableTileDict      bool
	skipInternalWorkers bool
	sharedSampler       *codec.SharedSampler

	variables  []format.VariableEntry
	idByName   map[string]uint16
	specByName map[string]VariableSpec

	// blockMu guards blocks, declarations, variables, idByName, specByName and timeCatalog
	blockMu        sync.RWMutex
	blocks         map[blockKey]*blockBuilder
	declarations   []blockKey
	allowReplace   bool
	existingBlocks map[blockKey]struct{}

	creationTimeOverride time.Time

	timeCatalog format.TimeCatalog

	cursor uint64

	jobCh          chan submitMsg
	resCh          chan encodedTile
	workerWg       sync.WaitGroup
	serializerDone chan struct{}

	errMu     sync.Mutex
	firstErr  error
	finishing sync.Once
}

type loadedSnapshot struct {
	header     *format.SnapshotHeader
	variables  []format.VariableEntry
	timeCat    format.TimeCatalog
	blockTable []format.BlockTableEntry
	metadata   map[string]any
}

func OpenForAppend(path string, opts AppendOptions) (*AppendCtx, error) {
	if opts.ZstdLevel == 0 {
		opts.ZstdLevel = 3
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	headerBuf := make([]byte, format.HeaderSize)
	if _, err := f.ReadAt(headerBuf, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("read header: %w", err)
	}
	h, err := format.UnmarshalHeader(headerBuf)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("parse header: %w", err)
	}
	snap, err := loadActiveSnapshot(f, h)
	if err != nil {
		f.Close()
		return nil, err
	}

	pixSize := 1 << h.TilePixelSizeLog2
	pixPerTile := pixSize * pixSize

	idByName := make(map[string]uint16, len(snap.variables))
	specByName := make(map[string]VariableSpec, len(snap.variables))
	for _, v := range snap.variables {
		idByName[v.Name] = v.VariableID
		specByName[v.Name] = VariableSpec{
			Name: v.Name, Unit: v.Unit, ColormapHint: v.ColormapHint,
			Precision: v.DefaultPrecisionHint,
		}
	}

	clonedVars := make([]format.VariableEntry, len(snap.variables))
	copy(clonedVars, snap.variables)

	existing := make(map[blockKey]struct{}, len(snap.blockTable))
	for _, e := range snap.blockTable {
		existing[blockKey{e.VariableID, e.TimeID}] = struct{}{}
	}

	ctx := &AppendCtx{
		path:                 path,
		out:                  f,
		header:               h,
		snapshot:             snap,
		pixelSize:            pixSize,
		pixPerTile:           pixPerTile,
		zstdLevel:            opts.ZstdLevel,
		allowDelta:           !opts.DisableDeltaCodec,
		enableTileDict:       opts.EnableTileDict,
		skipInternalWorkers:  opts.SkipInternalWorkers,
		variables:            clonedVars,
		idByName:             idByName,
		specByName:           specByName,
		blocks:               make(map[blockKey]*blockBuilder),
		allowReplace:         opts.AllowReplace,
		creationTimeOverride: opts.CreationTime,
		timeCatalog:          cloneTimeCatalog(snap.timeCat),
		cursor:               h.FileLogicalEnd - uint64(format.FileTrailerSize), // overwrite existing trailer; new blocks + trailer are appended from here
		serializerDone:       make(chan struct{}),
	}

	ctx.existingBlocks = existing

	if !opts.SkipInternalWorkers {
		numWorkers := max(runtime.GOMAXPROCS(0), 1)
		ctx.jobCh = make(chan submitMsg, numWorkers*4)
		ctx.resCh = make(chan encodedTile, numWorkers*4)

		ctx.sharedSampler = codec.NewSharedSampler()
		ctx.workerWg.Add(numWorkers)
		for range numWorkers {
			go ctx.worker()
		}
		go ctx.serializer()
	} else {
		ctx.sharedSampler = codec.NewSharedSampler()
		close(ctx.serializerDone)
	}

	return ctx, nil
}

func loadActiveSnapshot(f *os.File, h *format.Header) (*loadedSnapshot, error) {
	buf := make([]byte, h.ActiveSnapshotLength)
	if _, err := f.ReadAt(buf, int64(h.ActiveSnapshotOffset)); err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	return parseSnapshot(buf, h)
}

func parseSnapshot(buf []byte, h *format.Header) (*loadedSnapshot, error) {
	if uint64(len(buf)) < uint64(format.SnapshotHeaderSize+format.SnapshotTrailerSize) {
		return nil, fmt.Errorf("snapshot: too short (%d B)", len(buf))
	}
	sh, err := format.UnmarshalSnapshotHeader(buf[:format.SnapshotHeaderSize])
	if err != nil {
		return nil, fmt.Errorf("parse snapshot header: %w", err)
	}
	trailerOff := uint64(len(buf)) - uint64(format.SnapshotTrailerSize)
	tr, err := format.UnmarshalSnapshotTrailer(buf[trailerOff:])
	if err != nil {
		return nil, fmt.Errorf("parse snapshot trailer: %w", err)
	}
	if tr.SnapshotTotalLength != uint64(len(buf)) {
		return nil, fmt.Errorf("snapshot trailer length %d != buf %d", tr.SnapshotTotalLength, len(buf))
	}
	if want := format.CRC32C(buf[:trailerOff]); want != tr.CRC32C {
		return nil, fmt.Errorf("%w: stored=0x%08X computed=0x%08X", format.ErrBadSnapshotCRC, tr.CRC32C, want)
	}

	varRaw, err := format.Decompress(buf[sh.VariableCatalogOff:sh.VariableCatalogOff+sh.VariableCatalogLen], h.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress catalog: %w", err)
	}
	vars, err := format.UnmarshalVariableCatalog(varRaw, int(sh.NumVariables))
	if err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}

	regular := h.Flags&format.FlagTimeCatalogRegular != 0
	tcRaw := buf[sh.TimeCatalogOff : sh.TimeCatalogOff+sh.TimeCatalogLen]
	if !regular {
		tcRaw, err = format.Decompress(tcRaw, h.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("decompress time catalog: %w", err)
		}
	}
	tc, err := format.UnmarshalTimeCatalog(tcRaw, regular)
	if err != nil {
		return nil, fmt.Errorf("parse time catalog: %w", err)
	}

	rootRaw, err := format.Decompress(buf[sh.BlockTableRootOff:sh.BlockTableRootOff+sh.BlockTableRootLen], h.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decompress block-table root: %w", err)
	}
	rootEntries, err := format.UnmarshalBlockTable(rootRaw)
	if err != nil {
		return nil, fmt.Errorf("parse block-table root: %w", err)
	}
	flat := make([]format.BlockTableEntry, 0, len(rootEntries))
	for _, e := range rootEntries {
		if !e.IsLeafPointer {
			flat = append(flat, e)
			continue
		}
		leafRaw := buf[sh.BlockTableLeavesOff+e.BlockOffset : sh.BlockTableLeavesOff+e.BlockOffset+e.BlockLength]
		leafRaw, err = format.Decompress(leafRaw, h.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("decompress block-table leaf: %w", err)
		}
		leafEntries, err := format.UnmarshalBlockTable(leafRaw)
		if err != nil {
			return nil, fmt.Errorf("parse block-table leaf: %w", err)
		}
		flat = append(flat, leafEntries...)
	}

	metadata := map[string]any{}
	if sh.MetadataLen > 0 {
		mdRaw, err := format.Decompress(buf[sh.MetadataOff:sh.MetadataOff+sh.MetadataLen], h.InternalCompression)
		if err != nil {
			return nil, fmt.Errorf("decompress metadata: %w", err)
		}
		_ = unmarshalMetadata(mdRaw, &metadata)
	}

	return &loadedSnapshot{
		header:     sh,
		variables:  vars,
		timeCat:    *tc,
		blockTable: flat,
		metadata:   metadata,
	}, nil
}

func (a *AppendCtx) RegisterVariable(spec VariableSpec) (uint16, error) {
	a.blockMu.Lock()
	defer a.blockMu.Unlock()
	if id, ok := a.idByName[spec.Name]; ok {
		return id, nil
	}
	id := uint16(len(a.variables))
	a.variables = append(a.variables, format.VariableEntry{
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
	a.idByName[spec.Name] = id
	a.specByName[spec.Name] = spec
	return id, nil
}

func (a *AppendCtx) RegisterTimeStep(unixMs int64) (uint32, error) {
	a.blockMu.Lock()
	defer a.blockMu.Unlock()

	if a.timeCatalog.Regular {
		if a.timeCatalog.IntervalMs == 0 {
			if a.timeCatalog.Count == 0 {
				a.timeCatalog.StartMs = unixMs
				a.timeCatalog.Count = 1
				return 0, nil
			}
			if unixMs == a.timeCatalog.StartMs {
				return 0, nil
			}
		}
		if a.timeCatalog.IntervalMs > 0 {
			delta := unixMs - a.timeCatalog.StartMs
			if delta%a.timeCatalog.IntervalMs == 0 {
				idx := delta / a.timeCatalog.IntervalMs
				if idx >= 0 && idx < a.timeCatalog.Count {
					return uint32(idx), nil
				}
				if idx == a.timeCatalog.Count {
					a.timeCatalog.Count++
					return uint32(idx), nil
				}
			}
		}
		a.timeCatalog = irregularFromRegular(a.timeCatalog)
	}
	for i, ts := range a.timeCatalog.TimestampsMs {
		if ts == unixMs {
			return uint32(i), nil
		}
	}
	if n := len(a.timeCatalog.TimestampsMs); n > 0 && a.timeCatalog.TimestampsMs[n-1] > unixMs {
		return 0, fmt.Errorf("time step %d would be out of chronological order", unixMs)
	}
	a.timeCatalog.TimestampsMs = append(a.timeCatalog.TimestampsMs, unixMs)
	a.timeCatalog.Count = int64(len(a.timeCatalog.TimestampsMs))
	return uint32(len(a.timeCatalog.TimestampsMs) - 1), nil
}

// TimeCount returns the number of time steps in the current append catalog.
func (a *AppendCtx) TimeCount() int {
	a.blockMu.RLock()
	defer a.blockMu.RUnlock()
	return int(a.timeCatalog.Count)
}

func (a *AppendCtx) DeclareBlock(spec BlockSpec) error {
	if !(spec.ValueMin <= spec.ValueMax) {
		return fmt.Errorf("DeclareBlock %q t=%d: invalid range [%g, %g]",
			spec.Variable, spec.TimeStep, spec.ValueMin, spec.ValueMax)
	}

	a.blockMu.Lock()
	defer a.blockMu.Unlock()
	id, ok := a.idByName[spec.Variable]
	if !ok {
		return fmt.Errorf("DeclareBlock: variable %q not registered (call RegisterVariable first)", spec.Variable)
	}
	if int64(spec.TimeStep) >= a.timeCatalog.Count {
		return fmt.Errorf("DeclareBlock %q t=%d: time step out of range [0, %d)",
			spec.Variable, spec.TimeStep, a.timeCatalog.Count)
	}
	precision := spec.Precision
	if precision == 0 {
		precision = a.specByName[spec.Variable].Precision
	}
	params := fitParamsFor(spec.ValueMin, spec.ValueMax, precision)
	k := blockKey{variableID: id, timeID: spec.TimeStep}
	if _, dup := a.blocks[k]; dup {
		return fmt.Errorf("DeclareBlock %q t=%d: already declared in this session", spec.Variable, spec.TimeStep)
	}
	if _, exists := a.existingBlocks[k]; exists && !a.allowReplace {
		return fmt.Errorf("DeclareBlock %q t=%d: block already exists in file (use AllowReplace)", spec.Variable, spec.TimeStep)
	}
	bb := newBlockBuilder(id, spec.Variable, spec.TimeStep, params, defaultCodec, a.pixPerTile)
	bb.vmin = spec.ValueMin
	bb.vmax = spec.ValueMax
	a.blocks[k] = bb
	a.declarations = append(a.declarations, k)

	v := &a.variables[id]
	if math.IsNaN(v.ValueMinObservedGlobal) || spec.ValueMin < v.ValueMinObservedGlobal {
		v.ValueMinObservedGlobal = spec.ValueMin
	}
	if math.IsNaN(v.ValueMaxObservedGlobal) || spec.ValueMax > v.ValueMaxObservedGlobal {
		v.ValueMaxObservedGlobal = spec.ValueMax
	}
	return nil
}

func (a *AppendCtx) Submit(t Tile) error {
	if err := a.checkErr(); err != nil {
		return err
	}
	if len(t.Pixels) != a.pixPerTile {
		return fmt.Errorf("Submit %s/%d/(%d,%d,%d): pixel count %d, want %d",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, len(t.Pixels), a.pixPerTile)
	}
	if t.Z < a.header.MinZoom || t.Z > a.header.MaxZoom {
		return fmt.Errorf("Submit %s/%d/(%d,%d,%d): zoom out of range [%d, %d]",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, a.header.MinZoom, a.header.MaxZoom)
	}
	if n := uint32(1) << t.Z; t.X >= n || t.Y >= n {
		return fmt.Errorf("Submit %s/%d/(%d,%d,%d): x/y out of range [0, %d) at z=%d",
			t.Variable, t.TimeStep, t.Z, t.X, t.Y, n, t.Z)
	}
	a.blockMu.RLock()
	id, ok := a.idByName[t.Variable]
	if !ok {
		a.blockMu.RUnlock()
		return fmt.Errorf("Submit: unknown variable %q", t.Variable)
	}
	if int64(t.TimeStep) >= a.timeCatalog.Count {
		a.blockMu.RUnlock()
		return fmt.Errorf("Submit %s/%d: time step out of range [0, %d)",
			t.Variable, t.TimeStep, a.timeCatalog.Count)
	}
	bb, ok := a.blocks[blockKey{variableID: id, timeID: t.TimeStep}]
	a.blockMu.RUnlock()
	if !ok {
		return fmt.Errorf("Submit %s/%d: block was not declared", t.Variable, t.TimeStep)
	}
	tid := tileid.Encode3D(t.Z, t.X, t.Y)
	a.jobCh <- submitMsg{block: bb, tid: tid, pixels: t.Pixels}
	return nil
}

func (a *AppendCtx) worker() {
	defer a.workerWg.Done()
	tcEnc, err := codec.NewEncoderWithOpts(a.zstdLevel, a.allowDelta)
	if err != nil {
		a.setErr(err)
		for range a.jobCh {
		}
		return
	}
	defer tcEnc.Close()

	var scratch []byte
	for msg := range a.jobCh {
		bb := msg.block
		stride := bb.params.DType.Bytes()
		quantBytes := a.pixPerTile * stride
		if cap(scratch) < quantBytes {
			scratch = make([]byte, quantBytes)
		}
		quant := scratch[:quantBytes]
		quantize.Encode(msg.pixels, bb.params, quant)

		var key [32]byte
		hashQuantInto(quant, &key)

		blob := tcEnc.EncodeBestShared(quant, bb.params, a.pixPerTile, bb.variable, a.sharedSampler)
		a.resCh <- encodedTile{block: bb, tid: msg.tid, key: key, blob: blob}
	}
}

func (a *AppendCtx) serializer() {
	defer close(a.serializerDone)
	for et := range a.resCh {
		et.block.addEncoded(et.tid, et.key, et.blob)
	}
}

// Finish ordering matters for crash safety: blocks → snapshot → trailer → fsync →
// header swap → fsync → truncate. Until the header swap is durable, readers see the
// previous active snapshot; the new bytes past file_logical_end are invisible
func (a *AppendCtx) Finish() error {
	var err error
	a.finishing.Do(func() {
		if !a.skipInternalWorkers {
			close(a.jobCh)
			a.workerWg.Wait()
			close(a.resCh)
			<-a.serializerDone
		}

		if e := a.checkErr(); e != nil {
			err = e
			a.cleanupOnErr()
			return
		}
		if len(a.declarations) == 0 {
			err = a.out.Close()
			return
		}

		comp := a.header.InternalCompression
		dictOpts := defaultDictOptions()
		dictOpts.enabled = a.enableTileDict
		if e := finishBlocksParallel(a.declarations, a.blocks, comp, dictOpts); e != nil {
			err = e
			a.cleanupOnErr()
			return
		}

		newEntries := make([]format.BlockTableEntry, 0, len(a.declarations))
		if _, e := a.out.Seek(int64(a.cursor), 0); e != nil {
			err = fmt.Errorf("seek: %w", e)
			a.cleanupOnErr()
			return
		}
		for _, k := range a.declarations {
			bb := a.blocks[k]
			off := a.cursor
			n, e := bb.writeBlockTo(a.out)
			if e != nil {
				err = fmt.Errorf("write block (var=%d t=%d): %w", k.variableID, k.timeID, e)
				a.cleanupOnErr()
				return
			}
			a.cursor += uint64(n)
			newEntries = append(newEntries, bb.blockTableEntry(off))
			bb.release()
		}

		merged := a.mergeBlockTable(newEntries)

		now := a.creationTimeOverride
		if now.IsZero() {
			now = time.Now()
		}
		plan := &snapshotPlan{
			creationTimeMs:  now.UnixMilli(),
			referenceTimeMs: a.snapshot.header.ReferenceTimeMs,
			generation:      a.header.SnapshotGeneration + 1,
			variables:       a.variables,
			timeCatalog:     a.timeCatalog,
			blockTable:      merged,
			metadata: buildMetadata(a.snapshot.metadata, Options{
				TilePixelSizeLog2: a.header.TilePixelSizeLog2,
				MinZoom:           a.header.MinZoom,
				MaxZoom:           a.header.MaxZoom,
			}, a.header.SnapshotGeneration+1, len(newEntries), now),
		}
		snap, regularTime, e := writeSnapshot(plan, comp)
		if e != nil {
			err = e
			a.cleanupOnErr()
			return
		}
		snapOff := a.cursor
		if _, e := a.out.WriteAt(snap, int64(snapOff)); e != nil {
			err = fmt.Errorf("write snapshot: %w", e)
			a.cleanupOnErr()
			return
		}
		a.cursor += uint64(len(snap))

		trailerOff := a.cursor
		ftBytes := format.MarshalFileTrailer(&format.FileTrailer{FileLogicalEnd: trailerOff + format.FileTrailerSize})
		if _, e := a.out.WriteAt(ftBytes, int64(trailerOff)); e != nil {
			err = fmt.Errorf("write file trailer: %w", e)
			a.cleanupOnErr()
			return
		}
		fileEnd := trailerOff + uint64(format.FileTrailerSize)

		// must be durable before the header swap publishes the new generation
		if e := a.out.Sync(); e != nil {
			err = fmt.Errorf("fsync before header swap: %w", e)
			a.cleanupOnErr()
			return
		}

		flags := uint16(0)
		if regularTime {
			flags |= format.FlagTimeCatalogRegular
		}
		if snapOff+uint64(len(snap)) <= format.ColdStartBudget {
			flags |= format.FlagColdStartInWindow
		}
		flags |= format.FlagHasPreviousSnapshot
		newHeader := &format.Header{
			FormatVersion:          format.FormatVersion,
			Flags:                  flags,
			ActiveSnapshotOffset:   snapOff,
			ActiveSnapshotLength:   uint64(len(snap)),
			PreviousSnapshotOffset: a.header.ActiveSnapshotOffset,
			PreviousSnapshotLength: a.header.ActiveSnapshotLength,
			FileLogicalEnd:         fileEnd,
			SnapshotGeneration:     a.header.SnapshotGeneration + 1,
			InternalCompression:    a.header.InternalCompression,
			TilePixelSizeLog2:      a.header.TilePixelSizeLog2,
			MinZoom:                a.header.MinZoom,
			MaxZoom:                a.header.MaxZoom,
			BBoxLonMinE7:           a.header.BBoxLonMinE7,
			BBoxLatMinE7:           a.header.BBoxLatMinE7,
			BBoxLonMaxE7:           a.header.BBoxLonMaxE7,
			BBoxLatMaxE7:           a.header.BBoxLatMaxE7,
		}
		if _, e := a.out.WriteAt(format.MarshalHeader(newHeader), 0); e != nil {
			err = fmt.Errorf("write new header: %w", e)
			a.cleanupOnErr()
			return
		}
		if e := a.out.Sync(); e != nil {
			err = fmt.Errorf("fsync after header swap: %w", e)
			a.cleanupOnErr()
			return
		}
		if e := a.out.Truncate(int64(fileEnd)); e != nil {
			err = fmt.Errorf("truncate: %w", e)
			a.cleanupOnErr()
			return
		}
		err = a.out.Close()
	})
	return err
}

func (a *AppendCtx) Close() error {
	a.finishing.Do(func() {
		a.setErr(errors.New("append context closed without Finish"))
		if !a.skipInternalWorkers {
			close(a.jobCh)
			a.workerWg.Wait()
			close(a.resCh)
			<-a.serializerDone
		}
		if a.out != nil {
			a.out.Close()
			a.out = nil
		}
	})
	return nil
}

func (a *AppendCtx) cleanupOnErr() {
	if a.out != nil {
		a.out.Close()
		a.out = nil
	}
}

func (a *AppendCtx) setErr(err error) {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	if a.firstErr == nil {
		a.firstErr = err
	}
}

func (a *AppendCtx) checkErr() error {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	return a.firstErr
}

func (a *AppendCtx) mergeBlockTable(newEntries []format.BlockTableEntry) []format.BlockTableEntry {
	byKey := make(map[uint64]format.BlockTableEntry, len(a.snapshot.blockTable)+len(newEntries))
	for _, e := range a.snapshot.blockTable {
		byKey[e.CompositeKey()] = e
	}
	for _, e := range newEntries {
		byKey[e.CompositeKey()] = e
	}
	out := make([]format.BlockTableEntry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CompositeKey() < out[j].CompositeKey() })
	return out
}

func (a *AppendCtx) VariableID(name string) (uint16, bool) {
	id, ok := a.idByName[name]
	return id, ok
}

func (a *AppendCtx) Snapshot() *loadedSnapshot { return a.snapshot }

func (a *AppendCtx) Header() *format.Header { return a.header }

func cloneTimeCatalog(t format.TimeCatalog) format.TimeCatalog {
	c := t
	if t.TimestampsMs != nil {
		c.TimestampsMs = make([]int64, len(t.TimestampsMs))
		copy(c.TimestampsMs, t.TimestampsMs)
	}
	return c
}

// expand a regular catalog to an explicit timestamp list once we hit a step that
// doesn't conform to the existing start+interval grid
func irregularFromRegular(t format.TimeCatalog) format.TimeCatalog {
	out := format.TimeCatalog{Regular: false, Count: t.Count}
	out.TimestampsMs = make([]int64, t.Count)
	for i := int64(0); i < t.Count; i++ {
		out.TimestampsMs[i] = t.StartMs + i*t.IntervalMs
	}
	return out
}

func unmarshalMetadata(raw []byte, into *map[string]any) error {
	return json.Unmarshal(raw, into)
}
