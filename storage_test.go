package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// detectMountPoint
// =============================================================================

func TestDetectMountPoint(t *testing.T) {
	tests := []struct {
		path, want string
	}{
		{"/data/photos/2023", "/data"}, // root + first component
		{"/Volumes/USB/DCIM", "/Volumes"},
		{"/tankm1", "/tankm1"},         // single component -> unchanged
		{"/", "/"},                     // root
		{"photos/2023", "photos/2023"}, // two-part relative -> unchanged
		{"a/b/c", "a"},                 // multi-part relative (Windows-style branch)
	}
	for _, tt := range tests {
		if got := detectMountPoint(tt.path); got != tt.want {
			t.Errorf("detectMountPoint(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// =============================================================================
// rowBelongsToSource
// =============================================================================

func TestRowBelongsToSource(t *testing.T) {
	src := ManifestSource{MachineName: "mac", ScanPath: "/Photos"}
	match := ManifestRow{MachineName: "mac", ScanPath: "/Photos"}
	if !rowBelongsToSource(match, src, 0) {
		t.Error("matching machine+scan path should belong to source")
	}
	if rowBelongsToSource(ManifestRow{MachineName: "mac", ScanPath: "/Other"}, src, 0) {
		t.Error("different scan path should not belong")
	}
	if rowBelongsToSource(ManifestRow{MachineName: "nas", ScanPath: "/Photos"}, src, 0) {
		t.Error("different machine should not belong")
	}
}

// =============================================================================
// getDiskUsageLocal
// =============================================================================

func TestGetDiskUsageLocal(t *testing.T) {
	// Runs `df` under the hood; result is either empty (df unavailable) or a
	// formatted summary. Assert the format when present rather than a value.
	got := getDiskUsageLocal()
	if got != "" && !(strings.Contains(got, "Disk:") && strings.Contains(got, "available")) {
		t.Errorf("getDiskUsageLocal() = %q, want empty or a Disk/available summary", got)
	}
}

// =============================================================================
// runStorageStatus / runStoragePlan (end-to-end via temp HOME)
// =============================================================================

// seedManifests writes two manifests into $HOME/manifests/_Manifest so the
// storage commands can discover them: a shared file (mac+nas) and a mac-only file.
func seedManifests(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "manifests", "_Manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	writeSearchManifest(t, filepath.Join(dir, "mac.csv"), []searchRow{
		{"mac", "/Photos", "a.jpg", "p1", "h1", "2026-07-01", 100}, // duplicated on nas
		{"mac", "/Photos", "b.jpg", "p2", "h2", "2026-07-01", 200}, // unique to mac
	})
	writeSearchManifest(t, filepath.Join(dir, "nas.csv"), []searchRow{
		{"nas", "/vol1", "copy.jpg", "p1", "h1", "2026-07-01", 100}, // dup of mac a.jpg
	})
}

func TestRunStorageStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home)

	out := captureStdout(t, func() { runStorageStatus(nil) })

	for _, w := range []string{"STORAGE STATUS REPORT", "mac", "nas", "SUMMARY", "Machines scanned: 2"} {
		if !strings.Contains(out, w) {
			t.Errorf("storage status output missing %q\n---\n%s", w, out)
		}
	}
}

func TestRunStorageStatusNoManifests(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty home -> no manifests
	out := captureStdout(t, func() { runStorageStatus(nil) })
	if out != "" {
		t.Errorf("expected no stdout when no manifests, got:\n%s", out)
	}
}

func TestRunStoragePlan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home)

	out := captureStdout(t, func() { runStoragePlan(nil) })

	for _, w := range []string{
		"STORAGE PLANNING REPORT",
		"PRIORITY BACKUPS",
		"STORAGE GAPS",
		"BACKUP TARGETS",
		"CLEANUP OPPORTUNITIES",
		"SUMMARY",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("storage plan output missing %q\n---\n%s", w, out)
		}
	}
}
