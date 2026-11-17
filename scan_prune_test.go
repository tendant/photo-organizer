package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// manifestText returns the contents of every manifest the scan wrote under home.
func manifestText(t *testing.T, home string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	var sb strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
	}
	return sb.String()
}

func pruneFixture(t *testing.T) (home, src string) {
	home = t.TempDir()
	src = filepath.Join(home, "photos")
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg"} {
		writeFile(t, filepath.Join(src, n), "content-"+n)
	}
	writeFile(t, filepath.Join(src, "sub/e.jpg"), "content-e")
	if out, code := runCLIWithHome(t, home, "scan", src, "--no-report"); code != 0 {
		t.Fatalf("first scan exit %d\n%s", code, out)
	}
	return home, src
}

func TestScanPrunesDeletedFilesByDefault(t *testing.T) {
	home, src := pruneFixture(t)
	if !strings.Contains(manifestText(t, home), "a.jpg") {
		t.Fatal("first scan should record a.jpg")
	}
	os.Remove(filepath.Join(src, "a.jpg"))
	if out, code := runCLIWithHome(t, home, "scan", src, "--no-report"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m := manifestText(t, home)
	if strings.Contains(m, "a.jpg") {
		t.Errorf("deleted file still in manifest after a default scan:\n%s", m)
	}
	if !strings.Contains(m, "b.jpg") || !strings.Contains(m, "e.jpg") {
		t.Errorf("surviving files must stay:\n%s", m)
	}
}

func TestScanNoPruneKeepsDeletedFiles(t *testing.T) {
	home, src := pruneFixture(t)
	os.Remove(filepath.Join(src, "a.jpg"))
	if out, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-prune"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(manifestText(t, home), "a.jpg") {
		t.Errorf("--no-prune must keep the row for a deleted file")
	}
}

func TestScanPruneFlagStillAccepted(t *testing.T) {
	home, src := pruneFixture(t)
	os.Remove(filepath.Join(src, "a.jpg"))
	if out, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--prune"); code != 0 {
		t.Fatalf("old --prune must still work, exit %d\n%s", code, out)
	}
	if strings.Contains(manifestText(t, home), "a.jpg") {
		t.Errorf("--prune should still prune")
	}
}

func TestScanDoesNotPruneWhatItCouldNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}
	home, src := pruneFixture(t)
	sub := filepath.Join(src, "sub")
	os.Chmod(sub, 0o000)
	defer os.Chmod(sub, 0o755)
	os.Remove(filepath.Join(src, "a.jpg")) // a genuine deletion in the same scan
	out, code := runCLIWithHome(t, home, "scan", src, "--no-report")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "Not pruning") {
		t.Errorf("expected a warning about unreadable paths:\n%s", out)
	}
	m := manifestText(t, home)
	if !strings.Contains(m, "e.jpg") {
		t.Errorf("file under the unreadable folder was pruned:\n%s", m)
	}
	if !strings.Contains(m, "a.jpg") {
		t.Errorf("nothing at all should be pruned when part of the scan is unreadable:\n%s", m)
	}
	// Once the folder is readable again the next scan prunes normally.
	os.Chmod(sub, 0o755)
	if out, code := runCLIWithHome(t, home, "scan", src, "--no-report"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(manifestText(t, home), "a.jpg") {
		t.Errorf("the deleted file should go on the next clean scan")
	}
}

func TestScanForcePruneOverridesTheHalfBrake(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		writeFile(t, filepath.Join(dir, n+".jpg"), "photo-"+n)
	}
	t.Setenv("HOME", t.TempDir())
	if err := runScan([]string{dir, "--no-report"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		os.Remove(filepath.Join(dir, n+".jpg"))
	}
	count := func() int {
		m, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest", "*.csv"))
		total := 0
		for _, f := range m {
			total += len(readCSV(t, f)) - 1
		}
		return total
	}
	runScan([]string{dir, "--no-report"})
	if got := count(); got != 4 {
		t.Fatalf("brake should have kept all 4 rows, got %d", got)
	}
	if err := runScan([]string{dir, "--no-report", "--force-prune"}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("--force-prune should leave 1 row, got %d", got)
	}
}

// ---- partial rescans -----------------------------------------------------------------

func manifestRows(t *testing.T, home string) (files []string, rels map[string]bool) {
	t.Helper()
	files, _ = filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	rels = map[string]bool{}
	for _, f := range files {
		rows := readCSV(t, f)
		for _, r := range rows[1:] {
			rels[r[1]] = true
		}
	}
	return
}

func TestScanOfASubfolderUpdatesTheParentManifestOnly(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "root")
	writeFile(t, filepath.Join(root, "a/one.jpg"), "one")
	writeFile(t, filepath.Join(root, "a/two.jpg"), "two")
	writeFile(t, filepath.Join(root, "a/three.jpg"), "three")
	writeFile(t, filepath.Join(root, "b/keep.jpg"), "keep")
	if out, code := runCLIWithHome(t, home, "scan", root, "--no-report"); code != 0 {
		t.Fatalf("full scan: exit %d\n%s", code, out)
	}
	// change things inside a/, and (without a rescan of b/) delete in b/
	os.Remove(filepath.Join(root, "a/one.jpg"))
	writeFile(t, filepath.Join(root, "a/new.jpg"), "brand new")
	os.Remove(filepath.Join(root, "b/keep.jpg"))

	out, code := runCLIWithHome(t, home, "scan", filepath.Join(root, "a"), "--no-report")
	if code != 0 || !strings.Contains(out, "Part of:") {
		t.Fatalf("partial scan: exit %d\n%s", code, out)
	}
	files, rels := manifestRows(t, home)
	if len(files) != 1 {
		t.Fatalf("a subfolder scan must not create a second manifest: %v", files)
	}
	for _, want := range []string{"a/two.jpg", "a/three.jpg", "a/new.jpg", "b/keep.jpg"} {
		if !rels[want] {
			t.Errorf("manifest should still have %s: %v", want, rels)
		}
	}
	if rels["a/one.jpg"] {
		t.Error("a deleted file inside the scanned part should be pruned")
	}
	// b/keep.jpg was deleted but b/ was not rescanned: its row is left alone until it is
	rows := readCSV(t, files[0])
	if rows[1][12] != root {
		t.Errorf("scan_path must stay the parent root, got %q", rows[1][12])
	}
}

func TestScanSeparateKeepsTheOldBehaviour(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "root")
	writeFile(t, filepath.Join(root, "a/one.jpg"), "one")
	runCLIWithHome(t, home, "scan", root, "--no-report")
	if out, code := runCLIWithHome(t, home, "scan", filepath.Join(root, "a"), "--separate", "--no-report"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if files, _ := manifestRows(t, home); len(files) != 2 {
		t.Errorf("--separate should write its own manifest: %v", files)
	}
}

func TestPartialScanBrakeLooksAtItsOwnPartOnly(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "root")
	for i := 0; i < 10; i++ {
		writeFile(t, filepath.Join(root, "big", fmt.Sprintf("f%d.jpg", i)), fmt.Sprintf("big-%d", i))
	}
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(root, "small", fmt.Sprintf("s%d.jpg", i)), fmt.Sprintf("small-%d", i))
	}
	runCLIWithHome(t, home, "scan", root, "--no-report")
	for i := 0; i < 3; i++ { // empty the small part: 3 of its 3 rows, but only 3 of 13 overall
		os.Remove(filepath.Join(root, "small", fmt.Sprintf("s%d.jpg", i)))
	}
	out, _ := runCLIWithHome(t, home, "scan", filepath.Join(root, "small"), "--no-report")
	if !strings.Contains(out, "Skipping prune: would remove 3 of 3 entries") {
		t.Errorf("the brake should judge the part that was scanned:\n%s", out)
	}
}

func TestRescanRefreshesScanDateOfUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "pics")
	writeFile(t, filepath.Join(dir, "a.jpg"), "one")
	if out, code := runCLIWithHome(t, home, "scan", dir, "--no-report"); code != 0 {
		t.Fatalf("scan: exit %d\n%s", code, out)
	}
	mf, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*pics.csv"))
	if len(mf) != 1 {
		t.Fatalf("manifest not found: %v", mf)
	}
	// Age the row as if it were scanned long ago.
	data, _ := os.ReadFile(mf[0])
	aged := regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,`+regexp.QuoteMeta(dir)).ReplaceAllString(string(data), "2020-01-01 00:00:00,"+dir)
	if aged == string(data) {
		t.Fatalf("could not age the scan_date in:\n%s", data)
	}
	os.WriteFile(mf[0], []byte(aged), 0600)

	if out, code := runCLIWithHome(t, home, "scan", dir, "--no-report"); code != 0 {
		t.Fatalf("rescan: exit %d\n%s", code, out)
	}
	after, _ := os.ReadFile(mf[0])
	if strings.Contains(string(after), "2020-01-01") {
		t.Errorf("an unchanged file kept its old scan_date:\n%s", after)
	}
}
