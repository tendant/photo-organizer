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

func TestCheckBackupShowsWhichFolderHoldsTheCopies(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "a/IMG_1.jpg"), "same-bytes-one")
	writeFile(t, filepath.Join(src, "a/IMG_2.jpg"), "only-here-two")
	writeFile(t, filepath.Join(lib, "2024/2024-03-15/IMG_1.jpg"), "same-bytes-one")
	for _, d := range []string{src, lib} {
		if out, code := runCLIWithHome(t, home, "scan", d, "--no-report"); code != 0 {
			t.Fatalf("scan %s: exit %d\n%s", d, code, out)
		}
	}
	out, code := runCLIWithHome(t, home, "check-backup", src)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"WHERE THE COPIES ARE", lib, "this machine, not a backup", "2024/2024-03-15",
		"1 of them", "have a copy elsewhere on this machine", "have no other copy anywhere"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLookupFindsRenamedCopiesByContent(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, "a")
	b := filepath.Join(home, "b")
	writeFile(t, filepath.Join(a, "IMG_0001.jpg"), "photo-bytes-xyz")
	writeFile(t, filepath.Join(b, "renamed/holiday.jpg"), "photo-bytes-xyz") // same content, other name
	writeFile(t, filepath.Join(b, "IMG_0001.jpg"), "different content")      // same name, other content
	for _, d := range []string{a, b} {
		if out, code := runCLIWithHome(t, home, "scan", d, "--no-report"); code != 0 {
			t.Fatalf("scan: exit %d\n%s", code, out)
		}
	}

	// Default: by content. Finds the renamed copy, not the same-named file with other content.
	out, code := runCLIWithHome(t, home, "lookup", filepath.Join(a, "IMG_0001.jpg"))
	if code != 0 || !strings.Contains(out, filepath.Join(b, "renamed/holiday.jpg")) || !strings.Contains(out, "[this file]") ||
		strings.Contains(out, filepath.Join(b, "IMG_0001.jpg")) {
		t.Errorf("content lookup: exit %d\n%s", code, out)
	}

	// A file that is not in any manifest is hashed on the spot.
	loose := filepath.Join(home, "loose.jpg")
	writeFile(t, loose, "photo-bytes-xyz")
	out, _ = runCLIWithHome(t, home, "lookup", loose)
	if !strings.Contains(out, "found in 2 place(s)") {
		t.Errorf("a loose file should match the two scanned copies:\n%s", out)
	}

	// A bare name does not search by name unless asked.
	out, code = runCLIWithHome(t, home, "lookup", "holiday.jpg")
	if strings.Contains(out, "Same content") || !strings.Contains(out, "--name") {
		t.Errorf("bare name should point at --name: exit %d\n%s", code, out)
	}
	out, _ = runCLIWithHome(t, home, "lookup", "--name", "IMG_0001.jpg")
	if !strings.Contains(out, "2 match(es) by name") || !strings.Contains(out, "content was not compared") {
		t.Errorf("--name output:\n%s", out)
	}
}

func TestCheckBackupTopListsFoldersNothingElseHas(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "game/cache/a.bin"), strings.Repeat("x", 5000))
	writeFile(t, filepath.Join(src, "game/cache/b.bin"), strings.Repeat("y", 5000))
	writeFile(t, filepath.Join(src, "trip/IMG_1.jpg"), "in-library")
	writeFile(t, filepath.Join(lib, "2024/IMG_1.jpg"), "in-library")
	for _, d := range []string{src, lib} {
		if out, code := runCLIWithHome(t, home, "scan", d, "--no-report"); code != 0 {
			t.Fatalf("scan: exit %d\n%s", code, out)
		}
	}
	out, code := runCLIWithHome(t, home, "check-backup", src, "--top", "5", "--depth", "1")
	if code != 0 || !strings.Contains(out, "FILES NO OTHER COPY HAS") || !strings.Contains(out, "2 files") || !strings.Contains(out, "game") ||
		strings.Contains(out, "  trip") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, code := runCLIWithHome(t, home, "check-backup", src, "--bogus"); code == 0 {
		t.Errorf("unknown option should fail")
	}
}

func TestCheckBackupOfAnotherMachinesFolderDoesNotCountItselfAsACopy(t *testing.T) {
	home := t.TempDir()
	drive := filepath.Join(home, "drive")
	writeFile(t, filepath.Join(drive, "machine-id"), "my-drive\n")
	writeFile(t, filepath.Join(drive, "only/a.bin"), "unique-content")
	if out, code := runCLIWithHome(t, home, "scan", drive, "--no-report"); code != 0 {
		t.Fatalf("scan: exit %d\n%s", code, out)
	}
	out, code := runCLIWithHome(t, home, "check-backup", drive, "--top", "3")
	if code != 0 || !strings.Contains(out, "2 files, ") || strings.Contains(out, "WHERE THE COPIES ARE") {
		t.Errorf("a lone file must be 'no copy anywhere else' and not list itself:\n%s", out)
	}
}
