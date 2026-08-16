package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingReporter returns a reporter that never draws, so tests can assert on
// the accounting alone.
func countingReporter(total int64) *progressReporter {
	var sink bytes.Buffer
	return newTestReporter(&sink, false, time.Hour, total)
}

func TestCopyFileProgressCountsBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	const size = 100 << 10
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	prog := countingReporter(size)
	if err := copyFileProgress(src, dst, prog); err != nil {
		t.Fatalf("copyFileProgress: %v", err)
	}

	if got := prog.done.Load(); got != size {
		t.Errorf("credited %d bytes, want %d", got, size)
	}
	data, err := os.ReadFile(dst)
	if err != nil || len(data) != size {
		t.Errorf("destination not written correctly: len=%d err=%v", len(data), err)
	}
}

func TestCopyFileNilProgressStillCopies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "hello" {
		t.Errorf("copy = %q, %v; want %q", data, err, "hello")
	}
}

func TestRsyncBatchEnd(t *testing.T) {
	tests := []struct {
		name       string
		batchBytes int64
		batchFiles int
		sizes      []int64
		start      int
		wantEnd    int
		wantAmount int64
	}{
		{
			name: "bytes are the primary bound", batchBytes: 100, batchFiles: 1000,
			sizes: []int64{40, 40, 40, 40}, start: 0,
			wantEnd: 2, wantAmount: 80,
		},
		{
			name: "file cap only trips on a run of tiny files", batchBytes: 1 << 30, batchFiles: 3,
			sizes: []int64{1, 1, 1, 1, 1}, start: 0,
			wantEnd: 3, wantAmount: 3,
		},
		{
			name: "oversized file advances by exactly one", batchBytes: 100, batchFiles: 1000,
			sizes: []int64{5000, 10, 10}, start: 0,
			wantEnd: 1, wantAmount: 5000,
		},
		{
			name: "oversized file mid-list closes the previous batch", batchBytes: 100, batchFiles: 1000,
			sizes: []int64{10, 5000, 10}, start: 0,
			wantEnd: 1, wantAmount: 10,
		},
		{
			name: "exact boundary fills the batch", batchBytes: 100, batchFiles: 1000,
			sizes: []int64{50, 50, 50}, start: 0,
			wantEnd: 2, wantAmount: 100,
		},
		{
			name: "resumes from a mid-list start", batchBytes: 100, batchFiles: 1000,
			sizes: []int64{40, 40, 40, 40}, start: 2,
			wantEnd: 4, wantAmount: 80,
		},
		{
			name: "nil sizes fall back to a count cap", batchBytes: 100, batchFiles: 2,
			sizes: nil, start: 0,
			wantEnd: 2, wantAmount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := 4
			if tt.sizes != nil {
				n = len(tt.sizes)
			}
			end, amount := rsyncBatchEnd(tt.sizes, tt.start, n, tt.batchBytes, tt.batchFiles)
			if end != tt.wantEnd || amount != tt.wantAmount {
				t.Errorf("rsyncBatchEnd() = (%d, %d), want (%d, %d)", end, amount, tt.wantEnd, tt.wantAmount)
			}
			if end <= tt.start {
				t.Errorf("batch must always advance: end=%d start=%d", end, tt.start)
			}
		})
	}
}

func TestPlannedAmount(t *testing.T) {
	batch := []string{"a.jpg", "b.jpg", "c.jpg"}
	sizes := []int64{10, 20, 30}

	tests := []struct {
		name       string
		plan       map[string]bool
		wantAmount int64
		wantItems  int64
	}{
		{"nil plan credits everything", nil, 60, 3},
		{"plan narrows to its members", map[string]bool{"b.jpg": true}, 20, 1},
		{"empty plan credits nothing", map[string]bool{}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amount, items := plannedAmount(batch, sizes, 0, tt.plan)
			if amount != tt.wantAmount || items != tt.wantItems {
				t.Errorf("plannedAmount() = (%d, %d), want (%d, %d)",
					amount, items, tt.wantAmount, tt.wantItems)
			}
		})
	}
}

func TestBatchBounds(t *testing.T) {
	tests := []struct {
		name      string
		envBytes  string
		envFiles  string
		wantBytes int64
		wantFiles int
	}{
		{"defaults", "", "", rsyncBatchBytes, rsyncBatchFiles},
		{"byte override", "1024", "", 1024, rsyncBatchFiles},
		{"file override", "", "5", rsyncBatchBytes, 5},
		{"both overridden", "2048", "7", 2048, 7},
		{"zero is ignored", "0", "0", rsyncBatchBytes, rsyncBatchFiles},
		{"junk is ignored", "big", "lots", rsyncBatchBytes, rsyncBatchFiles},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PHOTO_ORGANIZER_RSYNC_BATCH_BYTES", tt.envBytes)
			t.Setenv("PHOTO_ORGANIZER_RSYNC_BATCH_FILES", tt.envFiles)
			gotBytes, gotFiles := batchBounds()
			if gotBytes != tt.wantBytes || gotFiles != tt.wantFiles {
				t.Errorf("batchBounds() = (%d, %d), want (%d, %d)",
					gotBytes, gotFiles, tt.wantBytes, tt.wantFiles)
			}
		})
	}
}

// useShimRsync installs a fake rsync on PATH for the duration of the test.
func useShimRsync(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "rsync"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRsyncPlanTransfers(t *testing.T) {
	srcRoot := t.TempDir()
	relPaths := []string{"album/photo1.jpg", "album/photo2.jpg", "album/photo3.jpg"}

	tests := []struct {
		name   string
		script string
		want   map[string]bool
	}{
		{
			name:   "parses the out-format list",
			script: "#!/bin/sh\nprintf 'album/\\nalbum/photo1.jpg\\nalbum/photo3.jpg\\n'\n",
			want:   map[string]bool{"album/photo1.jpg": true, "album/photo3.jpg": true},
		},
		{
			name:   "silent rsync declines to narrow",
			script: "#!/bin/sh\nexit 0\n",
			want:   nil,
		},
		{
			name:   "unknown names are ignored",
			script: "#!/bin/sh\nprintf 'some/other/file.jpg\\n'\n",
			want:   nil,
		},
		{
			name:   "failure declines to narrow",
			script: "#!/bin/sh\nexit 1\n",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useShimRsync(t, tt.script)
			got := rsyncPlanTransfers(srcRoot, relPaths, "user@host", "/dest")

			if tt.want == nil {
				if got != nil {
					t.Fatalf("rsyncPlanTransfers() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("rsyncPlanTransfers() = %v, want %v", got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("missing %q in plan %v", k, got)
				}
			}
		})
	}
}

func TestRsyncOneLargeFallback(t *testing.T) {
	const size = 1000

	tests := []struct {
		name   string
		script string
		want   int64
	}{
		{
			name: "progress2 output is credited then topped up",
			// \r-delimited, as real rsync emits it.
			script: "#!/bin/sh\nprintf '        400  40%%  1.00MB/s 0:00:01\\r        900  90%%  1.00MB/s 0:00:00\\r'\n",
			want:   size,
		},
		{
			name:   "unsupported flag falls back to a plain send",
			script: "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --info=progress2) exit 1 ;; esac; done\nexit 0\n",
			want:   size,
		},
		{
			name:   "silent success still credits the file exactly once",
			script: "#!/bin/sh\nexit 0\n",
			want:   size,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The latch is global; each case starts from a clean slate.
			progress2Unavailable.Store(false)
			t.Cleanup(func() { progress2Unavailable.Store(false) })

			useShimRsync(t, tt.script)
			prog := countingReporter(size)
			if err := rsyncOneLarge(t.TempDir(), "big.mov", size, "user@host", "/dest", "", prog); err != nil {
				t.Fatalf("rsyncOneLarge: %v", err)
			}
			if got := prog.done.Load(); got != tt.want {
				t.Errorf("credited %d bytes, want %d", got, tt.want)
			}
		})
	}
}

func TestParseProgress2Bytes(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   int64
		wantOK bool
	}{
		{"plain", "  1234567  38%   84.21MB/s    0:00:24", 1234567, true},
		{"thousands separators", "  1,234,567,890  38%   84.21MB/s    0:00:24", 1234567890, true},
		{"no percent field", "sending incremental file list", 0, false},
		{"empty", "", 0, false},
		{"non-numeric", "abc  38%  1MB/s", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseProgress2Bytes(tt.line)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseProgress2Bytes(%q) = (%d, %v), want (%d, %v)",
					tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestScanCRLinesSplitsOnBothTerminators(t *testing.T) {
	// A \r-only stream must yield separate tokens, not one buffered blob.
	in := "first\rsecond\nthird"
	var got []string
	data := []byte(in)
	for len(data) > 0 {
		adv, tok, _ := scanCRLines(data, true)
		if adv == 0 {
			break
		}
		got = append(got, string(tok))
		data = data[adv:]
	}
	want := []string{"first", "second", "third"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("scanCRLines produced %v, want %v", got, want)
	}
}
