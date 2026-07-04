package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// copyFile
// =============================================================================

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	content := []byte("hello copy")
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst contents = %q, want %q", got, content)
	}
	// Permissions are preserved from the source.
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if si.Mode() != di.Mode() {
		t.Errorf("dst mode = %v, want %v", di.Mode(), si.Mode())
	}
}

func TestCopyFileErrors(t *testing.T) {
	dir := t.TempDir()
	// Missing source.
	if err := copyFile(filepath.Join(dir, "missing"), filepath.Join(dir, "out")); err == nil {
		t.Error("expected error copying a missing source")
	}
	// Un-creatable destination (parent dir does not exist).
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, filepath.Join(dir, "nope", "out")); err == nil {
		t.Error("expected error creating destination under a missing dir")
	}
}

// =============================================================================
// printScanSummary
// =============================================================================

func TestPrintScanSummary(t *testing.T) {
	s := ScanStats{Found: 100, Cached: 90, Symlinks: 2, FullHashed: 1, TotalBytes: 5 << 20}
	m := ManifestStats{New: 10, Updated: 3, Pruned: 4}

	out := captureStdout(t, func() { printScanSummary(s, m) })

	for _, w := range []string{
		"Files found:",
		"Cached (skipped):",
		"New entries:",
		"Hash upgraded:",    // m.Updated > 0
		"Full-hashed:",      // s.FullHashed > 0
		"Symlinks skipped:", // s.Symlinks > 0
		"Pruned (deleted):", // m.Pruned > 0
		"Total size:",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("summary missing %q\n---\n%s", w, out)
		}
	}
}

func TestPrintScanSummaryMinimal(t *testing.T) {
	// With zero optional counters, the conditional lines are omitted.
	out := captureStdout(t, func() { printScanSummary(ScanStats{Found: 5}, ManifestStats{New: 5}) })
	for _, absent := range []string{"Hash upgraded:", "Full-hashed:", "Symlinks skipped:", "Pruned (deleted):"} {
		if strings.Contains(out, absent) {
			t.Errorf("minimal summary should omit %q\n---\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "Files found:") {
		t.Errorf("expected the base summary lines:\n%s", out)
	}
}

// =============================================================================
// deleteStaleManiests
// =============================================================================

func TestDeleteStaleManifestsSSHUnreachable(t *testing.T) {
	t.Setenv("PHOTO_ORGANIZER_SSH_TIMEOUT", "2s")
	dir := t.TempDir()
	// A local manifest exists, but the SSH listing fails, so nothing is touched.
	local := filepath.Join(dir, "photo_manifest_mac_Photos.csv")
	if err := os.WriteFile(local, []byte("scan_path,relative_path\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// .invalid host never resolves -> ssh fails -> function returns silently.
	deleteStaleManiests("mac", "photo-organizer-nonexistent.invalid", dir)

	if _, err := os.Stat(local); err != nil {
		t.Errorf("manifest should be untouched when SSH fails: %v", err)
	}
}
