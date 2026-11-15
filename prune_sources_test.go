package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// srcPruneFixture builds source/, library/, backup/ and fakes distinct devices.
type srcPruneFixture struct {
	src, lib, bak, hold string
}

func newSrcPruneFixture(t *testing.T) srcPruneFixture {
	t.Helper()
	root := t.TempDir()
	f := srcPruneFixture{
		src: filepath.Join(root, "src"), lib: filepath.Join(root, "lib"),
		bak: filepath.Join(root, "bak"), hold: filepath.Join(root, "hold"),
	}
	for _, d := range []string{f.src, f.lib, f.bak} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := deviceOf
	deviceOf = func(p string) (uint64, bool) {
		if pathWithin(p, filepath.Join(root, "bak")) {
			return 2, true
		}
		return 1, true
	}
	t.Cleanup(func() { deviceOf = old })
	return f
}

func (f srcPruneFixture) args(extra ...string) []string {
	return append([]string{f.src, "--library", f.lib, "--backup", f.bak, "--no-progress"}, extra...)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneMovesOnlyFilesVerifiedInLibraryAndBackup(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.src, "a/libonly.jpg"), "photo-libonly")
	writeFile(t, filepath.Join(f.src, "a/none.jpg"), "photo-none")
	writeFile(t, filepath.Join(f.lib, "2024/2024-01-01/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.lib, "2024/2024-01-01/libonly.jpg"), "photo-libonly")
	writeFile(t, filepath.Join(f.bak, "x/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.bak, "x/other.jpg"), "unrelated")

	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "a/both.jpg")) {
		t.Error("verified file should have left the source")
	}
	if !exists(filepath.Join(f.src, "a/libonly.jpg")) || !exists(filepath.Join(f.src, "a/none.jpg")) {
		t.Error("unverified files must stay")
	}
	moved := holdingPath(f.hold, f.src, filepath.Join(f.src, "a/both.jpg"))
	if b, _ := os.ReadFile(moved); string(b) != "photo-both" {
		t.Errorf("moved file missing or changed at %s", moved)
	}
	// library and backup copies untouched
	if !exists(filepath.Join(f.lib, "2024/2024-01-01/both.jpg")) || !exists(filepath.Join(f.bak, "x/both.jpg")) {
		t.Error("library/backup must not change")
	}
}

func TestPrunePreviewChangesNothing(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "same")
	before := snapshotTree(t, filepath.Dir(f.src))
	if err := runPruneSources(f.args()); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, filepath.Dir(f.src))
	if len(before) != len(after) {
		t.Fatalf("preview changed the tree: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed", k)
		}
	}
}

func TestPruneSameSizeDifferentBytesIsNotACopy(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "AAAAAAAA")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "AAAAAAAB") // same size, differs at the end
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "AAAAAAAA")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("a file whose library copy differs must stay")
	}
}

func TestPruneNeedsABackupCopyNotJustLibrary(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "other.jpg"), "different")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("file with no backup copy must stay")
	}
}

func TestPruneKeepsSidecarsAndOtherType(t *testing.T) {
	f := newSrcPruneFixture(t)
	for _, root := range []string{f.src, f.lib, f.bak} {
		writeFile(t, filepath.Join(root, "p.jpg"), "photo")
		writeFile(t, filepath.Join(root, "p.xmp"), "edits")
		writeFile(t, filepath.Join(root, "v.mp4"), "video")
	}
	if err := runPruneSources(f.args("--type", "photo", "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "p.jpg")) {
		t.Error("photo should be moved")
	}
	if !exists(filepath.Join(f.src, "p.xmp")) {
		t.Error("sidecar must stay")
	}
	if !exists(filepath.Join(f.src, "v.mp4")) {
		t.Error("video must stay in a --type photo run")
	}
}

func TestPruneRefusesBackupOnLibraryDevice(t *testing.T) {
	f := newSrcPruneFixture(t)
	deviceOf = func(string) (uint64, bool) { return 1, true }
	writeFile(t, filepath.Join(f.src, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "x")
	err := runPruneSources(f.args())
	if err == nil || !strings.Contains(err.Error(), "same device") {
		t.Fatalf("want same-device refusal, got %v", err)
	}
}

func TestPruneRefusesOverlappingFolders(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "x")
	// library inside the source: its files would count as their own copy
	insideLib := filepath.Join(f.src, "library")
	os.MkdirAll(insideLib, 0o755)
	writeFile(t, filepath.Join(insideLib, "a.jpg"), "x")
	err := runPruneSources([]string{f.src, "--library", insideLib, "--backup", f.bak, "--no-progress"})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("want overlap refusal, got %v", err)
	}
	// holding inside the library
	err = runPruneSources(f.args("--holding", filepath.Join(f.lib, "h"), "--apply"))
	if err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("want holding refusal, got %v", err)
	}
}

func TestPruneRefusesEmptyBackup(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err == nil {
		t.Fatal("an empty backup must stop the run")
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("source must be untouched")
	}
}

func TestPruneArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"--library", "l", "--backup", "b"}, "source"},
		{"no library", []string{"s", "--backup", "b"}, "--library"},
		{"no backup", []string{"s", "--library", "l"}, "--backup"},
		{"apply without holding", []string{"s", "--library", "l", "--backup", "b", "--apply"}, "--holding"},
		{"bad type", []string{"s", "--library", "l", "--backup", "b", "--type", "x"}, "unknown type"},
		{"bad option", []string{"s", "--library", "l", "--backup", "b", "--nope"}, "unknown option"},
	} {
		_, err := parsePruneArgs(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestPruneWritesReportAndKeepsExistingHoldingFiles(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "same")
	rep := filepath.Join(filepath.Dir(f.src), "report.csv")
	// an earlier file already sits at the holding destination: do not overwrite it
	dst := holdingPath(f.hold, f.src, filepath.Join(f.src, "a.jpg"))
	writeFile(t, dst, "older")
	err := runPruneSources(f.args("--report", rep, "--holding", f.hold, "--apply"))
	if err == nil {
		t.Fatal("a refused move should be reported as a failure")
	}
	if b, _ := os.ReadFile(dst); string(b) != "older" {
		t.Error("existing holding file was overwritten")
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("source must stay when the move is refused")
	}
	rows := readCSV(t, rep)
	if len(rows) != 2 || rows[1][2] != "safe" {
		t.Errorf("unexpected report: %v", rows)
	}
}
