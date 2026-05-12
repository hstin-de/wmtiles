// Single source of truth for the scan helpers. Encoder and CLI used to keep
// their own copies which drifted: same input produced different variable
// names depending on which path the user took.
package scan

import (
	"fmt"
	"math"
	"time"

	"github.com/hstin-de/wmtiles/parser"
)

// VarKey: WMO triplet plus level so 500 hPa and 850 hPa temperature stay
// distinct on the same shortName.
type VarKey struct {
	Discipline, Category, Parameter int
	LevelType                       string
	Level, BottomLevel              int
}

func VarKeyOf(h *parser.GribHeader) VarKey {
	return VarKey{
		Discipline:  h.Discipline,
		Category:    h.ParameterCategory,
		Parameter:   h.ParameterNumber,
		LevelType:   h.TypeOfLevel,
		Level:       h.Level,
		BottomLevel: h.BottomLevel,
	}
}

type VarInfo struct {
	Name         string
	ShortName    string
	Unit         string
	VMin         float64
	VMax         float64
	HasFinite    bool
	MessageCount int
	Times        map[time.Time]struct{}
	// CLI-only: lets the final Variables table say where each block's
	// precision came from (override / auto / cap / default).
	PrecSources map[string]struct{}
}

// Seed VMin/VMax with infinities so the first finite sample replaces them.
func NewVarInfoFor(h *parser.GribHeader, k VarKey) *VarInfo {
	base := h.ShortName
	if base == "" || base == "unknown" {
		base = fmt.Sprintf("param_%d_%d_%d", k.Discipline, k.Category, k.Parameter)
	}
	return &VarInfo{
		Name:        base + LevelSuffix(k.LevelType, k.Level, k.BottomLevel),
		ShortName:   h.ShortName,
		Unit:        h.Units,
		VMin:        math.Inf(+1),
		VMax:        math.Inf(-1),
		Times:       map[time.Time]struct{}{},
		PrecSources: map[string]struct{}{},
	}
}
