package encoder

import (
	"fmt"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tileid"
	"github.com/hstin-de/wmtiles/tiler"
)

// DirectWorker runs quantize, hash and codec inline so a caller-owned
// goroutine can drive a StreamingEncoder or AppendCtx without going through
// the channel-based worker pool. Not safe to share across goroutines.
type DirectWorker struct {
	enc      *StreamingEncoder
	app      *AppendCtx
	tcEnc    *codec.Encoder
	scratch  []byte
	pixPer   int
	zstdLvl  int
	delta    bool
	dictMode bool
}

func (s *StreamingEncoder) NewDirectWorker() (*DirectWorker, error) {
	tcEnc, err := codec.NewEncoderWithOpts(s.opts.ZstdLevel, !s.opts.DisableDeltaCodec)
	if err != nil {
		return nil, err
	}
	return &DirectWorker{
		enc:      s,
		tcEnc:    tcEnc,
		pixPer:   s.pixPerTile,
		dictMode: s.opts.EnableTileDict,
	}, nil
}

func (a *AppendCtx) NewDirectAppendWorker() (*DirectWorker, error) {
	tcEnc, err := codec.NewEncoderWithOpts(a.zstdLevel, a.allowDelta)
	if err != nil {
		return nil, err
	}
	return &DirectWorker{
		app:      a,
		tcEnc:    tcEnc,
		pixPer:   a.pixPerTile,
		dictMode: a.enableTileDict,
	}, nil
}

func (w *DirectWorker) Close() error {
	if w.tcEnc != nil {
		w.tcEnc.Close()
	}
	return nil
}

// SubmitDirect quantises, hashes and encodes the tile on the caller goroutine
// and appends the result into the block's dedup buffer.
func (w *DirectWorker) SubmitDirect(t Tile) error {
	if w.enc != nil {
		return w.submitToStreaming(t)
	}
	return w.submitToAppend(t)
}

func (w *DirectWorker) submitToStreaming(t Tile) error {
	s := w.enc
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
	w.encodeAndStore(bb, tid, t.Pixels, s.opts.OnPixelsConsumed)
	return nil
}

func (w *DirectWorker) submitToAppend(t Tile) error {
	a := w.app
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
	bb, ok := a.blocks[blockKey{variableID: id, timeID: t.TimeStep}]
	a.blockMu.RUnlock()
	if !ok {
		return fmt.Errorf("Submit %s/%d: block was not declared", t.Variable, t.TimeStep)
	}
	tid := tileid.Encode3D(t.Z, t.X, t.Y)
	w.encodeAndStore(bb, tid, t.Pixels, nil)
	return nil
}

// SubmitTileFused runs the full per-tile pipeline without the float32
// intermediate buffer: tile+quantize writes u8/u16 directly into scratch.
// Returns (false, nil) when the tile has no data; returns errFusedNotSupported
// for f32 blocks or non-uniform grids — caller must fall back.
func (w *DirectWorker) SubmitTileFused(varName string, tIdx uint32, z uint8, x, y uint32, pixSize int, s *tiler.Sampler) (bool, error) {
	if w.enc == nil {
		return false, fmt.Errorf("SubmitTileFused requires a streaming encoder worker")
	}
	enc := w.enc
	if err := enc.checkErr(); err != nil {
		return false, err
	}
	id, ok := enc.idByName[varName]
	if !ok {
		return false, fmt.Errorf("SubmitTileFused: unknown variable %q", varName)
	}
	if z < enc.opts.MinZoom || z > enc.opts.MaxZoom {
		return false, fmt.Errorf("SubmitTileFused %s/%d/(%d,%d,%d): zoom out of range [%d, %d]",
			varName, tIdx, z, x, y, enc.opts.MinZoom, enc.opts.MaxZoom)
	}
	if n := uint32(1) << z; x >= n || y >= n {
		return false, fmt.Errorf("SubmitTileFused %s/%d/(%d,%d,%d): x/y out of range [0, %d) at z=%d",
			varName, tIdx, z, x, y, n, z)
	}
	enc.blockMu.RLock()
	bb, ok := enc.blocks[blockKey{variableID: id, timeID: tIdx}]
	enc.blockMu.RUnlock()
	if !ok {
		return false, fmt.Errorf("SubmitTileFused %s/%d: block was not declared", varName, tIdx)
	}
	if !s.Uniform() {
		return false, errFusedNotSupported
	}
	stride := bb.params.DType.Bytes()
	if stride != 1 && stride != 2 {
		return false, errFusedNotSupported
	}
	quantBytes := w.pixPer * stride
	if cap(w.scratch) < quantBytes {
		w.scratch = make([]byte, quantBytes)
	}
	quant := w.scratch[:quantBytes]

	var produced bool
	switch stride {
	case 2:
		produced = tiler.TileQuantU16(s, z, x, y, pixSize, bb.params.Scale, bb.params.Offset, quant)
	case 1:
		produced = tiler.TileQuantU8(s, z, x, y, pixSize, bb.params.Scale, bb.params.Offset, quant)
	}
	if !produced {
		return false, nil
	}

	tid := tileid.Encode3D(z, x, y)
	w.runEncodeFromQuant(bb, tid, quant)
	return true, nil
}

// errFusedNotSupported signals the caller should use the float32 fallback.
var errFusedNotSupported = fmt.Errorf("fused tile path not supported for this block")

func IsFusedNotSupported(err error) bool { return err == errFusedNotSupported }

// runEncodeFromQuant runs hash + codec + store on already-quantized bytes.
// Probes dedup first so duplicates skip the codec pass entirely.
func (w *DirectWorker) runEncodeFromQuant(bb *blockBuilder, tid uint64, quant []byte) {
	var key [32]byte
	hashQuantInto(quant, &key)

	if !w.dictMode && bb.dedupHit(tid, key) {
		return
	}

	if w.dictMode {
		tag, inner := w.tcEnc.EncodeInnerOnly(quant, bb.params, w.pixPer)
		bb.addEncodedInner(tid, key, tag, inner)
		return
	}

	var blob []byte
	switch {
	case w.enc != nil && w.enc.sharedSampler != nil:
		blob = w.tcEnc.EncodeBestShared(quant, bb.params, w.pixPer, bb.variable, w.enc.sharedSampler)
	case w.app != nil && w.app.sharedSampler != nil:
		blob = w.tcEnc.EncodeBestShared(quant, bb.params, w.pixPer, bb.variable, w.app.sharedSampler)
	default:
		blob = w.tcEnc.EncodeBestSampled(quant, bb.params, w.pixPer, bb.variable)
	}
	bb.addEncoded(tid, key, blob)
}

func (w *DirectWorker) encodeAndStore(bb *blockBuilder, tid uint64, pixels []float32, onConsumed func([]float32)) {
	stride := bb.params.DType.Bytes()
	quantBytes := w.pixPer * stride
	if cap(w.scratch) < quantBytes {
		w.scratch = make([]byte, quantBytes)
	}
	quant := w.scratch[:quantBytes]
	quantize.Encode(pixels, bb.params, quant)
	if onConsumed != nil {
		onConsumed(pixels)
	}

	var key [32]byte
	hashQuantInto(quant, &key)

	if !w.dictMode && bb.dedupHit(tid, key) {
		return
	}

	if w.dictMode {
		tag, inner := w.tcEnc.EncodeInnerOnly(quant, bb.params, w.pixPer)
		bb.addEncodedInner(tid, key, tag, inner)
		return
	}

	var blob []byte
	switch {
	case w.enc != nil && w.enc.sharedSampler != nil:
		blob = w.tcEnc.EncodeBestShared(quant, bb.params, w.pixPer, bb.variable, w.enc.sharedSampler)
	case w.app != nil && w.app.sharedSampler != nil:
		blob = w.tcEnc.EncodeBestShared(quant, bb.params, w.pixPer, bb.variable, w.app.sharedSampler)
	default:
		blob = w.tcEnc.EncodeBestSampled(quant, bb.params, w.pixPer, bb.variable)
	}
	bb.addEncoded(tid, key, blob)
}
