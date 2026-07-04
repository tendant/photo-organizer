package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkSource builds a ManifestSource from explicit rows, stamping machine/scan
// path onto each row. Unlike makeSource it lets a test set per-row extensions.
func mkSource(machine, scanPath, lastScanned string, rows []ManifestRow) ManifestSource {
	for i := range rows {
		rows[i].MachineName = machine
		rows[i].ScanPath = scanPath
		if rows[i].Filename == "" {
			rows[i].Filename = filepath.Base(rows[i].RelativePath)
		}
	}
	return ManifestSource{
		MachineName: machine,
		ScanPath:    scanPath,
		Label:       machine + " @ " + scanPath,
		LastScanned: lastScanned,
		Rows:        rows,
	}
}

// =============================================================================
// fileType
// =============================================================================

func TestFileType(t *testing.T) {
	tests := []struct {
		ext, want string
	}{
		{".jpg", "photo"},
		{".mp4", "video"},
		{".mp3", "audio"},
		{".xmp", "sidecar"},
		{".txt", "other"},
		{"", "other"},
	}
	for _, tt := range tests {
		if got := fileType(tt.ext); got != tt.want {
			t.Errorf("fileType(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}

// =============================================================================
// coverageStatus
// =============================================================================

func TestCoverageStatus(t *testing.T) {
	const threshold = 0.9
	tests := []struct {
		coverage float64
		want     string
	}{
		{1.0, "FULL"},
		{0.95, "HIGH"},
		{0.90, "HIGH"},
		{0.60, "MID"},
		{0.50, "MID"},
		{0.30, ""},
	}
	for _, tt := range tests {
		if got := coverageStatus(tt.coverage, threshold); got != tt.want {
			t.Errorf("coverageStatus(%.2f, %.1f) = %q, want %q", tt.coverage, threshold, got, tt.want)
		}
	}
}

// =============================================================================
// computeFolderRedundancy
// =============================================================================

func TestComputeFolderRedundancy(t *testing.T) {
	mac := mkSource("mac", "/Photos", "", []ManifestRow{
		{RelativePath: "Vacation/a.jpg", SizeBytes: 100, PartialHash: "h1", FullHash: "h1"},
		{RelativePath: "Vacation/b.jpg", SizeBytes: 200, PartialHash: "h2", FullHash: "h2"},
		{RelativePath: "Private/c.jpg", SizeBytes: 300, PartialHash: "h3", FullHash: "h3"},
	})
	nas := mkSource("nas", "/vol1", "", []ManifestRow{
		{RelativePath: "x.jpg", SizeBytes: 100, PartialHash: "h1", FullHash: "h1"}, // covers Vacation/a.jpg
		{RelativePath: "y.jpg", SizeBytes: 200, PartialHash: "h2", FullHash: "h2"}, // covers Vacation/b.jpg
	})

	sources := []ManifestSource{mac, nas}
	stats := computeFolderRedundancy(sources, buildHashIndex(sources))

	get := func(label, folder string) (FolderStats, bool) {
		for _, s := range stats {
			if s.SourceLabel == label && s.FolderPath == folder {
				return s, true
			}
		}
		return FolderStats{}, false
	}

	if s, ok := get("mac @ /Photos", "Vacation"); !ok || s.TotalFiles != 2 || s.CoveredFiles != 2 || s.Coverage != 1.0 {
		t.Errorf("mac Vacation = %+v (ok=%v), want fully covered (2/2)", s, ok)
	}
	if s, ok := get("mac @ /Photos", "Private"); !ok || s.TotalFiles != 1 || s.CoveredFiles != 0 || s.Coverage != 0.0 {
		t.Errorf("mac Private = %+v (ok=%v), want uncovered (0/1)", s, ok)
	}
}

// =============================================================================
// computeSummaries
// =============================================================================

func TestComputeSummaries(t *testing.T) {
	mac := mkSource("mac", "/Photos", "", []ManifestRow{
		{RelativePath: "a.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "h1", FullHash: "h1"}, // dup on nas
		{RelativePath: "v.mp4", SizeBytes: 500, Extension: ".mp4", PartialHash: "h2", FullHash: "h2"}, // unique to mac
	})
	nas := mkSource("nas", "/vol1", "", []ManifestRow{
		{RelativePath: "c.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "h1", FullHash: "h1"}, // dup of mac a.jpg
	})

	sources := []ManifestSource{mac, nas}
	summaries := computeSummaries(sources, buildHashIndex(sources))

	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2", len(summaries))
	}
	// Sorted by machine name: mac, then nas.
	m, n := summaries[0], summaries[1]
	if m.MachineName != "mac" || n.MachineName != "nas" {
		t.Fatalf("machine order = %q,%q, want mac,nas", m.MachineName, n.MachineName)
	}

	if m.TotalFiles != 2 || m.TotalBytes != 600 {
		t.Errorf("mac totals = %d files/%d bytes, want 2/600", m.TotalFiles, m.TotalBytes)
	}
	if m.ByType["photo"] != 1 || m.ByType["video"] != 1 {
		t.Errorf("mac ByType = %v, want photo:1 video:1", m.ByType)
	}
	if m.UniqueFiles != 1 || m.DupedFiles != 1 {
		t.Errorf("mac unique/duped = %d/%d, want 1/1", m.UniqueFiles, m.DupedFiles)
	}
	if n.TotalFiles != 1 || n.UniqueFiles != 0 || n.DupedFiles != 1 {
		t.Errorf("nas total/unique/duped = %d/%d/%d, want 1/0/1", n.TotalFiles, n.UniqueFiles, n.DupedFiles)
	}
}

// =============================================================================
// CSV writers
// =============================================================================

func TestWriteDuplicatesCSV(t *testing.T) {
	groups := []DuplicateGroup{
		{PartialHash: "p1", FullHash: "f1", Confirmed: true, SizeBytes: 100, Locations: []string{"mac: a.jpg", "nas: b.jpg"}},
	}
	path := filepath.Join(t.TempDir(), "dups.csv")
	if err := writeDuplicatesCSV(path, groups); err != nil {
		t.Fatalf("writeDuplicatesCSV: %v", err)
	}
	recs := readCSV(t, path)
	if len(recs) != 3 { // header + 2 locations
		t.Fatalf("rows = %d, want 3", len(recs))
	}
	wantHeader := "partial_hash,full_hash,confirmed,size_bytes,location"
	if strings.Join(recs[0], ",") != wantHeader {
		t.Errorf("header = %v, want %s", recs[0], wantHeader)
	}
	if recs[1][0] != "p1" || recs[1][2] != "true" || recs[1][3] != "100" || recs[1][4] != "mac: a.jpg" {
		t.Errorf("row = %v, unexpected values", recs[1])
	}
}

func TestWriteUniqueCSV(t *testing.T) {
	uniqueByMachine := map[string][]ManifestRow{
		"mac": {{RelativePath: "z.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "p", FullHash: "f"}},
		"nas": {{RelativePath: "a.jpg", SizeBytes: 200, Extension: ".jpg", PartialHash: "q", FullHash: "g"}},
	}
	path := filepath.Join(t.TempDir(), "unique.csv")
	if err := writeUniqueCSV(path, uniqueByMachine); err != nil {
		t.Fatalf("writeUniqueCSV: %v", err)
	}
	recs := readCSV(t, path)
	if len(recs) != 3 { // header + 2 machines
		t.Fatalf("rows = %d, want 3", len(recs))
	}
	if strings.Join(recs[0], ",") != "machine_name,relative_path,size_bytes,extension,partial_hash,full_hash" {
		t.Errorf("unexpected header %v", recs[0])
	}
	// Machines are emitted in sorted order: mac before nas.
	if recs[1][0] != "mac" || recs[2][0] != "nas" {
		t.Errorf("machine order = %s,%s, want mac,nas", recs[1][0], recs[2][0])
	}
}

func TestWriteFoldersCSV(t *testing.T) {
	stats := []FolderStats{
		{SourceLabel: "mac @ /Photos", FolderPath: "Vacation", TotalFiles: 2, CoveredFiles: 2, Coverage: 1.0},
		{SourceLabel: "mac @ /Photos", FolderPath: "Private", TotalFiles: 4, CoveredFiles: 1, Coverage: 0.25},
	}
	path := filepath.Join(t.TempDir(), "folders.csv")
	if err := writeFoldersCSV(path, stats); err != nil {
		t.Fatalf("writeFoldersCSV: %v", err)
	}
	recs := readCSV(t, path)
	if len(recs) != 3 {
		t.Fatalf("rows = %d, want 3", len(recs))
	}
	if strings.Join(recs[0], ",") != "source_label,folder_path,total_files,covered_files,coverage_pct,status" {
		t.Errorf("unexpected header %v", recs[0])
	}
	// Fully-covered folder is FULL; 25% folder is below any status band.
	if recs[1][5] != "FULL" || recs[1][4] != "100.0" {
		t.Errorf("row1 = %v, want FULL/100.0", recs[1])
	}
	if recs[2][5] != "" {
		t.Errorf("row2 status = %q, want empty (25%% coverage)", recs[2][5])
	}
}

func TestWriteAnalysisCSV(t *testing.T) {
	mac := mkSource("mac", "/Photos", "", []ManifestRow{
		{RelativePath: "Vacation/a.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "h1", FullHash: "h1"},
		{RelativePath: "Vacation/uniq.mp4", SizeBytes: 500, Extension: ".mp4", PartialHash: "h9", FullHash: "h9"},
	})
	nas := mkSource("nas", "/vol1", "", []ManifestRow{
		{RelativePath: "copy.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "h1", FullHash: "h1"},
	})

	prefix := filepath.Join(t.TempDir(), "analysis")
	out := captureStdout(t, func() {
		if err := writeAnalysisCSV([]ManifestSource{mac, nas}, prefix); err != nil {
			t.Fatalf("writeAnalysisCSV: %v", err)
		}
	})
	if !strings.Contains(out, "Wrote") {
		t.Errorf("expected progress output, got %q", out)
	}

	for _, suffix := range []string{"_duplicates.csv", "_unique.csv", "_folders.csv"} {
		if _, err := os.Stat(prefix + suffix); err != nil {
			t.Errorf("expected %s to be written: %v", prefix+suffix, err)
		}
	}
	// The shared h1 file is the one duplicate group.
	dups := readCSV(t, prefix+"_duplicates.csv")
	if len(dups) < 2 {
		t.Errorf("duplicates csv = %v, want at least one data row", dups)
	}
}

// =============================================================================
// printReport (end-to-end formatting)
// =============================================================================

func TestPrintReport(t *testing.T) {
	// Hashes must be >= 12 chars — printReport slices PartialHash[:12].
	mac := mkSource("mac", "/Photos", "2020-01-01 00:00:00", []ManifestRow{ // old -> stale-age branch
		{RelativePath: "Vacation/a.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "hashAAAAAAAA01", FullHash: "hashAAAAAAAA01"},
		{RelativePath: "Vacation/b.jpg", SizeBytes: 200, Extension: ".jpg", PartialHash: "hashBBBBBBBB02", FullHash: "hashBBBBBBBB02"},
		{RelativePath: "Backup/b2.jpg", SizeBytes: 200, Extension: ".jpg", PartialHash: "hashBBBBBBBB02", FullHash: "hashBBBBBBBB02"}, // intra-machine dup
	})
	nas := mkSource("nas", "/vol1", "", []ManifestRow{ // empty -> "no scan date" branch
		{RelativePath: "copy.jpg", SizeBytes: 100, Extension: ".jpg", PartialHash: "hashAAAAAAAA01", FullHash: "hashAAAAAAAA01"}, // cross-machine dup
		{RelativePath: "unique.mp4", SizeBytes: 300, Extension: ".mp4", PartialHash: "hashCCCCCCCC03", FullHash: "hashCCCCCCCC03"},
	})

	var buf bytes.Buffer
	printReport([]ManifestSource{mac, nas}, 0.9, 0, &buf)
	out := buf.String()

	wants := []string{
		"PHOTO DUPLICATE ANALYSIS",
		"MACHINE SUMMARIES",
		"DUPLICATE GROUPS",
		"FILES UNIQUE TO ONE MACHINE",
		"INTRA-MACHINE DUPLICATES",
		"FOLDER REDUNDANCY",
		"mac",
		"nas",
		"no scan date recorded", // nas stale branch
		"days ago",              // mac stale-age branch
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("report missing %q\n---\n%s", w, out)
		}
	}
}
