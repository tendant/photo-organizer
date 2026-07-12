package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHome gives the test an isolated HOME and checkpoint dir and returns it.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PHOTO_ORGANIZER_CHECKPOINT_DIR", filepath.Join(home, "checkpoints"))
	return home
}

func wantFailed(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, errFailed) {
		t.Errorf("%s: err = %v, want errFailed", what, err)
	}
}

func wantNil(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: err = %v, want nil", what, err)
	}
}

// =============================================================================
// runScan
// =============================================================================

func TestRunScanHappyPath(t *testing.T) {
	home := setupHome(t)
	scanDir := filepath.Join(home, "photos")
	writeFilesInto(t, scanDir, "a.jpg", "b.txt")

	out := captureBoth(t, func() {
		wantNil(t, runScan([]string{scanDir, "--no-report"}), "first scan")
	})
	if !strings.Contains(out, "Files found") {
		t.Errorf("expected scan summary, got:\n%s", out)
	}
	manifests, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	if len(manifests) != 1 {
		t.Fatalf("manifests written = %d, want 1", len(manifests))
	}

	// Second scan warns that the folder was already scanned.
	out = captureBoth(t, func() {
		wantNil(t, runScan([]string{scanDir, "--no-report"}), "rescan")
	})
	if !strings.Contains(out, "already scanned") {
		t.Errorf("expected already-scanned warning, got:\n%s", out)
	}
}

func TestRunScanMissingDir(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() {
		wantFailed(t, runScan([]string{filepath.Join(home, "nope"), "--no-report"}), "missing dir")
	})
}

func TestRunScanAutoIdentify(t *testing.T) {
	home := setupHome(t)
	root := filepath.Join(home, "disk")
	writeFilesInto(t, filepath.Join(root, "PhotosVac"),
		"IMG_1.jpg", "IMG_2.jpg", "IMG_3.jpg", "IMG_4.jpg", "IMG_5.jpg")
	writeFilesInto(t, filepath.Join(root, "junk"), "f1.txt", "f2.txt")

	out := captureBoth(t, func() {
		err := runScan([]string{root, "--no-report", "--auto-identify-folders", "--detect-only"})
		wantNil(t, err, "detect-only")
	})
	if !strings.Contains(out, "PhotosVac") {
		t.Errorf("expected qualifying folder listed, got:\n%s", out)
	}
}

// =============================================================================
// runBackup / runRestore / runListArchives
// =============================================================================

func TestRunBackup(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runBackup(nil), "usage") })
	captureBoth(t, func() {
		wantFailed(t, runBackup([]string{filepath.Join(home, "nope"), filepath.Join(home, "arch")}), "no manifest")
	})

	writeManifestFixture(t, home, "machine-a", "photos", map[string]string{
		"trip/a.jpg": "aaa", "b.jpg": "bbb",
	})
	scanDir := filepath.Join(home, "photos")
	archiveRoot := filepath.Join(home, "archive")

	out := captureBoth(t, func() {
		wantNil(t, runBackup([]string{scanDir, archiveRoot}), "backup")
	})
	if !strings.Contains(out, "BACKUP COMPLETE") {
		t.Errorf("expected completion banner, got:\n%s", out)
	}
	archived, _ := filepath.Glob(filepath.Join(archiveRoot, "*", "trip", "a.jpg"))
	if len(archived) != 1 {
		t.Errorf("expected trip/a.jpg inside timestamped archive, glob found %v", archived)
	}
}

func TestRunBackupNewOnly(t *testing.T) {
	home := setupHome(t)
	shared := map[string]string{"a.jpg": "same-bytes", "b.jpg": "same-two"}
	writeManifestFixture(t, home, "machine-a", "photos", shared)
	writeManifestFixture(t, home, "machine-b", "mirror", shared) // independent backup

	out := captureBoth(t, func() {
		wantNil(t, runBackup([]string{filepath.Join(home, "photos"), filepath.Join(home, "arch"), "--new-only"}), "new-only")
	})
	if !strings.Contains(out, "Files skipped:   2") {
		t.Errorf("expected both files skipped (backed up on machine-b), got:\n%s", out)
	}
}

func TestRunRestore(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runRestore(nil), "usage") })
	captureBoth(t, func() {
		wantFailed(t, runRestore([]string{filepath.Join(home, "nope"), filepath.Join(home, "out")}), "missing archive")
	})

	archive := filepath.Join(home, "archive-src")
	writeFilesInto(t, filepath.Join(archive, "trip"), "a.jpg")
	dest := filepath.Join(home, "restored")

	out := captureBoth(t, func() {
		wantNil(t, runRestore([]string{archive, dest}), "restore")
	})
	if !strings.Contains(out, "RESTORE COMPLETE") {
		t.Errorf("expected completion banner, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "trip", "a.jpg")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

func TestRunListArchives(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runListArchives(nil), "usage") })
	captureBoth(t, func() {
		wantFailed(t, runListArchives([]string{filepath.Join(home, "nope")}), "missing root")
	})

	empty := filepath.Join(home, "emptyroot")
	os.MkdirAll(empty, 0o755)
	out := captureBoth(t, func() { wantNil(t, runListArchives([]string{empty}), "empty root") })
	if !strings.Contains(out, "No archives found") {
		t.Errorf("expected empty message, got:\n%s", out)
	}

	// Root with one timestamped archive folder.
	root := filepath.Join(home, "archroot")
	writeFilesInto(t, filepath.Join(root, "2026-06-19-143022-photos"), "a.jpg")
	out = captureBoth(t, func() { wantNil(t, runListArchives([]string{root}), "list") })
	if !strings.Contains(out, "2026-06-19-143022-photos") {
		t.Errorf("expected archive listed, got:\n%s", out)
	}
}

// =============================================================================
// runVerifyBackup / runSignManifest / runRepairManifest
// =============================================================================

func TestRunVerifyBackup(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runVerifyBackup(nil), "usage") })
	captureBoth(t, func() {
		wantFailed(t, runVerifyBackup([]string{filepath.Join(home, "nope.csv")}), "bad manifest")
	})

	manifest := writeManifestFixture(t, home, "machine-a", "photos", map[string]string{
		"a.jpg": "aaa", "b.jpg": "bbb",
	})

	out := captureBoth(t, func() { wantNil(t, runVerifyBackup([]string{manifest}), "healthy") })
	if !strings.Contains(out, "healthy") {
		t.Errorf("expected healthy verdict, got:\n%s", out)
	}

	// Delete a file — verification must fail.
	os.Remove(filepath.Join(home, "photos", "a.jpg"))
	out = captureBoth(t, func() { wantFailed(t, runVerifyBackup([]string{manifest}), "missing file") })
	if !strings.Contains(out, "issues") {
		t.Errorf("expected issues verdict, got:\n%s", out)
	}
}

func TestRunSignManifest(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runSignManifest(nil), "usage") })

	manifest := writeManifestFixture(t, home, "machine-a", "photos", map[string]string{"a.jpg": "aaa"})
	captureBoth(t, func() { wantFailed(t, runSignManifest([]string{manifest}), "missing --key") })

	out := captureBoth(t, func() {
		wantNil(t, runSignManifest([]string{manifest, "--key", "secret"}), "sign")
	})
	if !strings.Contains(out, "signed successfully") {
		t.Errorf("expected signing confirmation, got:\n%s", out)
	}
}

func TestRunRepairManifest(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runRepairManifest(nil), "usage") })

	manifest := writeManifestFixture(t, home, "machine-a", "photos", map[string]string{
		"a.jpg": "aaa", "b.jpg": "bbb",
	})
	scanDir := filepath.Join(home, "photos")

	captureBoth(t, func() {
		wantFailed(t, runRepairManifest([]string{manifest, filepath.Join(home, "nope")}), "missing archive")
	})

	// Remove one file, then repair against the scan dir.
	os.Remove(filepath.Join(scanDir, "a.jpg"))
	captureBoth(t, func() {
		wantNil(t, runRepairManifest([]string{manifest, scanDir}), "repair")
	})
	src, err := readManifest(manifest)
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	for _, row := range src.Rows {
		if row.RelativePath == "a.jpg" {
			t.Errorf("repair should have pruned the missing a.jpg entry")
		}
	}
}

// =============================================================================
// runArchive
// =============================================================================

func TestRunArchive(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runArchive(nil), "usage") })
	captureBoth(t, func() {
		wantFailed(t, runArchive([]string{filepath.Join(home, "x"), "nodest"}), "missing --dest")
	})

	writeManifestFixture(t, home, "machine-a", "photos", map[string]string{"a.jpg": "aaa"})
	srcDir := filepath.Join(home, "photos")
	destDir := filepath.Join(home, "archive")

	// Declining the confirmation leaves the folder in place.
	captureBoth(t, func() {
		withStdin(t, "n\n", func() {
			wantNil(t, runArchive([]string{srcDir, "--dest", destDir}), "declined")
		})
	})
	if _, err := os.Stat(srcDir); err != nil {
		t.Fatalf("declined archive must not move the folder: %v", err)
	}

	// Accepting moves it into a timestamped folder under dest.
	captureBoth(t, func() {
		withStdin(t, "y\n", func() {
			wantNil(t, runArchive([]string{srcDir, "--dest", destDir}), "accepted")
		})
	})
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source should be gone after archiving, stat err = %v", err)
	}
	moved, _ := filepath.Glob(filepath.Join(destDir, "*", "a.jpg"))
	if len(moved) != 1 {
		t.Errorf("expected a.jpg inside timestamped archive, glob found %v", moved)
	}
}

// =============================================================================
// runLookup / runCheckBackupStatus / runAnalyze / runCollect
// =============================================================================

func TestRunLookup(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runLookup(nil), "usage") })
	captureBoth(t, func() { wantNil(t, runLookup([]string{"x.jpg"}), "no manifests") })

	writeManifestFixtureSet(t, home)
	out := captureBoth(t, func() { wantNil(t, runLookup([]string{"shared.jpg"}), "lookup") })
	if !strings.Contains(out, "shared.jpg") {
		t.Errorf("expected match shown, got:\n%s", out)
	}
}

func TestRunCheckBackupStatus(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runCheckBackupStatus(nil), "usage") })
	captureBoth(t, func() {
		wantNil(t, runCheckBackupStatus([]string{filepath.Join(home, "x")}), "no manifests")
	})

	// Two machines sharing one file; checking machine-a's folder shows coverage.
	writeManifestFixtureSet(t, home)
	out := captureBoth(t, func() {
		wantNil(t, runCheckBackupStatus([]string{filepath.Join(home, "photos-a")}), "check")
	})
	if !strings.Contains(out, "BACKUP COVERAGE") {
		t.Errorf("expected coverage report, got:\n%s", out)
	}
	// shared.jpg exists on machine-b -> some backup coverage is reported.
	if !strings.Contains(out, "machine-b") {
		t.Errorf("expected machine-b listed as holding copies, got:\n%s", out)
	}
}

func TestRunAnalyze(t *testing.T) {
	home := setupHome(t)
	captureBoth(t, func() { wantFailed(t, runAnalyze(nil), "no manifests anywhere") })

	writeManifestFixtureSet(t, home)
	manifests, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	if len(manifests) != 2 {
		t.Fatalf("fixture manifests = %d, want 2", len(manifests))
	}

	out := captureBoth(t, func() { wantNil(t, runAnalyze(manifests), "analyze") })
	for _, w := range []string{"PHOTO DUPLICATE ANALYSIS", "MACHINE SUMMARIES", "DUPLICATE GROUPS"} {
		if !strings.Contains(out, w) {
			t.Errorf("analyze output missing %q\n---\n%s", w, out)
		}
	}
}

func TestRunCollectConfig(t *testing.T) {
	setupHome(t)

	// Bad --add format.
	captureBoth(t, func() { wantFailed(t, runCollect([]string{"--add", "noequals"}), "bad add") })

	// --list with no machines configured.
	out := captureBoth(t, func() { wantNil(t, runCollect([]string{"--list"}), "empty list") })
	if !strings.Contains(out, "No machines configured") {
		t.Errorf("expected empty-config message, got:\n%s", out)
	}

	// Register a machine, then list it.
	out = captureBoth(t, func() {
		wantNil(t, runCollect([]string{"--add", "nas=admin@nas.local"}), "add")
	})
	if !strings.Contains(out, "Registered") {
		t.Errorf("expected registration confirmation, got:\n%s", out)
	}
	cfg := loadMachinesConfig()
	if cfg["nas"] != "admin@nas.local" {
		t.Errorf("config after --add = %v, want nas registered", cfg)
	}
	out = captureBoth(t, func() { wantNil(t, runCollect([]string{"--list"}), "list") })
	if !strings.Contains(out, "nas") {
		t.Errorf("expected nas in list, got:\n%s", out)
	}
}
