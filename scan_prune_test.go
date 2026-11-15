package main

import (
	"os"
	"path/filepath"
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
