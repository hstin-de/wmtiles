package scan

import (
	"fmt"
	"strings"
)

var levelSuffixFmt = map[string]string{
	"isobaricInhPa":     "_%dmb",
	"isobaricInPa":      "_%dPa",
	"heightAboveGround": "_%dm",
	"heightAboveSea":    "_%dmasl",
	"depthBelowLand":    "_%dm_dbl",
	"sigma":             "_sig%d",
	"hybrid":            "_hyb%d",
}

var levelSuffixLayer = map[string]string{
	"depthBelowLandLayer": "_%d-%dcm",
	"sigmaLayer":          "_sig%d-%d",
	"hybridLayer":         "_hyb%d-%d",
}

var levelSuffixFixed = map[string]string{
	"tropopause":                  "_trop",
	"nominalTop":                  "_top",
	"atmosphere":                  "_atmos",
	"atmosphereSingleLayer":       "_atmos",
	"entireAtmosphere":            "_atmos",
	"isothermZero":                "_0iso",
	"maxWind":                     "_maxwind",
	"highestTroposphericFreezing": "_freezing",
	"lowCloudLayer":               "_lowcld",
	"lowCloudBottom":              "_lowcld",
	"lowCloudTop":                 "_lowcld",
	"middleCloudLayer":            "_midcld",
	"middleCloudBottom":           "_midcld",
	"middleCloudTop":              "_midcld",
	"highCloudLayer":              "_highcld",
	"highCloudBottom":             "_highcld",
	"highCloudTop":                "_highcld",
	"cloudBase":                   "_cldbase",
	"cloudTop":                    "_cldtop",
	"convectiveCloudBottom":       "_ccldbot",
	"convectiveCloudTop":          "_ccldtop",
	"convectiveCloudLayer":        "_ccld",
	"boundaryLayerCloudLayer":     "_blcld",
	"planetaryBoundaryLayer":      "_pbl",
}

// Stable name suffixes; renaming any of these breaks every encoded file
// that uses that variable. Unknown types hit the sanitised fallback at the
// bottom; that path also affects forward compat so think before extending.
func LevelSuffix(typeOfLevel string, level, bottomLevel int) string {
	switch typeOfLevel {
	case "", "surface", "meanSea", "unknown":
		return ""
	case "potentialVorticity":
		return fmt.Sprintf("_pv%d", decodeSignedLevel(level))
	}
	if f, ok := levelSuffixFmt[typeOfLevel]; ok {
		return fmt.Sprintf(f, level)
	}
	if f, ok := levelSuffixLayer[typeOfLevel]; ok {
		return fmt.Sprintf(f, level, bottomLevel)
	}
	if s, ok := levelSuffixFixed[typeOfLevel]; ok {
		return s
	}
	clean := sanitizeName(typeOfLevel)
	if bottomLevel != 0 && bottomLevel != level {
		return fmt.Sprintf("_%s_%d_%d", clean, level, bottomLevel)
	}
	if level != 0 {
		return fmt.Sprintf("_%s_%d", clean, level)
	}
	return "_" + clean
}

// GRIB2 packs the sign in the top bit instead of using two's complement.
func decodeSignedLevel(v int) int {
	const signBit = 1 << 31
	if v >= signBit {
		return -(v - signBit)
	}
	return v
}

// Last-resort suffix for levelTypes not in the maps above; keeps file names
// portable by stripping anything outside [a-z0-9_].
func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}
