package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

var cliOut io.Writer = os.Stdout
var cliSectionStarted bool

func cliSection(title string) {
	if cliSectionStarted {
		fmt.Fprintln(cliOut)
	}
	cliSectionStarted = true
	fmt.Fprintln(cliOut, title)
}

func cliKV(label, value string) {
	fmt.Fprintf(cliOut, "  %-20s %s\n", label+":", value)
}

func cliKVf(label, format string, args ...any) {
	cliKV(label, fmt.Sprintf(format, args...))
}

func cliTable(headers []string, rows [][]string) {
	if len(headers) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i := range headers {
			if i < len(row) && len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
	}
	fmt.Fprint(cliOut, "  ")
	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(cliOut, "  ")
		}
		fmt.Fprintf(cliOut, "%-*s", widths[i], h)
	}
	fmt.Fprintln(cliOut)
	for _, row := range rows {
		fmt.Fprint(cliOut, "  ")
		for i := range headers {
			if i > 0 {
				fmt.Fprint(cliOut, "  ")
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			fmt.Fprintf(cliOut, "%-*s", widths[i], cell)
		}
		fmt.Fprintln(cliOut)
	}
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func formatBBox(b [4]float64) string {
	return fmt.Sprintf("lon %.5g..%.5g, lat %.5g..%.5g", b[0], b[2], b[1], b[3])
}

func formatRange(min, max float64) string {
	minS := formatFloat(min)
	maxS := formatFloat(max)
	if math.IsNaN(min) {
		minS = "n/a"
	}
	if math.IsNaN(max) {
		maxS = "n/a"
	}
	return "[" + minS + ", " + maxS + "]"
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.6g", v)
}

func formatDuration(d time.Duration) string {
	return d.Truncate(time.Millisecond).String()
}

func formatThroughput(count int64, d time.Duration, unit string) string {
	if count <= 0 || d <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f %s/s", float64(count)/d.Seconds(), unit)
}

func formatDedupRatio(contents, addressed uint64) string {
	if addressed == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(contents)/float64(addressed))
}

func commaUint(n uint64) string {
	const maxInt64 = uint64(^uint64(0) >> 1)
	if n <= maxInt64 {
		return commaInt(int64(n))
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func dtypeCodeName(d uint8) string {
	switch d {
	case 0:
		return "u8"
	case 1:
		return "u16"
	case 3:
		return "f32"
	default:
		return fmt.Sprintf("dtype(%d)", d)
	}
}
