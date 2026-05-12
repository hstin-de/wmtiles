package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// cliOut is kept as a back-compat hook for the few callers that build bespoke
// formatted lines (compare.go's fidelity table). It mirrors whatever the
// renderer is writing to, so test harnesses can still capture output.
var cliOut io.Writer = &renderProxy{}

type renderProxy struct{}

func (renderProxy) Write(p []byte) (int, error) {
	if ui == nil {
		return len(p), nil
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	return ui.out.Write(p)
}

func cliSection(title string) { ui.Section(title) }

func cliKV(label, value string) { ui.KV(label, value) }

func cliKVf(label, format string, args ...any) {
	cliKV(label, fmt.Sprintf(format, args...))
}

func cliTable(headers []string, rows [][]string) { ui.Table(headers, rows, "") }

func cliTableAligned(headers []string, rows [][]string, align string) {
	ui.Table(headers, rows, align)
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
	rate := float64(count) / d.Seconds()
	switch {
	case rate >= 1e6:
		return fmt.Sprintf("%.2fM %s/s", rate/1e6, unit)
	case rate >= 1e3:
		return fmt.Sprintf("%.1fk %s/s", rate/1e3, unit)
	}
	return fmt.Sprintf("%.1f %s/s", rate, unit)
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

// dtypeBadge colours the small dtype tag so it pops in tables.
func dtypeBadge(d string) string {
	if ui == nil {
		return d
	}
	switch d {
	case "u8":
		return ui.styled(d, ansiGreen)
	case "u16":
		return ui.styled(d, ansiCyan)
	case "f32":
		return ui.styled(d, ansiMagenta)
	}
	return d
}

// captureLines is used by tests that previously redirected cliOut to a buffer.
// The renderer is paused, output is captured to buf, then resumed.
func captureLines(fn func()) string {
	var buf bytes.Buffer
	if ui == nil {
		fn()
		return buf.String()
	}
	prev := ui.out
	ui.out = &buf
	defer func() { ui.out = prev }()
	fn()
	return buf.String()
}
