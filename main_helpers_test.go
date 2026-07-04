package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// Machine identity helpers (localHostName / hardwareUUIDSuffix / buildMachineID)
// =============================================================================

func TestLocalHostName(t *testing.T) {
	// Always resolves to something non-empty (falls back to "unknown").
	if localHostName() == "" {
		t.Error("localHostName should never be empty")
	}
}

func TestHardwareUUIDSuffix(t *testing.T) {
	// Either empty (detection failed) or a 6-char lowercase hex string.
	got := hardwareUUIDSuffix()
	if got == "" {
		return
	}
	if len(got) != 6 {
		t.Errorf("suffix = %q, want 6 chars", got)
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("suffix %q has non-hex char %q", got, c)
		}
	}
}

func TestBuildMachineID(t *testing.T) {
	id := buildMachineID()
	if id == "" {
		t.Fatal("buildMachineID should not be empty")
	}
	// The id is the hostname, optionally suffixed with "-<uuid>".
	if !strings.HasPrefix(id, localHostName()) {
		t.Errorf("machine id %q should start with host name %q", id, localHostName())
	}
}

// =============================================================================
// pruneManifestEntries
// =============================================================================

func writePruneManifest(t *testing.T, path string, rows [][]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("scan_path,relative_path\n")
	for _, r := range rows {
		b.WriteString(r[0] + "," + r[1] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestPruneManifestEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.csv")
	writePruneManifest(t, path, [][]string{
		{"/Photos", "Vacation/a.jpg"}, // under the pruned folder
		{"/Photos", "Vacation/b.jpg"}, // under the pruned folder
		{"/Photos", "Other/c.jpg"},    // kept
	})

	n, err := pruneManifestEntries(path, "/Photos/Vacation")
	if err != nil {
		t.Fatalf("pruneManifestEntries: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}
	recs := readCSV(t, path)
	if len(recs) != 2 { // header + surviving row
		t.Fatalf("remaining rows = %d, want 2", len(recs))
	}
	if recs[1][1] != "Other/c.jpg" {
		t.Errorf("survivor = %v, want Other/c.jpg", recs[1])
	}
}

func TestPruneManifestEntriesNothingToPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.csv")
	writePruneManifest(t, path, [][]string{{"/Photos", "a.jpg"}})

	n, err := pruneManifestEntries(path, "/Elsewhere")
	if err != nil || n != 0 {
		t.Errorf("prune non-matching = %d/%v, want 0/nil", n, err)
	}
}

func TestPruneManifestEntriesErrors(t *testing.T) {
	// Missing file.
	if _, err := pruneManifestEntries(filepath.Join(t.TempDir(), "missing.csv"), "/x"); err == nil {
		t.Error("expected error for missing manifest")
	}

	// Missing required columns.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.csv")
	if err := os.WriteFile(bad, []byte("foo,bar\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneManifestEntries(bad, "/x"); err == nil {
		t.Error("expected error for manifest missing required columns")
	}
}

// =============================================================================
// displayManifestTable
// =============================================================================

func TestDisplayManifestTable(t *testing.T) {
	layout := "2006-01-02 15:04:05"
	now := time.Now()
	sources := []ManifestSource{
		{MachineName: "fresh-m", ScanPath: "/a", Rows: make([]ManifestRow, 3), LastScanned: now.Add(-30 * time.Minute).Format(layout)},
		{MachineName: "week-m", ScanPath: "/b", Rows: make([]ManifestRow, 1), LastScanned: now.Add(-3 * 24 * time.Hour).Format(layout)},
		{MachineName: "wks-m", ScanPath: "/c", Rows: make([]ManifestRow, 1), LastScanned: now.Add(-14 * 24 * time.Hour).Format(layout)},
		{MachineName: "old-m", ScanPath: "/very/long/scan/path/that/exceeds/twenty-eight-characters", Rows: nil, LastScanned: now.Add(-60 * 24 * time.Hour).Format(layout)},
	}

	out := captureStderr(t, func() { displayManifestTable(sources, "💻 lcl") })

	for _, w := range []string{"fresh-m", "✓ Fresh", "⚡", "wks", "mo", "..."} {
		if !strings.Contains(out, w) {
			t.Errorf("table output missing %q\n---\n%s", w, out)
		}
	}
}

// =============================================================================
// runSearch / runManifests
// =============================================================================

func TestRunSearch(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "m.csv")
	writeSearchManifest(t, csv, []searchRow{
		{"mac", "/Photos", "a.jpg", "p1", "h1", "2026-07-01", 100},
	})
	out := captureStdout(t, func() { runSearch([]string{csv, "-name", "*.jpg"}) })
	if !strings.Contains(out, "a.jpg") || !strings.Contains(out, "Total: 1 file(s)") {
		t.Errorf("runSearch output unexpected:\n%s", out)
	}
}

func TestRunManifestsListing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home)

	out := captureStderr(t, func() { runManifests(nil) })
	for _, w := range []string{"REMOTE MACHINES", "mac", "nas", "Summary:"} {
		if !strings.Contains(out, w) {
			t.Errorf("manifest listing missing %q\n---\n%s", w, out)
		}
	}
}

func TestRunManifestsNoManifests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := captureStderr(t, func() { runManifests(nil) })
	if !strings.Contains(out, "No manifests found") {
		t.Errorf("expected no-manifests message, got:\n%s", out)
	}
}

// withStdin redirects os.Stdin to feed input to functions that read from it.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()
	go func() { io.WriteString(w, input); w.Close() }()
	fn()
}

// captureBoth captures stdout and stderr for one call (runManifests writes to both).
func captureBoth(t *testing.T, fn func()) string {
	t.Helper()
	var errStr string
	out := captureStdout(t, func() { errStr = captureStderr(t, fn) })
	return out + errStr
}

// seedExistingManifest writes a manifest whose scan_path exists, so it is not
// treated as stalled.
func seedExistingManifest(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "manifests", "_Manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSearchManifest(t, filepath.Join(dir, "local.csv"), []searchRow{
		{"local", home, "a.jpg", "p1", "h1", "2026-07-01", 100},
	})
}

func manifestFileCount(t *testing.T, home string) int {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	return len(m)
}

// =============================================================================
// runManifests flag paths
// =============================================================================

func TestRunManifestsRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home) // mac.csv (/Photos), nas.csv (/vol1)

	// Matching scan path removes exactly that manifest.
	out := captureBoth(t, func() { runManifests([]string{"--remove", "/Photos"}) })
	if !strings.Contains(out, "Removed manifest") {
		t.Errorf("expected removal message, got:\n%s", out)
	}
	if n := manifestFileCount(t, home); n != 1 {
		t.Errorf("remaining manifests = %d, want 1 (nas kept)", n)
	}

	// No match reports nothing removed.
	out = captureBoth(t, func() { runManifests([]string{"--remove", "/nowhere"}) })
	if !strings.Contains(out, "No manifest found for") {
		t.Errorf("expected no-match message, got:\n%s", out)
	}
}

func TestRunManifestsStalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home) // /Photos and /vol1 don't exist -> stalled

	out := captureBoth(t, func() { runManifests([]string{"--stalled"}) })
	if !strings.Contains(out, "STALLED MANIFESTS") || !strings.Contains(out, "mac.csv") {
		t.Errorf("expected stalled listing, got:\n%s", out)
	}
}

func TestRunManifestsStalledNone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedExistingManifest(t, home) // scan path exists -> not stalled

	out := captureBoth(t, func() { runManifests([]string{"--stalled"}) })
	if !strings.Contains(out, "No stalled manifests found") {
		t.Errorf("expected no-stalled message, got:\n%s", out)
	}
}

func TestRunManifestsCleanupConfirm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home) // stalled

	out := captureBoth(t, func() {
		withStdin(t, "y\n", func() { runManifests([]string{"--cleanup"}) })
	})
	if !strings.Contains(out, "Removed 2 stalled manifest(s)") {
		t.Errorf("expected 2 removed, got:\n%s", out)
	}
	if n := manifestFileCount(t, home); n != 0 {
		t.Errorf("remaining manifests = %d, want 0", n)
	}
}

func TestRunManifestsCleanupDecline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedManifests(t, home) // stalled

	out := captureBoth(t, func() {
		withStdin(t, "n\n", func() { runManifests([]string{"--cleanup"}) })
	})
	if !strings.Contains(out, "Cancelled") {
		t.Errorf("expected cancellation, got:\n%s", out)
	}
	if n := manifestFileCount(t, home); n != 2 {
		t.Errorf("declining cleanup should keep all manifests, got %d", n)
	}
}

func TestRunManifestsCleanupNone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedExistingManifest(t, home) // not stalled

	out := captureBoth(t, func() { runManifests([]string{"--cleanup"}) })
	if !strings.Contains(out, "No stalled manifests to clean up") {
		t.Errorf("expected nothing-to-clean message, got:\n%s", out)
	}
}
