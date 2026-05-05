package encoder

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hstin-de/wmtiles/format"
)

type snapshotPlan struct {
	creationTimeMs  int64
	referenceTimeMs int64
	generation      uint64

	variables []format.VariableEntry

	timeCatalog format.TimeCatalog

	blockTable []format.BlockTableEntry

	metadata map[string]any
}

func buildMetadata(user map[string]any, opts Options, generation uint64, addedBlocks int, now time.Time) map[string]any {
	out := map[string]any{
		"generator":     "wmtiles-encoder/0.2.0",
		"creationTime":  now.UTC().Format(time.RFC3339),
		"tilePixelSize": 1 << opts.TilePixelSizeLog2,
		"minZoom":       opts.MinZoom,
		"maxZoom":       opts.MaxZoom,
	}
	if !opts.ReferenceForecastTime.IsZero() {
		out["forecastReferenceTime"] = opts.ReferenceForecastTime.UTC().Format(time.RFC3339)
	}
	for k, v := range user {
		out[k] = v
	}
	hist, _ := out["snapshotHistory"].([]any)
	hist = append(hist, map[string]any{
		"generation":  generation,
		"time":        now.UTC().Format(time.RFC3339),
		"addedBlocks": addedBlocks,
	})
	out["snapshotHistory"] = hist
	return out
}

func writeSnapshot(plan *snapshotPlan, comp format.InternalCompression) (
	out []byte, regularTime bool, err error,
) {
	varBlob := format.MarshalVariableCatalog(plan.variables)
	varComp, err := format.Compress(varBlob, comp)
	if err != nil {
		return nil, false, fmt.Errorf("compress variable catalog: %w", err)
	}

	tcBlob := format.MarshalTimeCatalog(&plan.timeCatalog)
	var tcOnDisk []byte
	if plan.timeCatalog.Regular {
		tcOnDisk = tcBlob
	} else {
		tcOnDisk, err = format.Compress(tcBlob, comp)
		if err != nil {
			return nil, false, fmt.Errorf("compress time catalog: %w", err)
		}
	}

	rootEntries, leavesBlob, err := partitionBlockTable(plan.blockTable, comp)
	if err != nil {
		return nil, false, fmt.Errorf("partition block table: %w", err)
	}
	rootRaw := format.MarshalBlockTable(rootEntries)
	rootComp, err := format.Compress(rootRaw, comp)
	if err != nil {
		return nil, false, fmt.Errorf("compress block table root: %w", err)
	}
	if len(rootComp) > format.MaxBlockTableRootBytes {
		return nil, false, fmt.Errorf("block-table root %d > limit %d (need finer partitioning)",
			len(rootComp), format.MaxBlockTableRootBytes)
	}

	mdJSON, err := json.Marshal(plan.metadata)
	if err != nil {
		return nil, false, fmt.Errorf("marshal metadata: %w", err)
	}
	mdComp, err := format.Compress(mdJSON, comp)
	if err != nil {
		return nil, false, fmt.Errorf("compress metadata: %w", err)
	}

	hdr := &format.SnapshotHeader{
		SchemaVersion:      format.SnapshotSchemaVersion,
		SnapshotGeneration: plan.generation,
		CreationTimeMs:     plan.creationTimeMs,
		ReferenceTimeMs:    plan.referenceTimeMs,
		NumVariables:       uint16(len(plan.variables)),
		NumTimeSteps:       uint32(plan.timeCatalog.Count),
		NumBlocks:          uint64(len(plan.blockTable)),
	}
	off := uint64(format.SnapshotHeaderSize)
	hdr.VariableCatalogOff = off
	hdr.VariableCatalogLen = uint64(len(varComp))
	off += hdr.VariableCatalogLen
	hdr.TimeCatalogOff = off
	hdr.TimeCatalogLen = uint64(len(tcOnDisk))
	off += hdr.TimeCatalogLen
	hdr.BlockTableRootOff = off
	hdr.BlockTableRootLen = uint64(len(rootComp))
	off += hdr.BlockTableRootLen
	if len(leavesBlob) > 0 {
		hdr.BlockTableLeavesOff = off
		hdr.BlockTableLeavesLen = uint64(len(leavesBlob))
		off += hdr.BlockTableLeavesLen
	}
	hdr.MetadataOff = off
	hdr.MetadataLen = uint64(len(mdComp))
	off += hdr.MetadataLen

	totalLen := off + format.SnapshotTrailerSize

	out = make([]byte, 0, totalLen)
	out = append(out, format.MarshalSnapshotHeader(hdr)...)
	out = append(out, varComp...)
	out = append(out, tcOnDisk...)
	out = append(out, rootComp...)
	out = append(out, leavesBlob...)
	out = append(out, mdComp...)

	crc := format.CRC32C(out)
	tr := &format.SnapshotTrailer{SnapshotTotalLength: totalLen, CRC32C: crc}
	out = append(out, format.MarshalSnapshotTrailer(tr)...)
	return out, plan.timeCatalog.Regular, nil
}

func partitionBlockTable(entries []format.BlockTableEntry, comp format.InternalCompression) (
	root []format.BlockTableEntry, leavesBlob []byte, err error,
) {
	flat := format.MarshalBlockTable(entries)
	flatComp, err := format.Compress(flat, comp)
	if err != nil {
		return nil, nil, err
	}
	if len(flatComp) <= format.MaxBlockTableRootBytes {
		out := make([]format.BlockTableEntry, len(entries))
		copy(out, entries)
		sort.Slice(out, func(i, j int) bool { return out[i].CompositeKey() < out[j].CompositeKey() })
		return out, nil, nil
	}

	k := isqrt(len(entries))
	if k < 2 {
		k = 2
	}
	for {
		root, leavesBlob, ok, err := tryPartitionBlockTable(entries, k, comp)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return root, leavesBlob, nil
		}
		if k >= len(entries) {
			return nil, nil, fmt.Errorf("cannot fit block-table root within %d B even with k=%d leaves",
				format.MaxBlockTableRootBytes, k)
		}
		k *= 2
		if k > len(entries) {
			k = len(entries)
		}
	}
}

func tryPartitionBlockTable(entries []format.BlockTableEntry, k int, comp format.InternalCompression) (
	root []format.BlockTableEntry, leavesBlob []byte, ok bool, err error,
) {
	if k > len(entries) {
		k = len(entries)
	}
	per := (len(entries) + k - 1) / k
	root = make([]format.BlockTableEntry, 0, k)
	leavesBlob = nil
	var leafOff uint64
	for start := 0; start < len(entries); start += per {
		end := start + per
		if end > len(entries) {
			end = len(entries)
		}
		slice := entries[start:end]
		raw := format.MarshalBlockTable(slice)
		compBlob, e := format.Compress(raw, comp)
		if e != nil {
			return nil, nil, false, e
		}
		first := slice[0]
		root = append(root, format.BlockTableEntry{
			VariableID:    first.VariableID,
			TimeID:        first.TimeID,
			IsLeafPointer: true,
			BlockOffset:   leafOff,
			BlockLength:   uint64(len(compBlob)),
		})
		leavesBlob = append(leavesBlob, compBlob...)
		leafOff += uint64(len(compBlob))
	}
	rootRaw := format.MarshalBlockTable(root)
	rootComp, err := format.Compress(rootRaw, comp)
	if err != nil {
		return nil, nil, false, err
	}
	ok = len(rootComp) <= format.MaxBlockTableRootBytes
	return root, leavesBlob, ok, nil
}
