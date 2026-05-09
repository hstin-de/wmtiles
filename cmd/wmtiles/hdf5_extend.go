package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hstin-de/wmtiles/parser"
	"github.com/hstin-de/wmtiles/tiler"
)

// HDF5 mirror of parseAllMessages: produces the same parsedMsg cache so
// tileFromParsed runs unchanged on extend's HDF5 path.
func parseAllHDF5Messages(path string, filterShortNames string) (
	parsed []parsedMsg,
	bySig map[varKey]*varInfo,
	allTimes []time.Time,
	bbox [4]float64,
	totalSeen, keptSeen int,
	err error,
) {
	files, perr := parser.ParseHDF5File(path)
	if perr != nil {
		err = perr
		return
	}
	var keep map[string]bool
	if filterShortNames != "" {
		keep = map[string]bool{}
		for n := range strings.SplitSeq(filterShortNames, ",") {
			if n = strings.TrimSpace(n); n != "" {
				keep[n] = true
			}
		}
	}

	parsed = make([]parsedMsg, len(files))
	for i, gf := range files {
		if keep != nil && !keep[gf.Header.ShortName] {
			parsed[i] = parsedMsg{header: gf.Header, skipMsg: true}
			continue
		}
		vmin, vmax, hasFin := finiteRange(gf.DataValues, gf.Header.MissingValue)
		parsed[i] = parsedMsg{
			header: gf.Header,
			values: gf.DataValues,
			vmin:   vmin,
			vmax:   vmax,
			hasFin: hasFin,
		}
	}

	bySig = map[varKey]*varInfo{}
	timesSeen := map[time.Time]struct{}{}
	bbox = [4]float64{180, 90, -180, -90}
	bboxInit := false

	for i := range parsed {
		totalSeen++
		if parsed[i].skipMsg {
			continue
		}
		keptSeen++
		h := &parsed[i].header
		k := varKeyOf(h)
		v, ok := bySig[k]
		if !ok {
			base := h.ShortName
			if base == "" || base == "unknown" {
				base = fmt.Sprintf("param_%d_%d_%d", k.d, k.c, k.p)
			}
			v = &varInfo{
				name:        base + levelSuffix(k.levelType, k.level, k.bottomLevel),
				shortName:   h.ShortName,
				unit:        h.Units,
				vmin:        math.Inf(+1),
				vmax:        math.Inf(-1),
				times:       map[time.Time]struct{}{},
				precSources: map[string]struct{}{},
			}
			bySig[k] = v
		}
		v.messageCount++
		v.times[h.ReferenceTime] = struct{}{}

		shell := parser.GRIBFile{Header: *h}
		gw, gs, ge, gn := tiler.GridBBox(&shell)
		if !bboxInit {
			bbox = [4]float64{gw, gs, ge, gn}
			bboxInit = true
		} else {
			if gw < bbox[0] {
				bbox[0] = gw
			}
			if gs < bbox[1] {
				bbox[1] = gs
			}
			if ge > bbox[2] {
				bbox[2] = ge
			}
			if gn > bbox[3] {
				bbox[3] = gn
			}
		}
		timesSeen[h.ReferenceTime] = struct{}{}
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
	return
}
