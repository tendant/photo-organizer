package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// small pure helpers
// =============================================================================

func TestTruncate(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},         // shorter than max — unchanged
		{"hello", 5, "hello"},          // exactly max — unchanged
		{"hello world", 8, "hello..."}, // truncated with ellipsis
	}
	for _, tt := range tests {
		if got := truncate(tt.s, tt.max); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
	}
}

func TestPct(t *testing.T) {
	tests := []struct {
		part, total int
		want        float64
	}{
		{0, 0, 0}, // divide-by-zero guard
		{1, 4, 25},
		{3, 3, 100},
	}
	for _, tt := range tests {
		if got := pct(tt.part, tt.total); got != tt.want {
			t.Errorf("pct(%d, %d) = %v, want %v", tt.part, tt.total, got, tt.want)
		}
	}
}

// =============================================================================
// parseDateRange
// =============================================================================

func TestParseDateRange(t *testing.T) {
	if s, e := parseDateRange(""); !s.IsZero() || !e.IsZero() {
		t.Errorf("parseDateRange(\"\") = %v,%v, want zero,zero", s, e)
	}

	// Single date spans that whole day.
	start, end := parseDateRange("2024-03-15")
	wantStart := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	wantEnd := wantStart.AddDate(0, 0, 1).Add(-time.Second)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("parseDateRange single = %v..%v, want %v..%v", start, end, wantStart, wantEnd)
	}

	// Explicit range: end is inclusive to end-of-day.
	start, end = parseDateRange("2024-01-01:2024-01-31")
	wantStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	wantEnd = time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).Add(-time.Second)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("parseDateRange range = %v..%v, want %v..%v", start, end, wantStart, wantEnd)
	}
}

// =============================================================================
// displayTableResults / displayGroupedResults
// =============================================================================

func TestDisplayTableResults(t *testing.T) {
	rows := []ManifestRow{
		{MachineName: "nas", ScanPath: "/vol1", RelativePath: "a-copy.jpg", SizeBytes: 100, FullHash: "fa"},
		{MachineName: "mac", ScanPath: "/Photos", RelativePath: "a.jpg", SizeBytes: 100, FullHash: "fa"},
	}
	hashCounts := map[string]int{"fa": 2}

	out := captureStdout(t, func() { displayTableResults(rows, hashCounts) })

	for _, w := range []string{"Machine", "/Photos/a.jpg", "/vol1/a-copy.jpg", "Total: 2 file(s)"} {
		if !strings.Contains(out, w) {
			t.Errorf("table output missing %q\n---\n%s", w, out)
		}
	}
	// mac sorts before nas.
	if strings.Index(out, "mac") > strings.Index(out, "nas") {
		t.Errorf("expected mac row before nas row:\n%s", out)
	}
}

func TestDisplayGroupedResults(t *testing.T) {
	rows := []ManifestRow{
		{MachineName: "mac", ScanPath: "/Photos", RelativePath: "a.jpg", SizeBytes: 100, FullHash: "dup"},
		{MachineName: "nas", ScanPath: "/vol1", RelativePath: "a-copy.jpg", SizeBytes: 100, FullHash: "dup"},
		{MachineName: "mac", ScanPath: "/Photos", RelativePath: "unique.png", SizeBytes: 200, FullHash: "solo"},
	}

	out := captureStdout(t, func() { displayGroupedResults(rows, nil) })

	for _, w := range []string{"[Group 1/1]", "/Photos/a.jpg", "/vol1/a-copy.jpg", "Total: 1 duplicate group(s)"} {
		if !strings.Contains(out, w) {
			t.Errorf("grouped output missing %q\n---\n%s", w, out)
		}
	}
	// Non-duplicate files are not shown.
	if strings.Contains(out, "unique.png") {
		t.Errorf("grouped output should skip non-duplicate file:\n%s", out)
	}
}

// =============================================================================
// writeSearchResults
// =============================================================================

func TestWriteSearchResults(t *testing.T) {
	rows := []ManifestRow{
		{MachineName: "mac", ScanPath: "/Photos", RelativePath: "a.jpg", SizeBytes: 100, FullHash: "fa", ScanDate: "2026-07-01"},
	}
	out := filepath.Join(t.TempDir(), "results.csv")

	msg := captureStdout(t, func() { writeSearchResults(out, rows) })
	if !strings.Contains(msg, "Results written to") {
		t.Errorf("expected confirmation message, got %q", msg)
	}

	recs := readCSV(t, out)
	wantHeader := []string{"machine_name", "scan_path", "relative_path", "file_size_bytes", "full_hash", "scan_date"}
	if len(recs) != 2 {
		t.Fatalf("csv rows = %d, want 2 (header+1)", len(recs))
	}
	if strings.Join(recs[0], ",") != strings.Join(wantHeader, ",") {
		t.Errorf("header = %v, want %v", recs[0], wantHeader)
	}
	if got := recs[1]; got[0] != "mac" || got[2] != "a.jpg" || got[3] != "100" || got[4] != "fa" {
		t.Errorf("row = %v, unexpected values", got)
	}
}

// =============================================================================
// runSearchAnalyze (end-to-end, in-process)
// =============================================================================

type searchRow struct {
	machine, scanPath, rel, partial, full, date string
	size                                        int64
}

// writeSearchManifest writes a minimal readManifest-compatible CSV.
func writeSearchManifest(t *testing.T, path string, rows []searchRow) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"machine_name", "scan_path", "relative_path", "file_size_bytes", "partial_hash", "full_hash", "scan_date"})
	for _, r := range rows {
		w.Write([]string{r.machine, r.scanPath, r.rel, strconv.FormatInt(r.size, 10), r.partial, r.full, r.date})
	}
}

func TestRunSearchAnalyze(t *testing.T) {
	dir := t.TempDir()
	macCSV := filepath.Join(dir, "mac.csv")
	nasCSV := filepath.Join(dir, "nas.csv")
	writeSearchManifest(t, macCSV, []searchRow{
		{"mac", "/Photos", "a.jpg", "pa", "fa", "2026-07-01", 100},
		{"mac", "/Photos", "notes.txt", "pt", "ft", "2026-07-01", 50},
	})
	writeSearchManifest(t, nasCSV, []searchRow{
		{"nas", "/vol1", "a-copy.jpg", "pa", "fa", "2026-07-01", 100}, // duplicate of fa
	})

	t.Run("name filter", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, nasCSV, "-name", "*.jpg"}) })
		for _, w := range []string{"a.jpg", "a-copy.jpg", "Total: 2 file(s)"} {
			if !strings.Contains(out, w) {
				t.Errorf("output missing %q\n%s", w, out)
			}
		}
		if strings.Contains(out, "notes.txt") {
			t.Errorf("*.jpg filter should exclude notes.txt:\n%s", out)
		}
	})

	t.Run("no match", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, "-name", "zzz.zzz"}) })
		if !strings.Contains(out, "No matching files found.") {
			t.Errorf("expected no-match message, got:\n%s", out)
		}
	})

	t.Run("group duplicates", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, nasCSV, "-group"}) })
		if !strings.Contains(out, "[Group 1/1]") || !strings.Contains(out, "duplicate group(s)") {
			t.Errorf("expected one duplicate group, got:\n%s", out)
		}
	})

	t.Run("duplicates-only filter", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, nasCSV, "-duplicates-only"}) })
		if !strings.Contains(out, "Total: 2 file(s)") {
			t.Errorf("expected 2 duplicate files, got:\n%s", out)
		}
		if strings.Contains(out, "notes.txt") {
			t.Errorf("-duplicates-only should exclude the unique notes.txt:\n%s", out)
		}
	})

	t.Run("machine and size filter", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, nasCSV, "-machine", "nas", "-size", "100"}) })
		if !strings.Contains(out, "a-copy.jpg") || !strings.Contains(out, "Total: 1 file(s)") {
			t.Errorf("expected only the nas 100-byte file, got:\n%s", out)
		}
	})

	t.Run("date filter", func(t *testing.T) {
		out := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, "-date", "2026-07-01"}) })
		if !strings.Contains(out, "Total: 2 file(s)") {
			t.Errorf("expected both mac files on 2026-07-01, got:\n%s", out)
		}
	})

	t.Run("csv output", func(t *testing.T) {
		outCSV := filepath.Join(dir, "search_out.csv")
		msg := captureStdout(t, func() { runSearchAnalyze([]string{macCSV, nasCSV, "-name", "*.jpg", "-csv", outCSV}) })
		if !strings.Contains(msg, "Results written to") {
			t.Errorf("expected write confirmation, got:\n%s", msg)
		}
		recs := readCSV(t, outCSV)
		if len(recs) != 3 { // header + 2 jpgs
			t.Errorf("csv rows = %d, want 3 (header+2)", len(recs))
		}
	})
}
