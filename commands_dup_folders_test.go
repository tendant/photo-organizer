package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// distinctDevices
// =============================================================================

func TestDistinctDevices(t *testing.T) {
	folders := []*FolderInfo{
		{MachineName: "mac"},
		{MachineName: "mac"}, // same machine, counted once
		{MachineName: "nas"},
		{MachineName: "cam"}, // removable — excluded
		{MachineName: ""},    // unknown — excluded
	}
	cfg := map[string]string{"cam": "[removable] scanned from: /Volumes/USB"}

	if got := distinctDevices(folders, cfg); got != 2 {
		t.Errorf("distinctDevices = %d, want 2 (mac, nas; cam removable, blank skipped)", got)
	}
}

// =============================================================================
// dup-folders --min-devices / Archive? verdict
// =============================================================================

// seedDupFolderManifests writes manifests producing two duplicate-folder groups:
//   - Trip: same content on mac and nas  -> 2 devices (archivable)
//   - DupA: same content twice on mac     -> 1 device  (keep)
func seedDupFolderManifests(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "manifests", "_Manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSearchManifest(t, filepath.Join(dir, "mac_photos.csv"), []searchRow{
		{"mac", "/Photos", "Trip/a.jpg", "t1", "t1", "2026-07-01", 100},
		{"mac", "/Photos", "Trip/b.jpg", "t2", "t2", "2026-07-01", 200},
		{"mac", "/Photos", "DupA/x.jpg", "d1", "d1", "2026-07-01", 300},
	})
	writeSearchManifest(t, filepath.Join(dir, "mac_backup.csv"), []searchRow{
		{"mac", "/Backup", "DupA/x.jpg", "d1", "d1", "2026-07-01", 300},
	})
	writeSearchManifest(t, filepath.Join(dir, "nas.csv"), []searchRow{
		{"nas", "/vol1", "Trip/a.jpg", "t1", "t1", "2026-07-01", 100},
		{"nas", "/vol1", "Trip/b.jpg", "t2", "t2", "2026-07-01", 200},
	})
}

func TestDupFoldersArchivableVerdict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDupFolderManifests(t, home)

	// Full output shows both groups with their verdicts.
	out := captureStdout(t, func() { runFindDuplicateFolders(nil) })
	if !strings.Contains(out, "ARCHIVABLE") {
		t.Errorf("expected the 2-device group marked ARCHIVABLE:\n%s", out)
	}
	if !strings.Contains(out, "KEEP") {
		t.Errorf("expected the 1-device group marked KEEP:\n%s", out)
	}
	if !strings.Contains(out, "/Photos/Trip") || !strings.Contains(out, "/vol1/Trip") {
		t.Errorf("expected both Trip copies listed:\n%s", out)
	}
}

func TestDupFoldersMinDevicesFilter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDupFolderManifests(t, home)

	// --min-devices 2 keeps only the archivable (2-device) group.
	out := captureStdout(t, func() { runFindDuplicateFolders([]string{"--min-devices", "2"}) })
	if !strings.Contains(out, "ARCHIVABLE") {
		t.Errorf("expected the archivable group to remain:\n%s", out)
	}
	if strings.Contains(out, "KEEP") {
		t.Errorf("--min-devices 2 should filter out the single-device group:\n%s", out)
	}
}

func TestDupFoldersMinDevicesSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedDupFolderManifests(t, home)

	out := captureStdout(t, func() { runFindDuplicateFolders([]string{"-s", "--min-devices", "2"}) })
	if !strings.Contains(out, "Archive?") || !strings.Contains(out, "yes") {
		t.Errorf("summary should show Archive? column with a yes row:\n%s", out)
	}
	if strings.Contains(out, "no (1 device)") {
		t.Errorf("single-device group should be filtered from summary:\n%s", out)
	}
}
