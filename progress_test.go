package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"negative clamps to zero", -1 * time.Second, "0s"},
		{"zero", 0, "0s"},
		{"sub-second truncates", 900 * time.Millisecond, "0s"},
		{"seconds", 45 * time.Second, "45s"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"exactly a minute", 60 * time.Second, "1m00s"},
		{"minutes and seconds", 190 * time.Second, "3m10s"},
		{"just under an hour", 3599 * time.Second, "59m59s"},
		{"exactly an hour", 3600 * time.Second, "1h00m"},
		{"hours and minutes", 3661 * time.Second, "1h01m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDuration(tt.d); got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestProgressRender(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		unit       progressUnit
		total      int64
		totalItems int64
		done       int64
		items      int64
		elapsed    time.Duration
		rate       float64
		itemRate   float64
		note       string
		want       string
	}{
		{
			name: "bytes determinate", label: "Copying", unit: unitBytes,
			total: 5 << 30, totalItems: 1204,
			done: 3 << 30, items: 742,
			elapsed: time.Minute, rate: 60 << 20,
			want: "Copying  60%  3.0 GB / 5.0 GB  742 / 1,204 files  60.0 MB/s  ETA 34s",
		},
		{
			name: "files determinate", label: "Checking", unit: unitFiles,
			total: 12431, done: 4731,
			elapsed: 10 * time.Second, rate: 1204,
			want: "Checking  38%  4,731 / 12,431 files  1,204 files/s  ETA 6s",
		},
		{
			name: "indeterminate total has no percent and no ETA", label: "Checking", unit: unitFiles,
			total: 0, done: 4731,
			elapsed: 10 * time.Second, rate: 1204,
			want: "Checking  4,731 files  1,204 files/s",
		},
		{
			name: "warmup suppresses ETA", label: "Copying", unit: unitBytes,
			total: 5 << 30, totalItems: 1204,
			done: 100 << 20, items: 31,
			elapsed: time.Second, rate: 60 << 20,
			want: "Copying   2%  100.0 MB / 5.0 GB  31 / 1,204 files  60.0 MB/s  ETA --",
		},
		{
			name: "zero rate suppresses ETA and rate", label: "Copying", unit: unitBytes,
			total: 5 << 30, totalItems: 1204,
			done: 100 << 20, items: 31,
			elapsed: time.Minute, rate: 0,
			want: "Copying   2%  100.0 MB / 5.0 GB  31 / 1,204 files  ETA --",
		},
		{
			name: "note is appended", label: "Copying", unit: unitBytes,
			total: 5 << 30, totalItems: 1204,
			done: 3 << 30, items: 742,
			elapsed: time.Minute, rate: 60 << 20,
			note: "sending IMG_4410.mov (1.2 GB)",
			want: "Copying  60%  3.0 GB / 5.0 GB  742 / 1,204 files  60.0 MB/s  ETA 34s  sending IMG_4410.mov (1.2 GB)",
		},
		{
			name: "file counter omitted when unit is files", label: "Checking", unit: unitFiles,
			total: 100, totalItems: 100, done: 50, items: 50,
			elapsed: time.Minute, rate: 10,
			want: "Checking  50%  50 / 100 files  10 files/s  ETA 5s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &progressReporter{
				label: tt.label, unit: tt.unit,
				total: tt.total, totalItems: tt.totalItems,
			}
			got := p.render(tt.done, tt.items, tt.elapsed, tt.rate, tt.itemRate, tt.note)
			if got != tt.want {
				t.Errorf("render()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestProgressETATakesTheSlowerBound(t *testing.T) {
	tests := []struct {
		name       string
		total      int64
		totalItems int64
		done       int64
		items      int64
		elapsed    time.Duration
		rate       float64
		itemRate   float64
		want       time.Duration
		wantOK     bool
	}{
		{
			name:  "warmup refuses to guess",
			total: 100, done: 10, elapsed: time.Second, rate: 10,
			wantOK: false,
		},
		{
			name:  "bytes govern when they are slower",
			total: 1000, totalItems: 10, done: 0, items: 0,
			elapsed: time.Minute, rate: 10, itemRate: 10,
			want: 100 * time.Second, wantOK: true,
		},
		{
			// The small-file case: few bytes left, but many files to churn.
			name:  "files govern when per-file overhead dominates",
			total: 1000, totalItems: 1000, done: 990, items: 100,
			elapsed: time.Minute, rate: 100, itemRate: 10,
			want: 90 * time.Second, wantOK: true,
		},
		{
			name:  "no rate yet means no estimate",
			total: 1000, done: 100, elapsed: time.Minute, rate: 0,
			wantOK: false,
		},
		{
			name:  "nothing left reads as finishing",
			total: 1000, totalItems: 10, done: 1000, items: 10,
			elapsed: time.Minute, rate: 100, itemRate: 1,
			want: 0, wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &progressReporter{unit: unitBytes, total: tt.total, totalItems: tt.totalItems}
			got, ok := p.eta(tt.done, tt.items, tt.elapsed, tt.rate, tt.itemRate)
			if ok != tt.wantOK {
				t.Fatalf("eta() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("eta() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newTestReporter builds a reporter writing to buf, with the TTY decision and
// throttle forced rather than sniffed from the real stderr.
func newTestReporter(buf *bytes.Buffer, tty bool, every time.Duration, total int64) *progressReporter {
	now := time.Now()
	p := &progressReporter{
		w: buf, label: "Copying", unit: unitBytes,
		tty: tty, every: every, total: total,
		start: now, lastAt: now,
	}
	p.enabled.Store(true)
	return p
}

func TestProgressThrottle(t *testing.T) {
	tests := []struct {
		name      string
		every     time.Duration
		adds      int
		wantLines int
	}{
		{"no throttle draws every add plus done", 0, 3, 4},
		{"long throttle draws only done", time.Hour, 3, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := newTestReporter(&buf, false, tt.every, 1000)
			for i := 0; i < tt.adds; i++ {
				p.Add(1, 1)
			}
			p.Done("Copied 3 files")

			got := strings.Count(buf.String(), "\n")
			if got != tt.wantLines {
				t.Errorf("line count = %d, want %d\noutput:\n%s", got, tt.wantLines, buf.String())
			}
		})
	}
}

func TestProgressNonTTYUsesNewlines(t *testing.T) {
	var buf bytes.Buffer
	p := newTestReporter(&buf, false, 0, 1000)
	p.Add(10, 1)
	p.Add(10, 1)
	p.Done("Copied 2 files")

	out := buf.String()
	if strings.Contains(out, "\r") {
		t.Errorf("non-TTY output must not contain carriage returns:\n%q", out)
	}
	if lines := strings.Count(out, "\n"); lines != 3 {
		t.Errorf("line count = %d, want 3:\n%s", lines, out)
	}
}

func TestProgressTTYUsesCarriageReturn(t *testing.T) {
	var buf bytes.Buffer
	p := newTestReporter(&buf, true, 0, 1000)
	p.Add(10, 1)
	p.Add(10, 1)

	out := buf.String()
	if !strings.HasPrefix(out, "\r  ") {
		t.Errorf("TTY output should start with a carriage return and indent:\n%q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("TTY progress must not emit newlines before Done:\n%q", out)
	}
	// Each redraw is "\r" + two spaces + a 78-column padded field.
	for _, seg := range strings.Split(strings.TrimPrefix(out, "\r"), "\r") {
		if len(seg) != 2+progressWidth {
			t.Errorf("segment width = %d, want %d: %q", len(seg), 2+progressWidth, seg)
		}
	}

	p.Done("Copied 2 files")
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("Done must terminate the in-place line with a newline")
	}
}

func TestProgressNilSafe(t *testing.T) {
	var p *progressReporter
	// None of these may panic — callers should never need a nil check.
	p.Add(1, 1)
	p.SetTotal(10, 10)
	p.Note("x")
	p.Clear()
	p.Done("y")
}

func TestProgressDisabledIsSilent(t *testing.T) {
	var buf bytes.Buffer
	p := newTestReporter(&buf, false, 0, 1000)
	p.enabled.Store(false)

	p.Add(10, 1)
	p.Note("x")
	p.Done("Copied 1 file")

	if buf.Len() != 0 {
		t.Errorf("disabled reporter wrote output: %q", buf.String())
	}
}

func TestIsTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	regular, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer regular.Close()

	// The true case needs a PTY, so it is exercised by hand rather than here.
	tests := []struct {
		name string
		f    *os.File
	}{
		{"pipe is not a terminal", w},
		{"regular file is not a terminal", regular},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isTerminal(tt.f) {
				t.Errorf("isTerminal(%s) = true, want false", tt.name)
			}
		})
	}
}

func TestProgressEnabled(t *testing.T) {
	tests := []struct {
		name string
		mode progressMode
		env  string
		want bool
	}{
		{"default is on", progressAuto, "", true},
		{"flag on beats nothing", progressOn, "", true},
		{"flag off beats nothing", progressOff, "", false},
		{"flag off beats env on", progressOff, "1", false},
		{"flag on beats env off", progressOn, "0", true},
		{"env 0 disables", progressAuto, "0", false},
		{"env off disables", progressAuto, "off", false},
		{"env false disables", progressAuto, "FALSE", false},
		{"env 1 enables", progressAuto, "1", true},
		{"unrecognised env falls back to default", progressAuto, "banana", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PHOTO_ORGANIZER_PROGRESS", tt.env)
			if got := progressEnabled(tt.mode); got != tt.want {
				t.Errorf("progressEnabled(%v) with env %q = %v, want %v", tt.mode, tt.env, got, tt.want)
			}
		})
	}
}
