package encode

import "strings"

var precisionByShortName = map[string]float64{
	"t": 0.125, "2t": 0.125, "tmax": 0.125, "tmin": 0.125,
	"skt": 0.125, "stl1": 0.125, "stl2": 0.125, "stl3": 0.125, "stl4": 0.125,
	"d": 0.125, "2d": 0.125, "td": 0.125,

	"u": 0.125, "v": 0.125, "10u": 0.125, "10v": 0.125,
	"100u": 0.125, "100v": 0.125,
	"si": 0.125, "10si": 0.125, "ws": 0.125, "10fg": 0.125, "fg10": 0.125,
	"w": 0.01,

	"sp": 25, "msl": 25, "prmsl": 25, "pres": 25, "mslma": 25,

	"tcc": 0.5, "lcc": 0.5, "mcc": 0.5, "hcc": 0.5, "ccl": 0.5,
	"tciwc": 0.001,
	"r":     0.5, "2r": 0.5, "rh": 0.5,

	"tp": 0.05, "lsp": 0.05, "cp": 0.05, "rain": 0.05,
	"asnow": 0.05, "sf": 0.05, "sd": 0.01,
	"rprate": 0.01, "csnow": 0.01, "crain": 0.01, "cfrzr": 0.01, "cicep": 0.01,

	"q": 1e-6, "2q": 1e-6,

	"gh": 0.5, "z": 1, "orog": 1,

	"asob_s": 0.5, "asob_t": 0.5, "athb_s": 0.5, "athb_t": 0.5,
	"aswdir_s": 0.5, "aswdifd_s": 0.5, "aswdifu_s": 0.5,
	"dswrf": 0.5, "uswrf": 0.5, "dlwrf": 0.5, "ulwrf": 0.5,
	"ssrd": 0.5, "strd": 0.5, "ssr": 0.5, "str": 0.5, "nswrs": 0.5, "nlwrs": 0.5,

	"cape": 1, "cin": 1, "mlcape": 1, "mucape": 1, "sbcape": 1,

	"vis": 10,

	"dbzh": 0.5, "dbzv": 0.5, "th": 0.5, "tv": 0.5,
	"rate": 0.01, "rr": 0.01, "acrr": 0.05,
	"vrad": 0.1, "vradh": 0.1, "vradv": 0.1, "wrad": 0.1,
	"zdr":   0.05,
	"rhohv": 0.005,
	"phidp": 0.5,
	"kdp":   0.05,
	"sqi":   0.005, "snr": 0.1,
}

var precisionByUnit = map[string]float64{
	"K":        0.125,
	"degC":     0.125,
	"m s**-1":  0.125,
	"m s-1":    0.125,
	"m/s":      0.125,
	"Pa":       25,
	"hPa":      0.25,
	"%":        0.5,
	"(0 - 1)":  0.005,
	"kg m-2":   0.05,
	"kg/m^2":   0.05,
	"kg m**-2": 0.05,
	"mm":       0.05,
	"mm/h":     0.01,
	"W m-2":    0.5,
	"W/m^2":    0.5,
	"W m**-2":  0.5,
	"J kg-1":   1,
	"J/kg":     1,
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
