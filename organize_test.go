package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
