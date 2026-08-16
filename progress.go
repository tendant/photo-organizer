package main

// progress.go — the one place that decides how long-running phases answer
// "how much is done, and how much longer?".
//
// WHY a shared type: backup has three slow phases (select, copy, manifest
// refresh) and shares its copy helpers with archive. Without this, each phase
// grows its own throttle, its own ETA maths, and its own idea of what a
// non-terminal should see.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// rateAlpha weights the newest throughput sample in the smoothed rate.
	// ~10-sample memory: smooth enough to read, still reacts within seconds.
	rateAlpha = 0.3

	// etaWarmup is how long we refuse to guess. Below this an ETA is noise.
	etaWarmup = 3 * time.Second

	// addByteGate is how many bytes may accumulate before a redraw is even
	// considered. WHY: io.Copy calls Add every 32KB, and time.Now() on every
	// chunk is pure overhead.
	addByteGate = 256 << 10

	// progressWidth matches scan_engine.go's in-place line width.
	progressWidth = 78
)

// progressUnit decides how amounts and rates are worded.
type progressUnit int

const (
	unitFiles progressUnit = iota
	unitBytes
)

// bare renders an amount without its noun ("3.4 GB", "4,731").
func (u progressUnit) bare(n int64) string {
	if u == unitBytes {
		return formatSize(n)
	}
	return formatCount(int(n))
}

// amount renders an amount with its noun ("3.4 GB", "4,731 files").
func (u progressUnit) amount(n int64) string {
	if u == unitBytes {
		return formatSize(n)
	}
	return formatCount(int(n)) + " files"
}

// rate renders a throughput ("58.1 MB/s", "1,204 files/s").
func (u progressUnit) rate(perSec float64) string {
	if u == unitBytes {
		return formatSize(int64(perSec)) + "/s"
	}
	return formatCount(int(perSec)) + " files/s"
}

// progressReporter draws one throttled status line for one phase.
// The zero value is unusable; call newProgressReporter. A nil *progressReporter
// is valid and silently does nothing, so callers never need nil checks.
type progressReporter struct {
	w     io.Writer
	label string
	unit  progressUnit
	tty   bool          // stderr is a character device: redraw in place with \r
	every time.Duration // redraw throttle

	total      int64 // 0 means unknown: no percent, no ETA
	totalItems int64

	enabled  atomic.Bool
	done     atomic.Int64
	items    atomic.Int64
	lastDone atomic.Int64 // read outside mu by Add's cheap gate

	mu        sync.Mutex // guards the fields below
	start     time.Time
	lastAt    time.Time
	lastItems int64
	rate      float64 // smoothed unit/sec; 0 until the first sample
	itemRate  float64 // smoothed files/sec, for the per-file-overhead ETA
	note      string
	drawn     bool // an in-place line is currently on screen
	sampled   bool // at least one interval has been folded into the rates
}

// newProgressReporter builds a reporter for one phase. total may be 0 when the
// size of the work is not yet known. os.Stderr is read here rather than at
// package init because the tests swap it.
func newProgressReporter(label string, unit progressUnit, total, totalItems int64, enabled bool) *progressReporter {
	tty := isTerminal(os.Stderr)
	every := 5 * time.Second
	if tty {
		every = 250 * time.Millisecond
	}
	now := time.Now()
	p := &progressReporter{
		w:          os.Stderr,
		label:      label,
		unit:       unit,
		tty:        tty,
		every:      every,
		total:      total,
		totalItems: totalItems,
		start:      now,
		lastAt:     now,
	}
	p.enabled.Store(enabled)
	return p
}

// Add credits work as done: amount in the reporter's unit, items in files.
func (p *progressReporter) Add(amount, items int64) {
	if p == nil || !p.enabled.Load() {
		return
	}
	done := p.done.Add(amount)
	if items != 0 {
		p.items.Add(items)
	}
	// A file boundary always gets a look-in; mid-file byte chunks are gated.
	if items == 0 && done-p.lastDone.Load() < addByteGate {
		return
	}
	p.draw(false)
}

// SetTotal revises the denominator. Call before the work starts.
func (p *progressReporter) SetTotal(total, totalItems int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = total
	p.totalItems = totalItems
}

// Note sets trailing context — the folder or file currently being handled.
func (p *progressReporter) Note(s string) {
	if p == nil || !p.enabled.Load() {
		return
	}
	p.mu.Lock()
	p.note = s
	p.mu.Unlock()
	p.draw(false)
}

// Clear wipes the in-place line so an error message does not land on top of a
// half-drawn progress line.
func (p *progressReporter) Clear() {
	if p == nil || !p.enabled.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.drawn {
		fmt.Fprintf(p.w, "\r%-80s\r", "")
		p.drawn = false
	}
}

// Done prints a final summary line with elapsed time and stops further drawing.
func (p *progressReporter) Done(summary string) {
	if p == nil || !p.enabled.Load() {
		return
	}
	p.enabled.Store(false)

	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.start)
	line := fmt.Sprintf("%s in %s", summary, formatDuration(elapsed))
	if p.tty {
		// Overwrite the in-place line, then terminate it.
		fmt.Fprintf(p.w, "\r  %-*s\n", progressWidth, truncate(line, progressWidth))
	} else {
		fmt.Fprintf(p.w, "  %s\n", line)
	}
	p.drawn = false
}

// draw renders the current state, honouring the throttle unless force is set.
func (p *progressReporter) draw(force bool) {
	if p == nil || !p.enabled.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if !force && now.Sub(p.lastAt) < p.every {
		return
	}

	done := p.done.Load()
	items := p.items.Load()

	// Fold this interval's throughput into an exponentially weighted average.
	// WHY: photo trees mix 3MB JPEGs with 4GB videos, so a plain total/elapsed
	// average lurches every time a big file lands, while a raw instantaneous
	// rate jitters too much to read.
	if since := now.Sub(p.lastAt); since > 0 {
		sample := float64(done-p.lastDone.Load()) / since.Seconds()
		itemSample := float64(items-p.lastItems) / since.Seconds()
		if !p.sampled {
			p.rate, p.itemRate, p.sampled = sample, itemSample, true
		} else {
			p.rate = rateAlpha*sample + (1-rateAlpha)*p.rate
			p.itemRate = rateAlpha*itemSample + (1-rateAlpha)*p.itemRate
		}
	}
	p.lastAt = now
	p.lastItems = items
	p.lastDone.Store(done)

	line := p.render(done, items, now.Sub(p.start), p.rate, p.itemRate, p.note)
	if p.tty {
		fmt.Fprintf(p.w, "\r  %-*s", progressWidth, truncate(line, progressWidth))
		p.drawn = true
		return
	}
	fmt.Fprintf(p.w, "  %s\n", line)
}

// render builds the status line. It is deliberately pure — no clock, no I/O —
// so the exact format is table-testable.
func (p *progressReporter) render(done, items int64, elapsed time.Duration, rate, itemRate float64, note string) string {
	var b strings.Builder
	b.WriteString(p.label)

	if p.total > 0 {
		fmt.Fprintf(&b, " %3.0f%%  %s / %s", float64(done)/float64(p.total)*100,
			p.unit.bare(done), p.unit.amount(p.total))
	} else {
		fmt.Fprintf(&b, "  %s", p.unit.amount(done))
	}

	// The file counter is only extra information when the unit is bytes.
	if p.unit == unitBytes && p.totalItems > 0 {
		fmt.Fprintf(&b, "  %s / %s files", formatCount(int(items)), formatCount(int(p.totalItems)))
	}

	if rate > 0 {
		fmt.Fprintf(&b, "  %s", p.unit.rate(rate))
	}

	if p.total > 0 {
		// An ETA before the transfer settles is noise; print a placeholder
		// rather than a number that will be wrong by an order of magnitude.
		if remaining, ok := p.eta(done, items, elapsed, rate, itemRate); ok {
			fmt.Fprintf(&b, "  ETA %s", formatDuration(remaining))
		} else {
			b.WriteString("  ETA --")
		}
	}

	if note != "" {
		fmt.Fprintf(&b, "  %s", note)
	}
	return b.String()
}

// eta estimates the time left, or reports false when no honest estimate exists.
//
// It takes the LARGER of two estimates: bytes remaining over the byte rate, and
// files remaining over the file rate. WHY both: transfer cost is partly
// per-byte and partly per-file. A backup of 50,000 thumbnails is dominated by
// per-file overhead, and a bytes-only estimate would cheerfully report "0s"
// with minutes of work left — exactly the false reassurance this feature exists
// to remove. Whichever bound is slower is the one that governs.
func (p *progressReporter) eta(done, items int64, elapsed time.Duration, rate, itemRate float64) (time.Duration, bool) {
	if elapsed < etaWarmup {
		return 0, false
	}

	var remaining time.Duration
	var ok bool
	consider := func(left, perSec float64) {
		if left <= 0 || perSec <= 0 {
			return
		}
		if d := time.Duration(left / perSec * float64(time.Second)); d > remaining {
			remaining = d
		}
		ok = true
	}

	consider(float64(p.total-done), rate)
	if p.totalItems > 0 {
		consider(float64(p.totalItems-items), itemRate)
	}

	// Everything left is zero-sized or already counted: finishing, not unknown.
	if !ok && rate > 0 {
		return 0, true
	}
	return remaining, ok
}

// formatDuration renders a coarse human duration: "8s", "3m10s", "1h04m".
// Deliberately coarse — a second-accurate ETA is false precision.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// isTerminal reports whether f is a character device. Pipes, files, cron, and
// the test harness are not, and \r redraws are unreadable garbage there.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// progressWriter counts bytes as they are written and feeds them to a reporter.
// WHY byte-level: a single 4GB video would otherwise freeze the line for
// minutes with no sign that anything is happening.
type progressWriter struct {
	w    io.Writer
	prog *progressReporter
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	if n > 0 {
		pw.prog.Add(int64(n), 0)
	}
	return n, err
}

// progressMode is how the user asked for progress; auto means "use the default".
type progressMode int

const (
	progressAuto progressMode = iota
	progressOn
	progressOff
)

// progressEnabled resolves --progress/--no-progress against
// PHOTO_ORGANIZER_PROGRESS, defaulting to on.
func progressEnabled(mode progressMode) bool {
	switch mode {
	case progressOn:
		return true
	case progressOff:
		return false
	}
	switch strings.ToLower(os.Getenv("PHOTO_ORGANIZER_PROGRESS")) {
	case "0", "off", "false", "no":
		return false
	case "1", "on", "true", "yes":
		return true
	}
	return true
}
