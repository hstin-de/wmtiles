package encoder

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/DataDog/zstd"
	"github.com/hstin-de/wmtiles/codec"
	"github.com/hstin-de/wmtiles/format"
	"github.com/hstin-de/wmtiles/quantize"
)

// finishBlockDict picks a single per-block transform, trains a zstd dict over
// a stride sample of transformed tiles, then dict-compresses every unique tile.
// The transform is chosen per-block (not per-tile) because the dict can only be
// tuned for one byte distribution. Falls back to plain zstd when the trainer
// rejects the input.
func (b *blockBuilder) finishBlockDict(opts dictOptions) error {
	if len(b.uniques) == 0 {
		return nil
	}

	// constants are stored verbatim and skip transform + dict selection
	nonConstIdx := make([]int, 0, len(b.uniques))
	for i := range b.uniques {
		if b.uniques[i].tag != codec.IDConstant && len(b.uniques[i].inner) > 0 {
			nonConstIdx = append(nonConstIdx, i)
		}
	}
	if len(nonConstIdx) == 0 {
		return b.finishBlockUniquesNoDict()
	}

	chosenTag := pickBlockTransform(b.uniques, nonConstIdx, b.params, b.nPixels)

	tlen := codec.TransformedLen(chosenTag, b.params, b.nPixels)
	transformed := make([][]byte, len(b.uniques))
	for _, i := range nonConstIdx {
		buf := make([]byte, tlen)
		if err := codec.ApplyTransform(chosenTag, b.uniques[i].inner, b.params, b.nPixels, buf); err != nil {
			return err
		}
		transformed[i] = buf
	}

	// Each training pick is truncated: ZDICT's d=8 matcher only needs a few KB
	// per sample, full 131 KB tiles slow training without improving the dict.
	trainTiles := opts.trainSampleTiles
	if trainTiles <= 0 {
		trainTiles = 128
	}
	const perPickBytes = 4 * 1024
	stride := len(nonConstIdx) / trainTiles
	if stride < 1 {
		stride = 1
	}
	picks := make([][]byte, 0, trainTiles)
	for i := 0; i < len(nonConstIdx) && len(picks) < trainTiles; i += stride {
		t := transformed[nonConstIdx[i]]
		if len(t) > perPickBytes {
			t = t[:perPickBytes]
		}
		picks = append(picks, t)
	}

	maxDictBytes := opts.maxDictBytes
	if maxDictBytes <= 0 {
		maxDictBytes = 64 * 1024
	}
	level := defaultZstdLevel(opts.level)

	var dict *codec.Dict
	if uint64(len(nonConstIdx)) >= uint64(opts.minTiles) && len(picks) >= 2 {
		if trained, err := codec.TrainDict(picks, maxDictBytes); err == nil && len(trained) > 0 {
			if d, err := codec.NewDict(trained, level); err == nil {
				dict = d
			}
		}
	}

	tileData := make([]byte, 0, len(b.uniques)*8192)
	offsets := make([]uint64, len(b.uniques))
	lengths := make([]uint32, len(b.uniques))
	zr := zstd.NewCtx()
	for i := range b.uniques {
		u := b.uniques[i]
		var blob []byte
		switch {
		case u.tag == codec.IDConstant:
			blob = codec.PackConstantBlob(u.inner)
		case dict != nil:
			b2, err := codec.RepackWithDict(chosenTag, transformed[i], dict)
			if err != nil {
				return err
			}
			blob = b2
		default:
			body, err := zr.CompressLevel(nil, transformed[i], level)
			if err != nil {
				return err
			}
			blob = make([]byte, 1+len(body))
			blob[0] = chosenTag
			copy(blob[1:], body)
		}
		offsets[i] = uint64(len(tileData))
		lengths[i] = uint32(len(blob))
		tileData = append(tileData, blob...)
		// release inner bytes early to bound peak memory
		b.uniques[i].inner = nil
		transformed[i] = nil
	}

	if dict != nil {
		b.dictBytes = dict.Bytes
	}
	b.tileData = tileData
	b.uniqueLen = uint64(len(tileData))
	for i := range b.records {
		idx := b.records[i].offset
		b.records[i].offset = offsets[idx]
		b.records[i].length = lengths[idx]
	}
	for k, v := range b.dedup {
		b.dedup[k] = dedupVal{offset: offsets[v.offset], length: lengths[v.offset]}
	}
	b.uniques = nil
	return nil
}

// pickBlockTransform returns the transform whose sample tiles compress
// smallest. f32 always uses bitshuffle; delta/lorenzo aren't defined for it.
// Samples at zstd L1 — relative ranking is unchanged from L5.
func pickBlockTransform(uniques []uniqueInner, nonConstIdx []int, p quantize.Params, nPixels int) byte {
	if p.DType.Bytes() == 4 {
		return codec.IDBitshuffleZstd
	}
	const sampleN = 4
	const sampleLevel = 1
	stride := len(nonConstIdx) / sampleN
	if stride < 1 {
		stride = 1
	}
	zr := zstd.NewCtx()
	candidates := []byte{codec.IDBitshuffleZstd, codec.IDDeltaZstd, codec.IDLorenzoZstd}
	maxTLen := 0
	for _, tag := range candidates {
		if l := codec.TransformedLen(tag, p, nPixels); l > maxTLen {
			maxTLen = l
		}
	}
	scratch := make([]byte, maxTLen)
	cbuf := make([]byte, zstd.CompressBound(maxTLen))
	totals := make(map[byte]int, len(candidates))
	for i := 0; i < len(nonConstIdx) && i/stride < sampleN; i += stride {
		quant := uniques[nonConstIdx[i]].inner
		for _, tag := range candidates {
			tlen := codec.TransformedLen(tag, p, nPixels)
			if err := codec.ApplyTransform(tag, quant, p, nPixels, scratch[:tlen]); err != nil {
				continue
			}
			body, err := zr.CompressLevel(cbuf[:0], scratch[:tlen], sampleLevel)
			if err != nil {
				continue
			}
			totals[tag] += len(body)
		}
	}
	best := codec.IDBitshuffleZstd
	bestSize := -1
	for _, tag := range candidates {
		if size, ok := totals[tag]; ok && size > 0 {
			if bestSize == -1 || size < bestSize {
				bestSize = size
				best = tag
			}
		}
	}
	return best
}

// finishBlockUniquesNoDict compresses uniques without a dict — used when every
// tile is constant or the trainer refused.
func (b *blockBuilder) finishBlockUniquesNoDict() error {
	tileData := make([]byte, 0, len(b.uniques)*8192)
	offsets := make([]uint64, len(b.uniques))
	lengths := make([]uint32, len(b.uniques))
	zr := zstd.NewCtx()
	for i := range b.uniques {
		u := b.uniques[i]
		var blob []byte
		if u.tag == codec.IDConstant {
			blob = codec.PackConstantBlob(u.inner)
		} else {
			body, err := zr.CompressLevel(nil, u.inner, defaultZstdLevel(0))
			if err != nil {
				return err
			}
			blob = make([]byte, 1+len(body))
			blob[0] = u.tag
			copy(blob[1:], body)
		}
		offsets[i] = uint64(len(tileData))
		lengths[i] = uint32(len(blob))
		tileData = append(tileData, blob...)
	}
	b.tileData = tileData
	b.uniqueLen = uint64(len(tileData))
	for i := range b.records {
		idx := b.records[i].offset
		b.records[i].offset = offsets[idx]
		b.records[i].length = lengths[idx]
	}
	for k, v := range b.dedup {
		b.dedup[k] = dedupVal{offset: offsets[v.offset], length: lengths[v.offset]}
	}
	b.uniques = nil
	return nil
}

func defaultZstdLevel(level int) int {
	if level == 0 {
		return zstd.DefaultCompression
	}
	return level
}

// finishBlocksParallel runs finishBlock on every block over a GOMAXPROCS
// worker pool. Blocks are independent after addEncoded, so no synchronisation
// is needed beyond the result slice.
func finishBlocksParallel(declarations []blockKey, blocks map[blockKey]*blockBuilder, comp format.InternalCompression, dictOpts dictOptions) error {
	n := len(declarations)
	if n == 0 {
		return nil
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, n)
	for i := range declarations {
		jobs <- i
	}
	close(jobs)

	errs := make([]error, n)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				bb := blocks[declarations[i]]
				if e := bb.finishBlock(comp, dictOpts); e != nil {
					errs[i] = e
				}
			}
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// dictOptions controls per-block zstd dictionary training in finishBlock.
type dictOptions struct {
	enabled bool
	// minTiles is the minimum unique tile count that triggers the dict pass.
	minTiles int
	// maxDictBytes caps the trained dict size.
	maxDictBytes int
	// sampleTiles bounds the raw-content fallback dict source set.
	sampleTiles int
	// trainSampleTiles bounds the input set fed to ZDICT_trainFromBuffer.
	trainSampleTiles int
	// level is the zstd level for dict-aware recompression; 0 = library default.
	level int
}

func defaultDictOptions() dictOptions {
	return dictOptions{
		enabled:          true,
		minTiles:         16,
		maxDictBytes:     64 * 1024,
		sampleTiles:      16,
		trainSampleTiles: 16,
		level:            0,
	}
}

type uniqueTile struct {
	oldOffset uint64
	oldLength uint32
	tag       byte
	inner     []byte
}

// optimizeWithDict is the legacy two-pass path: peel each unique tile's zstd
// payload, train a dict, recompress. Commits only when the dict shrinks the
// block; otherwise leaves b unchanged.
func (b *blockBuilder) optimizeWithDict(opts dictOptions) error {
	if uint64(len(b.dedup)) < uint64(opts.minTiles) {
		return nil
	}
	if b.nPixels == 0 {
		return nil
	}

	zr := zstd.NewCtx()

	// stable iteration order keeps the dict reproducible across runs
	views := make([]dedupView, 0, len(b.dedup))
	for _, v := range b.dedup {
		views = append(views, dedupView{off: v.offset, ln: v.length})
	}
	sortDedupViews(views)

	uniques := make([]uniqueTile, 0, len(views))
	for _, v := range views {
		blob := b.tileData[v.off : v.off+uint64(v.ln)]
		tag, inner, err := codec.ExtractInnerBytes(blob, b.params, b.nPixels, zr)
		if err != nil {
			return nil // non-fatal: skip dict pass for this block
		}
		uniques = append(uniques, uniqueTile{
			oldOffset: v.off,
			oldLength: v.ln,
			tag:       tag,
			inner:     inner,
		})
	}

	dictBytes := buildTrainedOrSampleDict(uniques, opts)
	if len(dictBytes) == 0 {
		return nil
	}

	dict, err := codec.NewDict(dictBytes, opts.level)
	if err != nil {
		return nil
	}

	// recompress non-constants with dict; constants stay verbatim
	newTileData := make([]byte, 0, len(b.tileData))
	remap := make(map[uint64]dedupVal, len(uniques))
	for i := range uniques {
		u := &uniques[i]
		var newBlob []byte
		if u.tag == codec.IDConstant || len(u.inner) == 0 {
			old := b.tileData[u.oldOffset : u.oldOffset+uint64(u.oldLength)]
			newBlob = make([]byte, len(old))
			copy(newBlob, old)
		} else {
			newBlob, err = codec.RepackWithDict(u.tag, u.inner, dict)
			if err != nil {
				return nil
			}
		}
		off := uint64(len(newTileData))
		newTileData = append(newTileData, newBlob...)
		remap[u.oldOffset] = dedupVal{offset: off, length: uint32(len(newBlob))}
	}

	if uint64(len(newTileData))+uint64(len(dictBytes)) >= uint64(len(b.tileData)) {
		return nil // dict didn't earn its keep
	}

	b.tileData = newTileData
	b.uniqueLen = uint64(len(newTileData))
	b.dictBytes = dictBytes
	for i := range b.records {
		r := &b.records[i]
		nv, ok := remap[r.offset]
		if !ok {
			return fmt.Errorf("dict pass: missing remap for offset %d", r.offset)
		}
		r.offset = nv.offset
		r.length = nv.length
	}
	for k, v := range b.dedup {
		nv, ok := remap[v.offset]
		if !ok {
			return fmt.Errorf("dict pass: missing dedup remap for offset %d", v.offset)
		}
		b.dedup[k] = nv
	}
	return nil
}

// buildTrainedOrSampleDict trains a dict via ZDICT_trainFromBuffer; on failure
// it falls back to a raw-content concat, which libzstd still treats as a
// long-range prefix.
func buildTrainedOrSampleDict(uniques []uniqueTile, opts dictOptions) []byte {
	candidates := make([]uniqueTile, 0, len(uniques))
	for i := range uniques {
		if uniques[i].tag == codec.IDConstant || len(uniques[i].inner) == 0 {
			continue
		}
		candidates = append(candidates, uniques[i])
	}
	if len(candidates) == 0 {
		return nil
	}

	trainTiles := opts.trainSampleTiles
	if trainTiles <= 0 {
		trainTiles = 64
	}
	stride := len(candidates) / trainTiles
	if stride < 1 {
		stride = 1
	}
	picks := make([][]byte, 0, trainTiles)
	for i := 0; i < len(candidates) && len(picks) < trainTiles; i += stride {
		picks = append(picks, candidates[i].inner)
	}

	if len(picks) >= 2 {
		if dict, err := codec.TrainDict(picks, opts.maxDictBytes); err == nil && len(dict) > 0 {
			return dict
		}
	}
	return buildDictSample(uniques, opts.sampleTiles, opts.maxDictBytes)
}

// buildDictSample picks up to maxTiles non-constant uniques and concatenates a
// slice of each, keeping total bytes under maxBytes.
func buildDictSample(uniques []uniqueTile, maxTiles int, maxBytes int) []byte {
	candidates := uniques[:0:0]
	for i := range uniques {
		if uniques[i].tag == codec.IDConstant || len(uniques[i].inner) == 0 {
			continue
		}
		candidates = append(candidates, uniques[i])
	}
	if len(candidates) == 0 {
		return nil
	}
	if maxTiles <= 0 {
		maxTiles = 1
	}
	// stride sampling — adjacent tiles in dedup order tend to be similar
	stride := len(candidates) / maxTiles
	if stride < 1 {
		stride = 1
	}
	picks := make([]uniqueTile, 0, maxTiles)
	for i := 0; i < len(candidates) && len(picks) < maxTiles; i += stride {
		picks = append(picks, candidates[i])
	}

	per := maxBytes / len(picks)
	if per <= 0 {
		per = maxBytes
	}
	out := make([]byte, 0, maxBytes)
	for i := range picks {
		inner := picks[i].inner
		take := per
		if take > len(inner) {
			take = len(inner)
		}
		out = append(out, inner[:take]...)
		if len(out) >= maxBytes {
			break
		}
	}
	if len(out) > maxBytes {
		out = out[:maxBytes]
	}
	return out
}

// sortDedupViews uses insertion sort — len(v) is bounded by tiles per block.
func sortDedupViews(v []dedupView) {
	for i := 1; i < len(v); i++ {
		x := v[i]
		j := i - 1
		for j >= 0 && v[j].off > x.off {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
}

type dedupView struct {
	off uint64
	ln  uint32
}
