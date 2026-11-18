package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestPathCompareFollowsTheWalkOrder(t *testing.T) {
	// plain string order would put "a b/x" before "a/x"; the walk visits a/x first
	if pathCompare("a/x", "a b/x") >= 0 {
		t.Error("a/x must come before a b/x")
	}
	if pathCompare("a/x", "a.jpg") >= 0 {
		t.Error("a/x must come before a.jpg (folder a sorts before file a.jpg)")
	}
	if pathCompare("a/b", "a/b") != 0 || pathCompare("a", "a/b") >= 0 {
		t.Error("equal and prefix cases")
	}
}

func manifestRels(t *testing.T, home string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	if len(files) != 1 {
		t.Fatalf("want one manifest, got %v", files)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, r := range recs[1:] {
		rels = append(rels, r[1])
	}
	return rels
}

func TestScanWritesFolderOrderAndRescanKeepsEverything(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "photos")
	names := []string{"a/x.jpg", "a b/y.jpg", "a.jpg", "a-b/z.jpg", "b/c/d.jpg", "b/c.jpg", "B.jpg", "_u.jpg"}
	for _, n := range names {
		writeFile(t, filepath.Join(src, n), "c-"+n)
	}
	for i := 0; i < 2; i++ { // the second scan reads the first one's manifest through the merge
		if out, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-ignore-file"); code != 0 {
			t.Fatalf("scan %d exit %d\n%s", i, code, out)
		}
		rels := manifestRels(t, home)
		if len(rels) != len(names) {
			t.Fatalf("scan %d: %d rows, want %d: %v", i, len(rels), len(names), rels)
		}
		if !sort.SliceIsSorted(rels, func(i, j int) bool { return pathCompare(rels[i], rels[j]) < 0 }) {
			t.Errorf("rows not in folder order: %v", rels)
		}
	}
}

func TestScanConvertsAStringSortedManifestWithoutLosingRows(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "photos")
	names := []string{"a/x.jpg", "a b/y.jpg", "a.jpg", "z.jpg"}
	for _, n := range names {
		writeFile(t, filepath.Join(src, n), "c-"+n)
	}
	if _, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-ignore-file"); code != 0 {
		t.Fatal("first scan failed")
	}
	files, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	b, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	data := lines[1:]
	sort.Slice(data, func(i, j int) bool { // plain string order of the relative_path column
		return strings.SplitN(data[i], ",", 3)[1] < strings.SplitN(data[j], ",", 3)[1]
	})
	os.WriteFile(files[0], []byte(lines[0]+"\n"+strings.Join(data, "\n")+"\n"), 0o644)

	out, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-ignore-file")
	if code != 0 {
		t.Fatalf("rescan exit %d\n%s", code, out)
	}
	rels := manifestRels(t, home)
	if len(rels) != len(names) {
		t.Fatalf("rows lost converting: %v", rels)
	}
	if !sort.SliceIsSorted(rels, func(i, j int) bool { return pathCompare(rels[i], rels[j]) < 0 }) {
		t.Errorf("not converted to folder order: %v", rels)
	}
	if strings.Contains(out, "Skipping prune") {
		t.Errorf("an unordered manifest must not look like mass deletion:\n%s", out)
	}
}

func TestScanResumesFromAnInterruptedScansJournal(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "photos")
	writeFile(t, filepath.Join(src, "a.jpg"), "content-a")
	writeFile(t, filepath.Join(src, "b.jpg"), "content-b")
	if _, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-ignore-file"); code != 0 {
		t.Fatal("scan failed")
	}
	files, _ := filepath.Glob(filepath.Join(home, "manifests", "_Manifest", "*.csv"))
	os.Remove(files[0])
	// a journal a killed scan left behind: a.jpg already hashed (with a marker hash), b.jpg not reached
	hdr := strings.Join(manifestColumns, ",")
	row := "a.jpg,a.jpg,9,0.00,2024-01-01 00:00:00,2024:01:01 00:00:00,,,deadbeefdeadbeefdeadbeefdeadbeef,,.jpg,2024-01-01 00:00:00," + src + ",m"
	os.WriteFile(files[0]+".partial.1", []byte(hdr+"\n"+row+"\n"), 0o644)

	out, code := runCLIWithHome(t, home, "scan", src, "--no-report", "--no-ignore-file")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m := manifestText(t, home)
	if !strings.Contains(m, "deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Errorf("the journal's hash for a.jpg was not reused:\n%s", m)
	}
	if !strings.Contains(m, "b.jpg") {
		t.Errorf("b.jpg missing:\n%s", m)
	}
	if left, _ := filepath.Glob(files[0] + ".partial.*"); len(left) != 0 {
		t.Errorf("journals should be removed after a finished scan: %v", left)
	}
}

func TestScanReportListsOnlyTheLargestTypes(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "stuff")
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(src, "f"+string(rune('a'+i))+"."+"ext"+string(rune('a'+i))), strings.Repeat("x", 100+i))
	}
	out, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file", "--report")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "+5 more") {
		t.Errorf("want the 15 largest types plus '+5 more':\n%s", out)
	}
	if strings.Contains(out, ".exta ") || !strings.Contains(out, ".extt ") {
		t.Errorf("the smallest type must be rolled up and the largest listed:\n%s", out)
	}
	if !strings.Contains(out, "TOTAL") || !strings.Contains(out, "20") {
		t.Errorf("the total must still show:\n%s", out)
	}
}

func TestScanSkipsTheReportUnlessAsked(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "p")
	writeFile(t, filepath.Join(src, "a.jpg"), "a")
	out, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file")
	if code != 0 || strings.Contains(out, "File Type Summary") {
		t.Errorf("a plain scan must not print the report (exit %d):\n%s", code, out)
	}
	if _, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file", "--no-report"); code != 0 {
		t.Error("--no-report must still be accepted")
	}
}

func TestScanSummaryAndManifestsListShowManifestSize(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "p")
	writeFile(t, filepath.Join(src, "a.jpg"), "a")
	writeFile(t, filepath.Join(src, "b.jpg"), "b")
	out, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file")
	if code != 0 || !strings.Contains(out, "Manifest:          2 entries, ") {
		t.Fatalf("scan summary should give the manifest's entries and size (exit %d):\n%s", code, out)
	}
	list, _ := runCLIWithHome(t, home, "manifests")
	if !strings.Contains(list, "Size") || !strings.Contains(list, " B  ") {
		t.Errorf("manifests listing should have a Size column:\n%s", list)
	}
}

func TestScanSummaryCountsOnlyTheRowsLeftAfterPruning(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "p")
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg"} {
		writeFile(t, filepath.Join(src, n), n)
	}
	if _, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file"); code != 0 {
		t.Fatal("first scan failed")
	}
	if err := os.Remove(filepath.Join(src, "d.jpg")); err != nil {
		t.Fatal(err)
	}
	out, code := runCLIWithHome(t, home, "scan", src, "--no-ignore-file")
	if code != 0 || !strings.Contains(out, "Pruned (deleted):  1") || !strings.Contains(out, "Manifest:          3 entries, ") {
		t.Fatalf("after pruning one of four files the summary should show 3 entries (exit %d):\n%s", code, out)
	}
}
