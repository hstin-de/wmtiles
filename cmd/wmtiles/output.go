package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hstin-de/wmtiles/internal/scan"
)

func printVariablePlans(plans []scan.VariablePlan) {
	if len(plans) == 0 {
		return
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Name < plans[j].Name })
	const maxRows = 20
	visible := plans
	hidden := 0
	if len(plans) > maxRows {
		sort.SliceStable(plans, func(i, j int) bool { return plans[i].Messages > plans[j].Messages })
		visible = plans[:maxRows]
		hidden = len(plans) - maxRows
		sort.Slice(visible, func(i, j int) bool { return visible[i].Name < visible[j].Name })
	}
	rows := make([][]string, 0, len(visible))
	for _, p := range visible {
		rows = append(rows, []string{
			p.Name,
			emptyAsNA(p.Unit),
			fmt.Sprintf("%d", p.Messages),
			formatRange(p.Min, p.Max),
			formatFloat(p.Precision) + " (" + p.PrecSrc + ")",
			dtypeBadge(p.DType),
			formatFloat(p.Step),
		})
	}
	ui.Section("Variables")
	cliTableAligned([]string{"name", "unit", "msgs", "range", "precision", "dtype", "step"}, rows, "llrllll")
	if hidden > 0 {
		ui.KVf("more", "%d variables omitted", hidden)
	}
}

func formatTileRate(rate float64) string {
	switch {
	case rate >= 1e6:
		return fmt.Sprintf("%.2fM tiles/s", rate/1e6)
	case rate >= 1e3:
		return fmt.Sprintf("%.1fk tiles/s", rate/1e3)
	}
	return fmt.Sprintf("%.0f tiles/s", rate)
}

func formatTileRateString(count int64, d time.Duration) string {
	if count <= 0 || d <= 0 {
		return "n/a"
	}
	return formatTileRate(float64(count) / d.Seconds())
}

// compare.go formats its own fidelity table via fmt.Fprintf; the proxy
// keeps it writing into the renderer's output stream instead of os.Stdout
// so test harnesses still see one merged stream.
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

// Tests that used to redirect cliOut directly call this instead so the
// renderer's mutex state stays consistent.
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
