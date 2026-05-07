package encode

import (
	"fmt"
	"time"

	"github.com/hstin-de/wmtiles/format"
)

func tileSizeLog2(tileSize int) (uint8, error) {
	switch tileSize {
	case 128:
		return 7, nil
	case 256:
		return 8, nil
	case 512:
		return 9, nil
	case 1024:
		return 10, nil
	default:
		return 0, fmt.Errorf("wmtiles/encode: unsupported tile size %d; use 128, 256, 512 or 1024", tileSize)
	}
}

func validateTimes(times []time.Time) error {
	if len(times) == 0 {
		return fmt.Errorf("wmtiles/encode: no times")
	}
	prev := times[0].UnixMilli()
	for i := 1; i < len(times); i++ {
		cur := times[i].UnixMilli()
		if cur <= prev {
			return fmt.Errorf("wmtiles/encode: times must be strictly increasing; index %d is %s, not after %s",
				i, times[i].UTC().Format(time.RFC3339Nano), times[i-1].UTC().Format(time.RFC3339Nano))
		}
		prev = cur
	}
	return nil
}

func timeCatalogFromTimes(times []time.Time) format.TimeCatalog {
	if len(times) == 1 {
		return format.TimeCatalog{
			Regular:    true,
			StartMs:    times[0].UnixMilli(),
			IntervalMs: 0,
			Count:      1,
		}
	}
	step := times[1].UnixMilli() - times[0].UnixMilli()
	regular := true
	for i := 2; i < len(times); i++ {
		if times[i].UnixMilli()-times[i-1].UnixMilli() != step {
			regular = false
			break
		}
	}
	if regular {
		return format.TimeCatalog{
			Regular:    true,
			StartMs:    times[0].UnixMilli(),
			IntervalMs: step,
			Count:      int64(len(times)),
		}
	}
	out := format.TimeCatalog{
		Regular:      false,
		Count:        int64(len(times)),
		TimestampsMs: make([]int64, len(times)),
	}
	for i, t := range times {
		out.TimestampsMs[i] = t.UnixMilli()
	}
	return out
}
