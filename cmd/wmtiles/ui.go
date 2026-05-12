package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Output always goes to stderr so `wmtiles inspect | jq` stays clean.
// While a phase is active a background goroutine rewrites the active line
// in place on TTYs; non-TTY mode falls back to one line per phase end.
type Renderer struct {
	out  io.Writer
	mu   sync.Mutex
	mode renderMode

	width          int
	sectionStarted bool

	active   *Phase
	stopTick chan struct{}
	tickDone chan struct{}
}

type renderMode uint8

const (
	modeTTY renderMode = iota
	modePlain
	modePipe
)

func (m renderMode) color() bool   { return m == modeTTY }
func (m renderMode) live() bool    { return m == modeTTY }
func (m renderMode) heading() bool { return m != modePipe }

var ui *Renderer

func initRenderer() *Renderer {
	out := os.Stderr
	r := &Renderer{out: out, width: detectWidth(out)}
	switch {
	case forceMode("WMTILES_OUTPUT") == "plain":
		r.mode = modePlain
	case forceMode("WMTILES_OUTPUT") == "pipe":
		r.mode = modePipe
	case isTTY(out) && !noColor():
		r.mode = modeTTY
	case isTTY(out):
		r.mode = modePlain
	default:
		r.mode = modePipe
	}
	ui = r
	return r
}

func forceMode(env string) string { return strings.ToLower(strings.TrimSpace(os.Getenv(env))) }

func noColor() bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return true
	}
	if v := os.Getenv("TERM"); v == "dumb" {
		return true
	}
	return false
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func detectWidth(f *os.File) int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 20 {
			return n
		}
	}
	if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 20 {
		return int(ws.Col)
	}
	return 100
}

// styled is a no-op when colour is off so CI/pipe output stays plain.
const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiRed      = "\x1b[31m"
	ansiGreen    = "\x1b[32m"
	ansiMagenta  = "\x1b[35m"
	ansiCyan     = "\x1b[36m"
	ansiGrey     = "\x1b[38;5;244m"
	ansiBrCyan   = "\x1b[96m"
	ansiBrYellow = "\x1b[93m"
	clearLine    = "\r\x1b[2K"
)

func (r *Renderer) styled(s string, codes ...string) string {
	if !r.mode.color() {
		return s
	}
	var b strings.Builder
	for _, c := range codes {
		b.WriteString(c)
	}
	b.WriteString(s)
	b.WriteString(ansiReset)
	return b.String()
}

// Single line so noisy CI logs don't blow up.
func (r *Renderer) Banner(command, subtitle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == modePipe {
		fmt.Fprintf(r.out, "wmtiles %s %s\n", command, subtitle)
		return
	}
	title := r.styled("wmtiles", ansiBold, ansiBrCyan)
	cmd := r.styled(command, ansiBold)
	sub := r.styled(subtitle, ansiDim)
	fmt.Fprintln(r.out)
	fmt.Fprintf(r.out, " %s %s  %s\n", title, cmd, sub)
	fmt.Fprintln(r.out)
}

func (r *Renderer) Section(title string) {
	r.endPhase("")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sectionStarted {
		fmt.Fprintln(r.out)
	}
	r.sectionStarted = true
	if r.mode.heading() {
		fmt.Fprintln(r.out, r.styled(title, ansiBold))
	} else {
		fmt.Fprintln(r.out, title)
	}
}

func (r *Renderer) KV(label, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeKV(label, value)
}

func (r *Renderer) writeKV(label, value string) {
	if r.mode == modePipe {
		fmt.Fprintf(r.out, "  %s: %s\n", label, value)
		return
	}
	lab := r.styled(fmt.Sprintf("%-20s", label+":"), ansiGrey)
	fmt.Fprintf(r.out, "  %s %s\n", lab, value)
}

func (r *Renderer) KVf(label, format string, args ...any) {
	r.KV(label, fmt.Sprintf(format, args...))
}

// align is one rune per column: 'l' or 'r'. Anything else defaults to left.
func (r *Renderer) Table(headers []string, rows [][]string, align string) {
	r.endPhase("")
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(headers) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i := range headers {
			if i < len(row) && visibleLen(row[i]) > widths[i] {
				widths[i] = visibleLen(row[i])
			}
		}
	}
	right := func(i int) bool {
		if i >= len(align) {
			return false
		}
		return align[i] == 'r'
	}

	fmt.Fprint(r.out, "  ")
	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(r.out, "  ")
		}
		cell := h
		if right(i) {
			cell = fmt.Sprintf("%*s", widths[i], cell)
		} else {
			cell = fmt.Sprintf("%-*s", widths[i], cell)
		}
		fmt.Fprint(r.out, r.styled(cell, ansiDim))
	}
	fmt.Fprintln(r.out)
	for _, row := range rows {
		fmt.Fprint(r.out, "  ")
		for i := range headers {
			if i > 0 {
				fmt.Fprint(r.out, "  ")
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			pad := widths[i] - visibleLen(cell)
			if pad < 0 {
				pad = 0
			}
			if right(i) {
				fmt.Fprint(r.out, strings.Repeat(" ", pad), cell)
			} else {
				fmt.Fprint(r.out, cell, strings.Repeat(" ", pad))
			}
		}
		fmt.Fprintln(r.out)
	}
}

// Inserts a permanent line above the active phase without losing the phase.
func (r *Renderer) Log(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode.live() && r.active != nil {
		fmt.Fprint(r.out, clearLine)
		fmt.Fprintln(r.out, msg)
		r.drawPhaseLocked()
		return
	}
	fmt.Fprintln(r.out, msg)
}

// Setter methods are goroutine-safe so the tile worker pool can update
// progress without coordinating with the renderer.
type Phase struct {
	r     *Renderer
	name  string
	start time.Time

	total   atomic.Int64
	current atomic.Int64
	extraV  atomic.Value
	done    atomic.Bool

	// Last-two-samples ETA: full-run average would underestimate ramp-up
	// and overestimate steady-state, both ugly.
	emaMu     sync.Mutex
	lastTs    time.Time
	lastCur   int64
	smoothed  float64
	smoothSet bool

	spinIdx int
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// total=0 switches the renderer from progress bar to spinner+counter; use
// it whenever the upper bound isn't known up front.
func (r *Renderer) StartPhase(name string, total int64) *Phase {
	r.endPhase("")
	p := &Phase{r: r, name: name, start: time.Now()}
	p.total.Store(total)
	p.extraV.Store("")

	r.mu.Lock()
	r.active = p
	if r.mode == modePipe {
		fmt.Fprintf(r.out, "  -> %s\n", name)
	} else {
		r.drawPhaseLocked()
	}
	if r.mode.live() {
		r.stopTick = make(chan struct{})
		r.tickDone = make(chan struct{})
		go r.tickLoop(p, r.stopTick, r.tickDone, 80*time.Millisecond)
	} else if r.mode == modePlain {
		r.stopTick = make(chan struct{})
		r.tickDone = make(chan struct{})
		go r.tickLoop(p, r.stopTick, r.tickDone, 750*time.Millisecond)
	}
	r.mu.Unlock()
	return p
}

func (r *Renderer) tickLoop(p *Phase, stop chan struct{}, done chan struct{}, every time.Duration) {
	defer close(done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if p.done.Load() {
				return
			}
			r.mu.Lock()
			if r.active == p {
				r.drawPhaseLocked()
			}
			r.mu.Unlock()
		}
	}
}

func (p *Phase) SetTotal(n int64)    { p.total.Store(n) }
func (p *Phase) AddCurrent(n int64)  { p.current.Add(n) }
func (p *Phase) SetCurrent(n int64)  { p.current.Store(n) }
func (p *Phase) SetExtra(s string)   { p.extraV.Store(s) }
func (p *Phase) Current() int64      { return p.current.Load() }
func (p *Phase) Total() int64        { return p.total.Load() }

// Final extra wins over the in-flight extra set during polling so the last
// line reads as a summary, not the latest poll snapshot.
func (p *Phase) Done(extra string) {
	if p.done.Swap(true) {
		return
	}
	p.r.endPhase(extra)
}

func (r *Renderer) endPhase(finalExtra string) {
	r.mu.Lock()
	p := r.active
	if p == nil {
		r.mu.Unlock()
		return
	}
	stop, done := r.stopTick, r.tickDone
	r.active = nil
	r.stopTick = nil
	r.tickDone = nil
	r.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}

	p.done.Store(true)
	extra := finalExtra
	if extra == "" {
		if v := p.extraV.Load(); v != nil {
			extra = v.(string)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode.live() {
		fmt.Fprint(r.out, clearLine)
	}
	r.writePhaseLineFinal(p, extra)
}

func (r *Renderer) writePhaseLineFinal(p *Phase, extra string) {
	elapsed := time.Since(p.start)
	name := r.styled(fmt.Sprintf("%-18s", p.name), ansiBold)
	check := r.styled("✓", ansiGreen)
	if r.mode == modePipe {
		check = "done"
	}
	suffix := r.styled(formatDuration(elapsed), ansiDim)
	if extra != "" {
		fmt.Fprintf(r.out, "  %s %s  %s  %s\n", check, name, extra, suffix)
	} else {
		fmt.Fprintf(r.out, "  %s %s  %s\n", check, name, suffix)
	}
}

func (r *Renderer) drawPhaseLocked() {
	p := r.active
	if p == nil {
		return
	}
	line := r.renderPhaseLine(p)
	if r.mode.live() {
		fmt.Fprint(r.out, clearLine, line)
	} else if r.mode == modePlain {
		fmt.Fprintln(r.out, line)
	}
}

func (r *Renderer) renderPhaseLine(p *Phase) string {
	total := p.Total()
	cur := p.Current()
	extra := ""
	if v := p.extraV.Load(); v != nil {
		extra = v.(string)
	}

	frame := spinnerFrames[p.spinIdx%len(spinnerFrames)]
	p.spinIdx++
	spinner := r.styled(frame, ansiCyan)
	name := r.styled(fmt.Sprintf("%-18s", p.name), ansiBold)

	if total > 0 {
		pct := float64(cur) / float64(total)
		if pct > 1 {
			pct = 1
		}
		bar := r.renderBar(pct, 20)
		pctStr := r.styled(fmt.Sprintf("%5.1f%%", pct*100), ansiBrYellow)
		eta := p.estimateETA(cur, total)
		etaStr := ""
		if eta > 0 {
			etaStr = r.styled("eta "+formatDurationShort(eta), ansiDim)
		}
		if extra != "" {
			return fmt.Sprintf("  %s %s %s %s  %s  %s", spinner, name, bar, pctStr, extra, etaStr)
		}
		return fmt.Sprintf("  %s %s %s %s  %s", spinner, name, bar, pctStr, etaStr)
	}

	counter := r.styled(commaInt(cur), ansiBrYellow)
	if extra != "" {
		return fmt.Sprintf("  %s %s  %s  %s", spinner, name, counter, extra)
	}
	return fmt.Sprintf("  %s %s  %s", spinner, name, counter)
}

func (r *Renderer) renderBar(pct float64, width int) string {
	filled := int(math.Round(pct * float64(width)))
	if filled > width {
		filled = width
	}
	if r.mode.color() {
		return "[" + r.styled(strings.Repeat("█", filled), ansiGreen) +
			r.styled(strings.Repeat("░", width-filled), ansiGrey) + "]"
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

// EMA over recent samples; whole-run average would make ETA lag through the
// last few percent.
func (p *Phase) estimateETA(cur, total int64) time.Duration {
	if cur <= 0 || total <= 0 || cur >= total {
		return 0
	}
	p.emaMu.Lock()
	defer p.emaMu.Unlock()
	now := time.Now()
	if !p.smoothSet {
		p.lastTs = p.start
		p.lastCur = 0
	}
	dt := now.Sub(p.lastTs).Seconds()
	if dt < 0.1 {
		if p.smoothSet && p.smoothed > 0 {
			remain := float64(total - cur)
			return time.Duration(remain / p.smoothed * float64(time.Second))
		}
		return 0
	}
	rate := float64(cur-p.lastCur) / dt
	if rate < 0 {
		rate = 0
	}
	const alpha = 0.4
	if !p.smoothSet {
		p.smoothed = rate
		p.smoothSet = true
	} else {
		p.smoothed = alpha*rate + (1-alpha)*p.smoothed
	}
	p.lastTs = now
	p.lastCur = cur
	if p.smoothed <= 0 {
		return 0
	}
	remain := float64(total - cur)
	return time.Duration(remain / p.smoothed * float64(time.Second))
}

func (r *Renderer) Summary(rows [][2]string) {
	r.endPhase("")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sectionStarted {
		fmt.Fprintln(r.out)
	}
	r.sectionStarted = true
	if r.mode == modePipe {
		for _, kv := range rows {
			fmt.Fprintf(r.out, "  %s: %s\n", kv[0], kv[1])
		}
		return
	}
	maxLabel := 0
	for _, kv := range rows {
		if len(kv[0]) > maxLabel {
			maxLabel = len(kv[0])
		}
	}
	title := r.styled("done", ansiBold, ansiGreen)
	fmt.Fprintln(r.out, title)
	for _, kv := range rows {
		lab := r.styled(fmt.Sprintf("%-*s", maxLabel, kv[0]+":"), ansiGrey)
		fmt.Fprintf(r.out, "  %s %s\n", lab, kv[1])
	}
}

func formatDurationShort(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%02dm", h, m)
}

// Without ANSI stripping, coloured table cells would shift the column
// padding so plain cells in the same column don't line up.
func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' || r == 'K' || r == 'H' || r == 'J' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

func commaInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	first := len(s) % 3
	var b strings.Builder
	if first > 0 {
		b.WriteString(s[:first])
	}
	for i := first; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
