package main

import (
	"fmt"
	"math"
	"strings"
)

var precisionByShortName = map[string]float64{
	"t": 0.5, "2t": 0.5, "tmax": 0.5, "tmin": 0.5,
	"skt": 0.5, "stl1": 0.5, "stl2": 0.5, "stl3": 0.5, "stl4": 0.5,
	"d": 0.5, "2d": 0.5, "td": 0.5,

	"u": 0.5, "v": 0.5, "10u": 0.5, "10v": 0.5,
	"100u": 0.5, "100v": 0.5,
	"si": 0.5, "10si": 0.5, "ws": 0.5, "10fg": 0.5, "fg10": 0.5,
	"w": 0.05,

	"sp": 100, "msl": 100, "prmsl": 100, "pres": 100, "mslma": 100,

	"tcc": 1, "lcc": 1, "mcc": 1, "hcc": 1, "ccl": 1, "tciwc": 1,
	"r": 1, "2r": 1, "rh": 1,

	"tp": 0.1, "lsp": 0.1, "cp": 0.1, "rain": 0.1,
	"asnow": 0.1, "sf": 0.1, "sd": 0.5,
	"rprate": 0.05, "csnow": 0.05, "crain": 0.05, "cfrzr": 0.05, "cicep": 0.05,

	"q": 1e-5, "2q": 1e-5,

	"gh": 1, "z": 10, "orog": 1,

	"asob_s": 1, "asob_t": 1, "athb_s": 1, "athb_t": 1,
	"aswdir_s": 1, "aswdifd_s": 1, "aswdifu_s": 1,
	"dswrf": 1, "uswrf": 1, "dlwrf": 1, "ulwrf": 1,
	"ssrd": 1, "strd": 1, "ssr": 1, "str": 1, "nswrs": 1, "nlwrs": 1,

	"cape": 10, "cin": 10, "mlcape": 10, "mucape": 10, "sbcape": 10,

	"vis": 50,
}

var precisionByUnit = map[string]float64{
	"K":        0.5,
	"degC":     0.5,
	"m s**-1":  0.5,
	"m s-1":    0.5,
	"m/s":      0.5,
	"Pa":       100,
	"hPa":      1,
	"%":        1,
	"(0 - 1)":  0.01,
	"kg m-2":   0.1,
	"kg/m^2":   0.1,
	"kg m**-2": 0.1,
	"mm":       0.1,
	"mm/h":     0.05,
	"W m-2":    1,
	"W/m^2":    1,
	"W m**-2":  1,
	"J kg-1":   10,
	"J/kg":     10,
}

func defaultPrecisionFor(shortName, unit string) float64 {
	if p, ok := precisionByShortName[strings.ToLower(strings.TrimSpace(shortName))]; ok {
		return p
	}
	if p, ok := precisionByUnit[strings.TrimSpace(unit)]; ok {
		return p
	}
	return 0
}

func parsePrecisionOverrides(s string) (map[string]float64, error) {
	out := map[string]float64{}
	if s = strings.TrimSpace(s); s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("--precision: bad entry %q (want NAME=VALUE)", part)
		}
		name := strings.TrimSpace(part[:eq])
		valStr := strings.TrimSpace(part[eq+1:])
		var v float64
		if _, err := fmt.Sscanf(valStr, "%f", &v); err != nil {
			return nil, fmt.Errorf("--precision %s: %w", name, err)
		}
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("--precision %s: value %g must be >= 0 and finite", name, v)
		}
		out[name] = v
	}
	return out, nil
}
