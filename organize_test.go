package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// snapshotTree records every path with its content hash and mtime, so a test
// can prove a folder was not touched in any way.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			snap[rel+"/"] = "dir"
			return nil
		}
		data, _ := os.ReadFile(p)
		info, _ := d.Info()
		sum := sha256.Sum256(data)
		snap[rel] = string(sum[:]) + info.ModTime().String()
		return nil
	})
	return snap
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if !strings.HasPrefix(rel, organizeLogDir) {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizeNeverTouchesSources(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "scattered")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "trip/IMG_20240315_101010.jpg"), "one")
	writeFile(t, filepath.Join(src, "misc/IMG_20230102_090000.jpg"), "two")
	writeFile(t, filepath.Join(src, "misc/IMG_20230102_090000.xmp"), "<x/>")
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "trip/IMG_20240315_101010.jpg"), old, old)
	before := snapshotTree(t, src)

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	after := snapshotTree(t, src)
	if len(before) != len(after) {
		t.Fatalf("source tree changed: %d entries before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("source entry %s changed", k)
		}
	}
}

func TestOrganizeDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "one")

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Dry run") {
		t.Errorf("expected dry-run notice, got:\n%s", out)
	}
	if _, err := os.Stat(lib); err == nil {
		t.Errorf("dry run created the library folder")
	}
}

func TestOrganizeLaysOutByDateKeepsSidecarsTogether(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "a/IMG_20240315_101010.jpg"), "jpg-bytes")
	writeFile(t, filepath.Join(src, "a/IMG_20240315_101010.xmp"), "sidecar")
	writeFile(t, filepath.Join(src, "a/lonely.xmp"), "orphan")

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := listFiles(t, lib)
	want := []string{
		"2024/2024-03-15/IMG_20240315_101010.jpg",
		"2024/2024-03-15/IMG_20240315_101010.xmp",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("library = %v, want %v (orphan sidecar must be ignored)", got, want)
	}
}

func TestOrganizeMonthLayout(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "x")
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--layout", "month", "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 1 || got[0] != "2024/2024-03/IMG_20240315_101010.jpg" {
		t.Errorf("library = %v", got)
	}
}

func TestOrganizeSkipsDuplicatesAcrossSourcesAndRuns(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "same")
	writeFile(t, filepath.Join(s2, "copy of it.jpg"), "same") // identical bytes, other name
	writeFile(t, filepath.Join(s2, "IMG_20240401_101010.jpg"), "different")

	if out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 2 {
		t.Fatalf("expected 2 files after dedupe, got %v", got)
	}

	// Second run, plus a new file: only the new one is copied.
	writeFile(t, filepath.Join(s1, "IMG_20250101_000000.jpg"), "new")
	out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 3 {
		t.Errorf("expected 3 files after rerun, got %v", got)
	}
}

func TestOrganizeNameCollisionGetsSuffixNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	// Same name, same date, different content.
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "first")
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.jpg"), "second")

	if out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := listFiles(t, lib)
	want := []string{"2024/2024-03-15/IMG_20240315_101010.jpg", "2024/2024-03-15/IMG_20240315_101010_1.jpg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("library = %v, want %v", got, want)
	}
	a, _ := os.ReadFile(filepath.Join(lib, want[0]))
	b, _ := os.ReadFile(filepath.Join(lib, want[1]))
	if string(a) != "first" || string(b) != "second" {
		t.Errorf("contents = %q, %q", a, b)
	}
}

// The scan hash only samples the first and last 32KB. Two different photos that
// agree there must both be kept.
func TestOrganizeKeepsNearIdenticalFilesThatDiffer(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	edge := bytes.Repeat([]byte("e"), sampleSize)
	mk := func(mid string) string {
		return string(edge) + strings.Repeat(mid, 1000) + string(edge)
	}
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), mk("a"))
	writeFile(t, filepath.Join(src, "IMG_20240315_101011.jpg"), mk("b"))

	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 2 {
		t.Errorf("both distinct files must be kept, got %v", got)
	}
}

func TestOrganizeRejectsOverlappingLibrary(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "x")
	for _, lib := range []string{filepath.Join(src, "Library"), home} {
		out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
		if code == 0 || !strings.Contains(out, "overlap") {
			t.Errorf("dest %s: expected overlap error, code=%d:\n%s", lib, code, out)
		}
	}
	if got := listFiles(t, src); len(got) != 1 {
		t.Errorf("source modified: %v", got)
	}
}

func TestCaptureFolderAndPlausibleDate(t *testing.T) {
	d := time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	if got := captureFolder(d, true, false); got != filepath.Join("2024", "2024-03-05") {
		t.Errorf("day layout = %s", got)
	}
	if got := captureFolder(d, false, false); got != undatedFolder {
		t.Errorf("undated = %s", got)
	}
	if plausibleDate(time.Unix(0, 0)) || plausibleDate(time.Now().AddDate(5, 0, 0)) || !plausibleDate(d) {
		t.Errorf("plausibleDate misjudged")
	}
}

func TestOrganizeIgnoresMacAppleDoubleFiles(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "real")
	writeFile(t, filepath.Join(src, "._IMG_20240315_101010.jpg"), "apple-double")
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 1 || strings.HasPrefix(filepath.Base(got[0]), "._") {
		t.Errorf("library = %v, want only the real photo", got)
	}
}

// Two different photos can have byte-identical sidecars; each must keep its own.
func TestOrganizeIdenticalSidecarsStayWithTheirOwnPhoto(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "photoA")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.xmp"), "<x/>")
	writeFile(t, filepath.Join(src, "IMG_20240316_101010.jpg"), "photoB")
	writeFile(t, filepath.Join(src, "IMG_20240316_101010.xmp"), "<x/>")

	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := strings.Join(listFiles(t, lib), ",")
	for _, want := range []string{
		"2024/2024-03-15/IMG_20240315_101010.xmp",
		"2024/2024-03-16/IMG_20240316_101010.xmp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestOrganizeSidecarsFollowRenamedPhotoAndDottedNames(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "first")
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.jpg"), "second") // collides -> _1
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.xmp"), "<second/>")
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.jpg.xmp"), "<dotted/>")

	if out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := strings.Join(listFiles(t, lib), ",")
	for _, want := range []string{
		"2024-03-15/IMG_20240315_101010_1.jpg",
		"2024-03-15/IMG_20240315_101010_1.xmp",     // follows the renamed photo
		"2024-03-15/IMG_20240315_101010_1.jpg.xmp", // dotted style
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestOrganizeSidecarOfDuplicatePhotoLandsBesideLibraryCopy(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "same")
	if out, code := runCLIWithHome(t, home, "organize", s1, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// Same photo shows up elsewhere under another name, with an edit sidecar.
	writeFile(t, filepath.Join(s2, "renamed.jpg"), "same")
	writeFile(t, filepath.Join(s2, "renamed.xmp"), "<edit/>")
	if out, code := runCLIWithHome(t, home, "organize", s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := strings.Join(listFiles(t, lib), ",")
	if !strings.Contains(got, "2024-03-15/IMG_20240315_101010.xmp") || strings.Contains(got, "renamed") {
		t.Errorf("sidecar should sit beside the existing library photo, got %s", got)
	}
}

func TestOrganizeConflictingSidecarIsReportedNotOverwritten(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "same")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.xmp"), "<v1/>")
	runCLIWithHome(t, home, "organize", s1, "--dest", lib, "--apply", "--no-progress")
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.jpg"), "same")
	writeFile(t, filepath.Join(s2, "IMG_20240315_101010.xmp"), "<v2/>")
	out, code := runCLIWithHome(t, home, "organize", s2, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "differ") {
		t.Errorf("conflict should be reported:\n%s", out)
	}
	data, _ := os.ReadFile(filepath.Join(lib, "2024/2024-03-15/IMG_20240315_101010.xmp"))
	if string(data) != "<v1/>" {
		t.Errorf("existing sidecar overwritten: %q", data)
	}
}

func TestOrganizeSkipsSystemAndThumbnailFolders(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "real")
	writeFile(t, filepath.Join(src, "@eaDir/IMG_20240315_101010.jpg/SYNOPHOTO_THUMB_M.jpg"), "thumb")
	writeFile(t, filepath.Join(src, ".thumbnails/1234.jpg"), "thumb2")
	writeFile(t, filepath.Join(src, "#recycle/old.jpg"), "trash")
	writeFile(t, filepath.Join(src, "cat.lrdata/preview.jpg"), "preview")
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 1 {
		t.Errorf("only the real photo should be copied, got %v", got)
	}
	if !strings.Contains(out, "@eaDir") {
		t.Errorf("skipped folders should be reported:\n%s", out)
	}
}

func TestOrganizeReportsFilesItDoesNotCopy(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "real")
	writeFile(t, filepath.Join(src, "notes.txt"), "n")
	writeFile(t, filepath.Join(src, "doc.pdf"), "p")
	writeFile(t, filepath.Join(src, "doc2.pdf"), "p2")
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"NOT photos", ".pdf ×2", ".txt ×1", "keep them before you retire"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestOrganizeRecognisesMoreCameraFormats(t *testing.T) {
	for _, ext := range []string{".braw", ".mxf", ".pef", ".srw", ".3fr", ".gpr", ".psd", ".jxl", ".m2ts", ".thm"} {
		if !isMediaFile(ext) {
			t.Errorf("%s should be recognised", ext)
		}
	}
}

func TestOrganizeSkipsEmptyFiles(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "real")
	writeFile(t, filepath.Join(src, "empty1.jpg"), "")
	writeFile(t, filepath.Join(src, "empty2.jpg"), "")
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 1 {
		t.Errorf("empty files must not be copied, got %v", got)
	}
	if !strings.Contains(out, "2 empty") {
		t.Errorf("empty files should be reported:\n%s", out)
	}
}

func TestOrganizeRejectsLibraryHiddenBehindSymlink(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "x")
	link := filepath.Join(home, "shortcut")
	if err := os.Symlink(src, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", filepath.Join(link, "Library"), "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "overlap") {
		t.Errorf("expected overlap error, code=%d:\n%s", code, out)
	}
	if got := listFiles(t, src); len(got) != 1 {
		t.Errorf("source modified: %v", got)
	}
}

func TestOrganizeSpaceCheck(t *testing.T) {
	p := &organizePlan{copyBytes: 100, freeBytes: 50, freeKnown: true}
	if !p.lacksSpace() {
		t.Errorf("100 bytes into 50 free must report lack of space")
	}
	p.freeBytes = 100
	if p.lacksSpace() {
		t.Errorf("exactly enough space must pass")
	}
	p.freeKnown = false
	p.freeBytes = 0
	if p.lacksSpace() {
		t.Errorf("unknown free space must not block")
	}
	if _, ok := freeSpaceFor(filepath.Join(t.TempDir(), "not", "created", "yet")); !ok && runtimeSupportsDiskFree() {
		t.Errorf("free space should resolve via the nearest existing parent")
	}
}

func runtimeSupportsDiskFree() bool {
	_, ok := diskFreeBytes(os.TempDir())
	return ok
}

func tmpFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), organizeTmpPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestSafeCopyCancelledLeavesNoTempFileAndNoDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	big := bytes.Repeat([]byte("x"), 4<<20)
	os.WriteFile(src, big, 0o644)
	dst := filepath.Join(dir, "lib", "2024", "a.jpg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := safeCopyIntoLibrary(ctx, plannedCopy{src: src, dst: dst, size: int64(len(big))}, nil)
	if err == nil {
		t.Fatalf("a cancelled copy must report an error")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Errorf("destination must not exist after a cancelled copy")
	}
	if left := tmpFilesUnder(t, filepath.Join(dir, "lib")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestRemoveStaleTempOnlyRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, organizeTmpPrefix+"old")
	fresh := filepath.Join(dir, organizeTmpPrefix+"fresh")
	os.WriteFile(old, []byte("a"), 0o644)
	os.WriteFile(fresh, []byte("b"), 0o644)
	past := time.Now().Add(-2 * staleTmpAge)
	os.Chtimes(old, past, past)

	if n := removeStaleTemp([]string{old, fresh}); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if _, err := os.Stat(old); err == nil {
		t.Errorf("old temp file should be gone")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a recent temp file may belong to a running copy and must stay")
	}
}

func TestOrganizeApplySweepsStaleTempFromLibrary(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "photo")
	stale := filepath.Join(lib, "2024", "2024-03-15", organizeTmpPrefix+"12345")
	writeFile(t, stale, "half a photo")
	past := time.Now().Add(-3 * time.Hour)
	os.Chtimes(stale, past, past)

	// A dry run reports it but must not delete it.
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--no-progress")
	if code != 0 || !strings.Contains(out, "leftover temp") {
		t.Fatalf("dry run should mention leftovers (code %d):\n%s", code, out)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("dry run must not delete anything")
	}

	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if left := tmpFilesUnder(t, lib); len(left) != 0 {
		t.Errorf("stale temp not removed: %v", left)
	}
}

// Send a real SIGINT to a running organize and check it stops cleanly.
func TestOrganizeCtrlCMidCopyLeavesNoTempFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals differ on windows")
	}
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	os.MkdirAll(src, 0o755)
	big := filepath.Join(src, "IMG_20240315_101010.mp4")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(1 << 30) // 1 GiB sparse: long enough to interrupt mid-copy
	f.Close()
	writeFile(t, filepath.Join(src, "IMG_20240316_101010.jpg"), "second")

	cmd := exec.Command(os.Args[0], "-test.run=TestCLIMainHelper", "--",
		"organize", src, "--dest", lib, "--apply", "--no-progress")
	cmd.Env = append(os.Environ(), "HOME="+home, "PHOTO_ORGANIZER_TEST_MAIN=1",
		"PHOTO_ORGANIZER_CHECKPOINT_DIR="+filepath.Join(home, "checkpoints"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until the copy is under way, then interrupt.
	deadline := time.Now().Add(20 * time.Second)
	for len(tmpFilesUnder(t, lib)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	waitErr := cmd.Wait()

	if left := tmpFilesUnder(t, lib); len(left) != 0 {
		t.Errorf("temp files left after Ctrl-C: %v\noutput:\n%s", left, out.String())
	}
	if waitErr == nil {
		t.Logf("copy finished before the signal landed; nothing to assert about interruption")
		return
	}
	if !strings.Contains(out.String(), "Interrupted") {
		t.Errorf("expected an 'Interrupted' message, got:\n%s", out.String())
	}
	// A rerun picks up where it stopped and finishes the job.
	if o, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, o)
	}
	if got := listFiles(t, lib); len(got) != 2 {
		t.Errorf("after rerun library = %v, want both files", got)
	}
}

// ---- duplicate-check speed: cache, verify modes, parallelism -----------------

func TestOrganizeWritesAndReusesLibraryHashCache(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("IMG_20240315_10101%d.jpg", i)), strings.Repeat("p", 100+i))
	}
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	cache := filepath.Join(lib, organizeLogDir, cacheName)
	data, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if n := strings.Count(string(data), "\n"); n != 6 { // header + 5 photos
		t.Errorf("cache has %d lines, want 6:\n%s", n, data)
	}

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "5 from cache") {
		t.Errorf("second run should reuse the cache for all 5 files:\n%s", out)
	}
}

func TestOrganizeCacheIgnoredWhenLibraryFileChanged(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "original-content")
	if out, code := runCLIWithHome(t, home, "organize", s1, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// The library file is edited in place: same size, new modified time.
	libFile := filepath.Join(lib, "2024/2024-03-15/IMG_20240315_101010.jpg")
	os.WriteFile(libFile, []byte("edited!-content!!"[:len("original-content")]), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(libFile, future, future)

	// The original content turns up again elsewhere: it is no longer in the library.
	writeFile(t, filepath.Join(s2, "again.jpg"), "original-content")
	out, code := runCLIWithHome(t, home, "organize", s2, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 2 {
		t.Errorf("stale cache entry hid a change; library = %v", got)
	}
}

func TestOrganizeSurvivesCorruptCache(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "photo")
	writeFile(t, filepath.Join(lib, organizeLogDir, cacheName), "garbage,\"unterminated\n\x00\x01")
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("a damaged cache must not fail the run (exit %d):\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 1 {
		t.Errorf("library = %v", got)
	}
}

// writeBigPair writes two sparse files of the given size that match everywhere
// except one byte at diffAt.
func writeBigPair(t *testing.T, a, b string, size, diffAt int64) {
	t.Helper()
	for _, p := range []string{a, b} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(size)
		f.Close()
	}
	f, _ := os.OpenFile(b, os.O_WRONLY, 0)
	f.WriteAt([]byte{0xFF}, diffAt)
	f.Close()
}

// offsetOutsideSamples returns an offset in the middle that no sample window covers.
func offsetOutsideSamples(size int64) int64 {
	for off := size / 3; ; off += 4096 {
		covered := false
		for i := int64(0); i < sampleChunks; i++ {
			start := i * (size - sampleChunk) / (sampleChunks - 1)
			if off >= start && off < start+sampleChunk {
				covered = true
			}
		}
		if !covered {
			return off
		}
	}
}

func TestOrganizeAutoComparesEveryByteOfNormalSizedFiles(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	size := int64(8 << 20) // well under the 64 MB limit
	writeBigPair(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), filepath.Join(s2, "IMG_20240315_101010.jpg"), size, offsetOutsideSamples(size))
	if out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := listFiles(t, lib); len(got) != 2 {
		t.Errorf("a one-byte difference in an 8 MB file must keep both copies, got %v", got)
	}
}

func TestOrganizeVerifyModesOnHugeFiles(t *testing.T) {
	size := int64(80 << 20) // over the 64 MB auto limit
	off := offsetOutsideSamples(size)
	run := func(verify string) (files int, out string) {
		home := t.TempDir()
		s1 := filepath.Join(home, "s1")
		s2 := filepath.Join(home, "s2")
		lib := filepath.Join(home, "Library")
		writeBigPair(t, filepath.Join(s1, "IMG_20240315_101010.mp4"), filepath.Join(s2, "IMG_20240315_101010.mp4"), size, off)
		args := []string{"organize", s1, s2, "--dest", lib, "--apply", "--no-progress"}
		if verify != "" {
			args = append(args, "--verify", verify)
		}
		out, code := runCLIWithHome(t, home, args...)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		return len(listFiles(t, lib)), out
	}

	if n, _ := run("full"); n != 2 {
		t.Errorf("--verify full must tell the files apart, library has %d", n)
	}
	// The documented trade-off: sampling large files can miss a change between sample points.
	n, out := run("")
	if n != 1 || !strings.Contains(out, "confirmed by sampling") {
		t.Errorf("auto mode samples huge files and says so; library has %d files:\n%s", n, out)
	}
}

func TestOrganizeResultIsIdenticalWithOneOrManyJobs(t *testing.T) {
	build := func(jobs string) []string {
		home := t.TempDir()
		s1 := filepath.Join(home, "s1")
		s2 := filepath.Join(home, "s2")
		lib := filepath.Join(home, "Library")
		for i := 0; i < 60; i++ {
			day := 1 + i%9
			name := fmt.Sprintf("IMG_202403%02d_%06d.jpg", day, i%7) // many same-name collisions
			writeFile(t, filepath.Join(s1, fmt.Sprintf("d%d", i%5), name), fmt.Sprintf("photo-%d", i))
			if i%3 == 0 { // every third photo is duplicated in s2 under another name
				writeFile(t, filepath.Join(s2, fmt.Sprintf("copy-%d.jpg", i)), fmt.Sprintf("photo-%d", i))
			}
			writeFile(t, filepath.Join(s1, fmt.Sprintf("d%d", i%5), strings.TrimSuffix(name, ".jpg")+".xmp"), "<x/>")
		}
		if out, code := runCLIWithHome(t, home, "organize", s1, s2, "--dest", lib, "--apply", "--no-progress", "--jobs", jobs); code != 0 {
			t.Fatalf("jobs=%s exit %d:\n%s", jobs, code, out)
		}
		// Compare names and contents.
		var rows []string
		for _, rel := range listFiles(t, lib) {
			data, _ := os.ReadFile(filepath.Join(lib, rel))
			rows = append(rows, rel+"="+string(data))
		}
		return rows
	}
	one, many := build("1"), build("8")
	if strings.Join(one, "\n") != strings.Join(many, "\n") {
		t.Errorf("--jobs changes the result:\n1 job:  %d files\n8 jobs: %d files", len(one), len(many))
	}
	if len(one) < 20 {
		t.Errorf("fixture produced too few files: %d", len(one))
	}
}

func TestOrganizeRejectsBadJobsAndVerify(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "x")
	for _, bad := range [][]string{{"--jobs", "0"}, {"--jobs", "many"}, {"--verify", "maybe"}} {
		args := append([]string{"organize", src, "--dest", filepath.Join(home, "L")}, bad...)
		if out, code := runCLIWithHome(t, home, args...); code == 0 {
			t.Errorf("%v should be rejected:\n%s", bad, out)
		}
	}
}

// An apply that copies nothing still learns library digests; they must be kept.
func TestOrganizeNoOpApplyStillSavesLearnedDigests(t *testing.T) {
	home := t.TempDir()
	s1 := filepath.Join(home, "s1")
	s2 := filepath.Join(home, "s2")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(s1, "IMG_20240315_101010.jpg"), "same-bytes")
	writeFile(t, filepath.Join(s2, "other-name.jpg"), "same-bytes")
	if out, code := runCLIWithHome(t, home, "organize", s1, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	before, _ := os.ReadFile(filepath.Join(lib, organizeLogDir, cacheName))

	// s2 only holds duplicates: nothing is copied, but the library file gets compared.
	if out, code := runCLIWithHome(t, home, "organize", s2, "--dest", lib, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	after, _ := os.ReadFile(filepath.Join(lib, organizeLogDir, cacheName))
	rows := readCSV(t, filepath.Join(lib, organizeLogDir, cacheName))
	if len(rows) != 2 || rows[1][4] == "" {
		t.Errorf("library file's full digest should now be cached; before:\n%s\nafter:\n%s", before, after)
	}
}

func TestOrganizeVideoTimeZoneMovesEveningClipsOnly(t *testing.T) {
	if _, err := time.LoadLocation("America/New_York"); err != nil {
		t.Skip("no tz database")
	}
	home := t.TempDir()
	src := filepath.Join(home, "src")
	vid := filepath.Join(src, "clip.mov")
	os.MkdirAll(src, 0o755)
	os.WriteFile(vid, mp4WithCreation(time.Date(2023, 8, 20, 2, 30, 0, 0, time.UTC)), 0o644)
	writeFile(t, filepath.Join(src, "IMG_20230820_023000.jpg"), "photo") // dated by filename: never shifted

	asWritten := filepath.Join(home, "utc")
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", asWritten, "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, _ := runCLIWithHome(t, home, "organize", src, "--dest", filepath.Join(home, "preview"), "--no-progress"); strings.Contains(out, "Video times shown in") {
		t.Errorf("no zone given: the summary should not mention one:\n%s", out)
	}
	if got := strings.Join(listFiles(t, asWritten), ","); !strings.Contains(got, "2023/2023-08-20/clip.mov") {
		t.Errorf("default should keep UTC date, got %s", got)
	}

	shifted := filepath.Join(home, "ny")
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", filepath.Join(home, "ny-preview"), "--video-tz", "America/New_York", "--no-progress"); code != 0 ||
		!strings.Contains(out, "Video times shown in America/New_York: 1 filed under a different day") {
		t.Errorf("summary should report the zone and one shifted video (exit %d):\n%s", code, out)
	}
	if out, code := runCLIWithHome(t, home, "organize", src, "--dest", shifted, "--video-tz", "America/New_York", "--apply", "--no-progress"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := strings.Join(listFiles(t, shifted), ",")
	if !strings.Contains(got, "2023/2023-08-19/clip.mov") || !strings.Contains(got, "2023/2023-08-20/IMG_20230820_023000.jpg") {
		t.Errorf("video should move to the 19th and the photo stay on the 20th, got %s", got)
	}

	if _, code := runCLIWithHome(t, home, "organize", src, "--dest", filepath.Join(home, "x"), "--video-tz", "Mars/Olympus", "--no-progress"); code == 0 {
		t.Errorf("unknown zone should be rejected")
	}
}

func TestParseOrganizeArgsVideoTZ(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tz database unavailable")
	}
	cases := []struct {
		name string
		args []string
		want *time.Location
	}{
		{"default is as written", nil, nil},
		{"utc", []string{"--video-tz", "utc"}, nil},
		{"UTC is case-insensitive", []string{"--video-tz", "UTC"}, nil},
		{"local", []string{"--video-tz", "local"}, time.Local},
		{"Local is case-insensitive", []string{"--video-tz", "Local"}, time.Local},
		{"named zone", []string{"--video-tz", "America/New_York"}, ny},
		{"later flag wins", []string{"--video-tz", "local", "--video-tz", "utc"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, err := parseOrganizeArgs(append([]string{"src", "--dest", "lib"}, c.args...))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.want == nil {
				if opts.videoTZ != nil {
					t.Errorf("videoTZ = %v, want nil", opts.videoTZ)
				}
			} else if opts.videoTZ == nil || opts.videoTZ.String() != c.want.String() {
				t.Errorf("videoTZ = %v, want %v", opts.videoTZ, c.want)
			}
			if len(opts.sources) != 1 || opts.sources[0] != "src" {
				t.Errorf("the zone value must not be taken as a source folder: %v", opts.sources)
			}
		})
	}

	for _, bad := range [][]string{
		{"--video-tz"},
		{"--video-tz", "Mars/Olympus"},
		{"--video-tz", ""},
	} {
		if _, err := parseOrganizeArgs(append([]string{"src"}, bad...)); err == nil {
			t.Errorf("args %q should be rejected", bad)
		}
	}
}

func TestOrganizeVideoTimeZoneRejectedWithPlan(t *testing.T) {
	home := t.TempDir()
	out, code := runCLIWithHome(t, home, "organize", "--plan", filepath.Join(home, "p.json"), "--video-tz", "local", "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "--video-tz has no effect with --plan") {
		t.Errorf("want a clear rejection, got exit %d:\n%s", code, out)
	}
}
