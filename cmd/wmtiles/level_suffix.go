package main

import "fmt"

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

func levelSuffix(typeOfLevel string, level, bottomLevel int) string {
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
	if level != 0 || bottomLevel != 0 {
		return fmt.Sprintf("_%s_%d", typeOfLevel, level)
	}
	return "_" + typeOfLevel
}

func decodeSignedLevel(v int) int {
	const signBit = 1 << 31
	if v >= signBit {
		return -(v - signBit)
	}
	return v
}
