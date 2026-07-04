package main

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns whatever
// was written. Used to exercise the print* helpers that write directly to
// os.Stdout instead of an io.Writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// =============================================================================
// isRemovablePath
// =============================================================================

func TestIsRemovablePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// macOS removable mounts
		{"/Volumes/Untitled", true},
		{"/volumes/MyUSB", true},
		// Linux removable mounts
		{"/mnt/external", true},
		{"/media/pi", true}, // /media/<one> — 3 parts, treated as removable
		// Windows removable drives
		{"D:", true},
		{"E:", true},
		{"C:", false}, // system drive is not removable
		// Permanent storage
		{"/Users/lei/Photos", false},
		{"/home/user/Photos", false},
		{"/volume1/photos", false}, // NAS share, not /volumes/
		// /media paths with permanent-storage keywords
		{"/media/Photos/archived", false},
		{"/media/backups/2023", false},
		{"/media/tank/data", false},
	}
	for _, tt := range tests {
		if got := isRemovablePath(tt.path); got != tt.want {
			t.Errorf("isRemovablePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// =============================================================================
// analyzeBackupCompliance
// =============================================================================

func TestAnalyzeBackupCompliance(t *testing.T) {
	// Target machine "mac" holds three files with differing redundancy:
	//   safe.jpg   — copied on nas1 and nas2 (2 copies elsewhere -> safe)
	//   risky.jpg  — copied on nas1 only     (1 copy elsewhere  -> risky)
	//   crit.jpg   — nowhere else            (0 copies elsewhere -> critical)
	mac := makeSource("mac", "/Photos", []struct {
		rel, hash string
		size      int64
	}{
		{"safe.jpg", "s", 100},
		{"risky.jpg", "r", 200},
		{"crit.jpg", "c", 300},
	})
	nas1 := makeSource("nas1", "/vol1", []struct {
		rel, hash string
		size      int64
	}{
		{"safe.jpg", "s", 100},
		{"risky.jpg", "r", 200},
	})
	nas2 := makeSource("nas2", "/vol2", []struct {
		rel, hash string
		size      int64
	}{
		{"safe.jpg", "s", 100},
	})

	sources := []ManifestSource{mac, nas1, nas2}
	idx := buildHashIndex(sources)
	report := analyzeBackupCompliance(sources, idx, "mac", "")

	if report.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3", report.TotalFiles)
	}
	if report.TotalSize != 600 {
		t.Errorf("TotalSize = %d, want 600", report.TotalSize)
	}

	if len(report.SafeFiles) != 1 {
		t.Fatalf("SafeFiles = %d, want 1", len(report.SafeFiles))
	}
	safe := report.SafeFiles[0]
	if safe.CopiesElsewhere != 2 {
		t.Errorf("safe CopiesElsewhere = %d, want 2", safe.CopiesElsewhere)
	}
	if safe.ComplianceStatus != "safe" {
		t.Errorf("safe ComplianceStatus = %q, want safe", safe.ComplianceStatus)
	}
	if got := strings.Join(safe.CopyMachines, ","); got != "nas1,nas2" {
		t.Errorf("safe CopyMachines = %q, want nas1,nas2 (sorted)", got)
	}
	if report.SafeSize != 100 || report.SafeableSpaceFreed != 100 {
		t.Errorf("SafeSize/SafeableSpaceFreed = %d/%d, want 100/100", report.SafeSize, report.SafeableSpaceFreed)
	}

	if len(report.RiskyFiles) != 1 {
		t.Fatalf("RiskyFiles = %d, want 1", len(report.RiskyFiles))
	}
	if report.RiskyFiles[0].CopiesElsewhere != 1 {
		t.Errorf("risky CopiesElsewhere = %d, want 1", report.RiskyFiles[0].CopiesElsewhere)
	}
	if report.RiskyFiles[0].ComplianceStatus != "risky" {
		t.Errorf("risky ComplianceStatus = %q, want risky", report.RiskyFiles[0].ComplianceStatus)
	}
	if report.RiskySize != 200 {
		t.Errorf("RiskySize = %d, want 200", report.RiskySize)
	}

	if len(report.CriticalFiles) != 1 {
		t.Fatalf("CriticalFiles = %d, want 1", len(report.CriticalFiles))
	}
	if report.CriticalFiles[0].CopiesElsewhere != 0 {
		t.Errorf("critical CopiesElsewhere = %d, want 0", report.CriticalFiles[0].CopiesElsewhere)
	}
	if report.CriticalFiles[0].ComplianceStatus != "critical" {
		t.Errorf("critical ComplianceStatus = %q, want critical", report.CriticalFiles[0].ComplianceStatus)
	}
	if report.CriticalSize != 300 {
		t.Errorf("CriticalSize = %d, want 300", report.CriticalSize)
	}
}

func TestAnalyzeBackupComplianceNoManifests(t *testing.T) {
	mac := makeSource("mac", "/Photos", []struct {
		rel, hash string
		size      int64
	}{
		{"a.jpg", "a", 100},
	})
	sources := []ManifestSource{mac}
	idx := buildHashIndex(sources)

	// No manifests for the requested machine -> empty report.
	report := analyzeBackupCompliance(sources, idx, "ghost", "")
	if report.TotalFiles != 0 {
		t.Errorf("TotalFiles = %d, want 0 for unknown machine", report.TotalFiles)
	}
	if len(report.SafeFiles)+len(report.RiskyFiles)+len(report.CriticalFiles) != 0 {
		t.Error("expected no categorized files for unknown machine")
	}
}

// =============================================================================
// printBackupComplianceReport
// =============================================================================

func TestPrintBackupComplianceReport(t *testing.T) {
	r := BackupComplianceReport{
		Machine:    "mac",
		TargetPath: "/Photos",
		TotalFiles: 8,
		TotalSize:  800,
	}
	// 6 safe files to exercise the "... and N more" truncation branch.
	for i := 0; i < 6; i++ {
		r.SafeFiles = append(r.SafeFiles, BackupComplianceFile{
			Path:         "safe" + string(rune('0'+i)) + ".jpg",
			SizeBytes:    100,
			CopyMachines: []string{"nas1", "nas2"},
		})
	}
	r.RiskyFiles = append(r.RiskyFiles, BackupComplianceFile{Path: "risky.jpg", SizeBytes: 100, CopyMachines: []string{"nas1"}})
	r.CriticalFiles = append(r.CriticalFiles, BackupComplianceFile{Path: "crit.jpg", SizeBytes: 100})

	var buf bytes.Buffer
	printBackupComplianceReport(&buf, r)
	out := buf.String()

	wants := []string{
		"3-2-1 BACKUP COMPLIANCE ANALYSIS",
		"Machine:  mac",
		"Path:     /Photos",
		"SAFE TO DELETE",
		"RISKY",
		"CRITICAL",
		"safe0.jpg",   // sample safe file
		"and 1 more",  // 6 safe files -> 5 shown + 1 more
		"risky.jpg",   // sample risky file
		"crit.jpg",    // sample critical file
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("report output missing %q\n---\n%s", w, out)
		}
	}
}

// =============================================================================
// writeBackupComplianceCSV
// =============================================================================

func TestWriteBackupComplianceCSV(t *testing.T) {
	r := BackupComplianceReport{
		SafeFiles: []BackupComplianceFile{
			{Path: "safe.jpg", SizeBytes: 100, CopiesElsewhere: 2, CopyMachines: []string{"nas1", "nas2"}, ComplianceStatus: "safe"},
		},
		RiskyFiles: []BackupComplianceFile{
			{Path: "risky.jpg", SizeBytes: 200, CopiesElsewhere: 1, CopyMachines: []string{"nas1"}, ComplianceStatus: "risky"},
		},
		CriticalFiles: []BackupComplianceFile{
			{Path: "crit.jpg", SizeBytes: 300, CopiesElsewhere: 0, ComplianceStatus: "critical"},
		},
	}

	prefix := filepath.Join(t.TempDir(), "compliance")
	if err := writeBackupComplianceCSV(prefix, r); err != nil {
		t.Fatalf("writeBackupComplianceCSV: %v", err)
	}

	// Safe CSV: verify header and the single data row.
	recs := readCSV(t, prefix+"_safe.csv")
	wantHeader := []string{"path", "size_bytes", "size_mb", "copies_elsewhere", "copy_machines", "compliance_status"}
	if len(recs) != 2 {
		t.Fatalf("safe csv rows = %d, want 2 (header+1)", len(recs))
	}
	if strings.Join(recs[0], ",") != strings.Join(wantHeader, ",") {
		t.Errorf("safe csv header = %v, want %v", recs[0], wantHeader)
	}
	row := recs[1]
	if row[0] != "safe.jpg" || row[1] != "100" || row[3] != "2" || row[4] != "nas1;nas2" || row[5] != "safe" {
		t.Errorf("safe csv row = %v, unexpected values", row)
	}

	// Risky and critical CSVs should exist with their rows.
	if recs := readCSV(t, prefix+"_risky.csv"); len(recs) != 2 || recs[1][0] != "risky.jpg" {
		t.Errorf("risky csv = %v, want a single risky.jpg row", recs)
	}
	if recs := readCSV(t, prefix+"_critical.csv"); len(recs) != 2 || recs[1][0] != "crit.jpg" {
		t.Errorf("critical csv = %v, want a single crit.jpg row", recs)
	}
}

func TestWriteBackupComplianceCSVEmpty(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "empty")
	if err := writeBackupComplianceCSV(prefix, BackupComplianceReport{}); err != nil {
		t.Fatalf("writeBackupComplianceCSV on empty report: %v", err)
	}
	// No categories -> no files created.
	for _, suffix := range []string{"_safe.csv", "_risky.csv", "_critical.csv"} {
		if _, err := os.Stat(prefix + suffix); !os.IsNotExist(err) {
			t.Errorf("expected no %s for empty report", prefix+suffix)
		}
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return recs
}

// =============================================================================
// checkFolderBackup
// =============================================================================

func TestCheckFolderBackup(t *testing.T) {
	folder := t.TempDir()

	// Create real files so processFile can hash them; content is arbitrary but
	// must be distinct so each gets its own hash.
	writeFile := func(name, content string) (string, int64) {
		path := filepath.Join(folder, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		hash, _ := processFile(path)
		info, _ := os.Stat(path)
		return hash, info.Size()
	}

	safeHash, safeSize := writeFile("safe.jpg", "safe-contents")
	riskyHash, riskySize := writeFile("risky.jpg", "risky-contents")
	writeFile("lonely.jpg", "lonely-contents") // no manifest references it

	// Two non-removable machines back up safe.jpg; only one backs up risky.jpg.
	nas1 := makeSource("nas1", "/vol1/photos", []struct {
		rel, hash string
		size      int64
	}{
		{"safe.jpg", safeHash, safeSize},
		{"risky.jpg", riskyHash, riskySize},
	})
	nas2 := makeSource("nas2", "/vol2/photos", []struct {
		rel, hash string
		size      int64
	}{
		{"safe.jpg", safeHash, safeSize},
	})

	sources := []ManifestSource{nas1, nas2}
	idx := buildHashIndex(sources)
	result := checkFolderBackup(folder, sources, idx, "localmac")

	if result.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3", result.TotalFiles)
	}
	if len(result.SafelyBackedUp) != 1 || result.SafelyBackedUp[0].Path != "safe.jpg" {
		t.Errorf("SafelyBackedUp = %+v, want [safe.jpg]", result.SafelyBackedUp)
	}
	if len(result.SafelyBackedUp) == 1 && result.SafelyBackedUp[0].Locations != 2 {
		t.Errorf("safe file Locations = %d, want 2", result.SafelyBackedUp[0].Locations)
	}
	if len(result.AtRisk) != 1 || result.AtRisk[0].Path != "risky.jpg" {
		t.Errorf("AtRisk = %+v, want [risky.jpg]", result.AtRisk)
	}
	if len(result.NotBackedUp) != 1 || result.NotBackedUp[0].Path != "lonely.jpg" {
		t.Errorf("NotBackedUp = %+v, want [lonely.jpg]", result.NotBackedUp)
	}
	if result.AllBackedUp {
		t.Error("AllBackedUp = true, want false (risky + lonely files present)")
	}
}

func TestCheckFolderBackupRemovableDoesNotCount(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(folder, "photo.jpg")
	if err := os.WriteFile(path, []byte("only-on-removable"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	hash, _ := processFile(path)
	info, _ := os.Stat(path)

	// The only other copy lives on removable media, which must not count as a backup.
	usb := makeSource("usb", "/Volumes/USB", []struct {
		rel, hash string
		size      int64
	}{
		{"photo.jpg", hash, info.Size()},
	})
	sources := []ManifestSource{usb}
	idx := buildHashIndex(sources)
	result := checkFolderBackup(folder, sources, idx, "localmac")

	if len(result.NotBackedUp) != 1 {
		t.Errorf("NotBackedUp = %d, want 1 (removable copy ignored)", len(result.NotBackedUp))
	}
	if len(result.SafelyBackedUp)+len(result.AtRisk) != 0 {
		t.Error("removable-only copy should not be counted as backed up")
	}
}

// =============================================================================
// printCheckBackupResult
// =============================================================================

func TestPrintCheckBackupResult(t *testing.T) {
	r := BackupCheckResult{
		FolderPath:   "/Photos/Trip",
		TotalFiles:   3,
		TotalSize:    600,
		IgnoredFiles: 2,
		SafelyBackedUp: []FileBackupStatus{
			{Path: "safe.jpg", SizeBytes: 100, Locations: 2, LocationDetails: []string{"nas1 @ /vol1"}},
		},
		AtRisk: []FileBackupStatus{
			{Path: "risky.jpg", SizeBytes: 200, Locations: 1, LocationDetails: []string{"nas1 @ /vol1"}},
		},
		NotBackedUp: []FileBackupStatus{
			{Path: "lonely.jpg", SizeBytes: 300},
		},
	}

	out := captureStdout(t, func() { printCheckBackupResult(r) })

	wants := []string{
		"BACKUP STATUS",
		"/Photos/Trip",
		"Ignored (system/sync files): 2",
		"SAFELY BACKED UP",
		"safe.jpg",
		"AT RISK",
		"risky.jpg",
		"NOT BACKED UP",
		"lonely.jpg",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("check-backup output missing %q\n---\n%s", w, out)
		}
	}
}

func TestPrintCheckBackupResultAllBackedUp(t *testing.T) {
	r := BackupCheckResult{
		FolderPath: "/Photos/Trip",
		TotalFiles: 1,
		TotalSize:  100,
		SafelyBackedUp: []FileBackupStatus{
			{Path: "safe.jpg", SizeBytes: 100, Locations: 2, LocationDetails: []string{"nas1 @ /vol1", "nas2 @ /vol2"}},
		},
		AllBackedUp: true,
	}

	out := captureStdout(t, func() { printCheckBackupResult(r) })

	if !strings.Contains(out, "All files are safely backed up") {
		t.Errorf("expected all-safe message, got:\n%s", out)
	}
	if strings.Contains(out, "NOT BACKED UP") || strings.Contains(out, "AT RISK") {
		t.Errorf("unexpected risk sections for a fully backed-up folder:\n%s", out)
	}
}
