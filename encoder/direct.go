package encoder

import (
	"fmt"

	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/quantize"
	"github.com/hstin-de/wmtiles/tileid"
	"github.com/zeebo/blake3"
)

// DirectWorker runs quantize, hash and codec inline so a caller-owned
// goroutine can drive a StreamingEncoder or AppendCtx without going through
// the channel-based worker pool. Not safe to share across goroutines.
type DirectWorker struct {
	enc     *StreamingEncoder
	app     *AppendCtx
	tcEnc   *codec.Encoder
	hasher  *blake3.Hasher
	scratch []byte
	pixPer  int
	zstdLvl int
	delta   bool
}

func (s *StreamingEncoder) NewDirectWorker() (*DirectWorker, error) {
	tcEnc, err := codec.NewEncoderWithOpts(s.opts.ZstdLevel, !s.opts.DisableDeltaCodec)
	if err != nil {
		return nil, err
	}
	return &DirectWorker{
		enc:    s,
		tcEnc:  tcEnc,
		hasher: blake3.New(),
		pixPer: s.pixPerTile,
	}, nil
}

func (a *AppendCtx) NewDirectAppendWorker() (*DirectWorker, error) {
	tcEnc, err := codec.NewEncoderWithOpts(a.zstdLevel, a.allowDelta)
	if err != nil {
		return nil, err
	}
	return &DirectWorker{
		app:    a,
		tcEnc:  tcEnc,
		hasher: blake3.New(),
		pixPer: a.pixPerTile,
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

	w.hasher.Reset()
	w.hasher.Write(quant)
	var key [32]byte
	w.hasher.Sum(key[:0])

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
