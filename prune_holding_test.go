package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two sources on different (faked) filesystems, one --holding on each: every file must land in
// the holding folder of its own filesystem, in one run.
func TestPruneMultipleHoldingsOneRun(t *testing.T) {
	f := newSrcPruneFixture(t)
	root := filepath.Dir(f.src)
	srcB, holdB := filepath.Join(root, "srcB"), filepath.Join(root, "holdB")
	os.MkdirAll(srcB, 0o755)
	os.MkdirAll(holdB, 0o755) // a missing holding folder takes its parent's device; give the fake one of its own
	deviceOf = func(p string) (uint64, bool) {
		switch {
		case pathWithin(p, filepath.Join(root, "bak")):
			return 2, true
		case pathWithin(p, srcB) || pathWithin(p, holdB):
			return 3, true
		}
		return 1, true
	}
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(f.lib, "2024", name+".jpg"), "photo-"+name)
		writeFile(t, filepath.Join(f.bak, "x", name+".jpg"), "photo-"+name)
	}
	writeFile(t, filepath.Join(f.src, "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(srcB, "b.jpg"), "photo-b")

	args := []string{f.src, srcB, "--library", f.lib, "--backup", f.bak, "--no-progress", "--verify", "bytes"}
	if err := runPruneSources(append(args, "--holding", f.hold, "--holding", holdB, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(holdingPath(f.hold, f.src, filepath.Join(f.src, "a.jpg"))) {
		t.Error("a.jpg should be in the holding folder on its own filesystem")
	}
	if !exists(holdingPath(holdB, srcB, filepath.Join(srcB, "b.jpg"))) {
		t.Error("b.jpg should be in the holding folder on the other filesystem")
	}
	if exists(filepath.Join(f.src, "a.jpg")) || exists(filepath.Join(srcB, "b.jpg")) {
		t.Error("both sources should have been moved")
	}
}

// A source on a filesystem without a --holding must stop the whole apply before anything moves.
func TestPruneRefusesWhenNoHoldingOnASourcesFilesystem(t *testing.T) {
	f := newSrcPruneFixture(t)
	root := filepath.Dir(f.src)
	srcB := filepath.Join(root, "srcB")
	os.MkdirAll(srcB, 0o755)
	deviceOf = func(p string) (uint64, bool) {
		switch {
		case pathWithin(p, filepath.Join(root, "bak")):
			return 2, true
		case pathWithin(p, srcB):
			return 3, true
		}
		return 1, true
	}
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(f.lib, "2024", name+".jpg"), "photo-"+name)
		writeFile(t, filepath.Join(f.bak, "x", name+".jpg"), "photo-"+name)
	}
	writeFile(t, filepath.Join(f.src, "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(srcB, "b.jpg"), "photo-b")

	args := []string{f.src, srcB, "--library", f.lib, "--backup", f.bak, "--no-progress", "--verify", "bytes"}
	if err := runPruneSources(append(args, "--holding", f.hold, "--apply")); err == nil {
		t.Fatal("apply must refuse: srcB has no holding folder on its filesystem")
	}
	if !exists(filepath.Join(f.src, "a.jpg")) || !exists(filepath.Join(srcB, "b.jpg")) {
		t.Error("a refused apply must not move any file, not even the ones that could have moved")
	}
}

// Moving out of a folder we cannot write to fails per file; apply must refuse up front instead.
func TestPruneRefusesUnwritableSourceFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.lib, "2024", "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(f.bak, "x", "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(f.src, "locked", "a.jpg"), "photo-a")
	locked := filepath.Join(f.src, "locked")
	os.Chmod(locked, 0o555)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err == nil {
		t.Fatal("apply must refuse while the source folder is not writable")
	}
	if !exists(filepath.Join(locked, "a.jpg")) {
		t.Error("nothing may be moved by a refused apply")
	}
	if blocked := checkPruneMoves([]*pruneItem{{path: filepath.Join(locked, "a.jpg"), root: f.src, verdict: pruneSafe, size: 7}}, nil); len(blocked) != 1 || blocked[0].kind != "perm" {
		t.Errorf("preview without --holding should still report the permission problem, got %+v", blocked)
	}
}

// On a drive that is both the source and the only place a move can go, the holding folder
// sits inside the source. Its files are skipped, and a holding that contains a source is refused.
func TestPruneHoldingInsideSource(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.lib, "2024", "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(f.bak, "x", "a.jpg"), "photo-a")
	writeFile(t, filepath.Join(f.src, "a.jpg"), "photo-a")
	hold := filepath.Join(f.src, "_holding", "pruned")

	args := []string{f.src, "--library", f.lib, "--backup", f.bak, "--no-progress", "--verify", "bytes", "--holding", hold}
	if err := runPruneSources(append(args, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(holdingPath(hold, f.src, filepath.Join(f.src, "a.jpg"))) || exists(filepath.Join(f.src, "a.jpg")) {
		t.Fatal("a.jpg should have moved into the holding folder inside the source")
	}
	// A second run must not offer the moved file (now inside the source) again.
	if err := runPruneSources(append(args, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(holdingPath(hold, f.src, filepath.Join(f.src, "a.jpg"))) {
		t.Error("the holding folder's file must stay where it is")
	}
	// A holding folder that contains the source is still refused.
	err := runPruneSources([]string{f.src, "--library", f.lib, "--backup", f.bak, "--no-progress", "--holding", f.src, "--apply"})
	if err == nil || !strings.Contains(err.Error(), "contains the source") {
		t.Fatalf("want contains-the-source refusal, got %v", err)
	}
}
