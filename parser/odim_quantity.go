package parser

import "hash/fnv"

// maps ODIM `quantity` strings (OPERA spec Table 16) to synthetic WMO triples
// so varKeyOf produces unique keys without a real WMO mapping. ShortName feeds
// the encoder's precision lookup.
type odimQuantityInfo struct {
	ShortName string
	Units     string
	D, C, P   int
}

var odimQuantities = map[string]odimQuantityInfo{
	"DBZH":  {"dbzh", "dBZ", 0, 15, 1},
	"DBZV":  {"dbzv", "dBZ", 0, 15, 2},
	"TH":    {"th", "dBZ", 0, 15, 3},
	"TV":    {"tv", "dBZ", 0, 15, 4},
	"VRAD":  {"vrad", "m s-1", 0, 15, 5},
	"VRADH": {"vradh", "m s-1", 0, 15, 6},
	"VRADV": {"vradv", "m s-1", 0, 15, 7},
	"WRAD":  {"wrad", "m s-1", 0, 15, 8},
	"ZDR":   {"zdr", "dB", 0, 15, 9},
	"RHOHV": {"rhohv", "1", 0, 15, 10},
	"PHIDP": {"phidp", "deg", 0, 15, 11},
	"KDP":   {"kdp", "deg/km", 0, 15, 12},
	"SQI":   {"sqi", "1", 0, 15, 13},
	"SNR":   {"snr", "dB", 0, 15, 14},
	"RATE":  {"rate", "mm/h", 0, 1, 52},
	"ACRR":  {"acrr", "kg m-2", 0, 1, 8},
	"HGHT":  {"hght", "m", 0, 3, 6},
	"VIL":   {"vil", "kg m-2", 0, 16, 1},
	"QIND":  {"qind", "1", 0, 19, 1},
	"CLASS": {"class", "1", 0, 19, 2},
	"BRDR":  {"brdr", "1", 0, 19, 3},
	"OCCUR": {"occur", "1", 0, 19, 4},
	"RR":    {"rr", "mm/h", 0, 1, 52},
}

// unknown quantities land in (0, 254, hash&0xFF) so distinct names still split
// into distinct varKeys; collisions are possible but tolerated.
func resolveODIMQuantity(quantity, unit string) odimQuantityInfo {
	if info, ok := odimQuantities[quantity]; ok {
		if unit != "" && info.Units == "" {
			info.Units = unit
		}
		return info
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(quantity))
	return odimQuantityInfo{
		ShortName: stringsToLower(quantity),
		Units:     unit,
		D:         0,
		C:         254,
		P:         int(h.Sum32() & 0xFF),
	}
}

func stringsToLower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
