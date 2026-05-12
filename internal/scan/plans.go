package scan

import (
	"fmt"
	"sort"

	"github.com/hstin-de/wmtiles/quantize"
)

type VariablePlan struct {
	Name      string
	Unit      string
	Messages  int
	Min       float64
	Max       float64
	Precision float64
	PrecSrc   string
	DType     string
	Step      float64
}

// Variables that never saw a finite sample are dropped: no block was ever
// declared for them so the block table would never reference them.
func FinalVariablePlans(bySig map[VarKey]*VarInfo, overrides map[string]float64) []VariablePlan {
	plans := make([]VariablePlan, 0, len(bySig))
	for _, v := range bySig {
		if !v.HasFinite {
			continue
		}
		precision, src := ResolveBlockPrecision(v, v.VMin, v.VMax, overrides)
		if v.PrecSources != nil {
			src = DominantPrecSource(v.PrecSources)
		}
		params := quantize.FitParams(v.VMin, v.VMax, precision)
		plans = append(plans, VariablePlan{
			Name:      v.Name,
			Unit:      v.Unit,
			Messages:  v.MessageCount,
			Min:       v.VMin,
			Max:       v.VMax,
			Precision: precision,
			PrecSrc:   src,
			DType:     DTypeName(params.DType),
			Step:      params.Scale,
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Name < plans[j].Name })
	return plans
}

func DTypeName(d quantize.DType) string {
	switch d {
	case quantize.DTypeU8:
		return "u8"
	case quantize.DTypeU16:
		return "u16"
	case quantize.DTypeF32:
		return "f32"
	}
	return fmt.Sprintf("dtype(%d)", uint8(d))
}

// Two GRIB messages can collide on shortName when one of them is "unknown"
// or two variants share a level type. Caller invokes this twice with
// different suffix sources (WMO triplet, then level) so the milder
// discriminator wins where it suffices.
func DisambiguateNames(bySig map[VarKey]*VarInfo, suffix func(VarKey) string) {
	counts := map[string]int{}
	for _, v := range bySig {
		counts[v.Name]++
	}
	for k, v := range bySig {
		if counts[v.Name] > 1 {
			v.Name += suffix(k)
		}
	}
}
