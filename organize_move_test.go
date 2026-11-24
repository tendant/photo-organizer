package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailureCause(t *testing.T) {
	for want, err := range map[string]error{
		"permission": &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrPermission},
		"exists":     errors.New("/lib/x.mp4 appeared during the run; not overwriting"),
		"changed":    errors.New("/src/x.mp4 changed since the plan; not moving it"),
		"other":      errors.New("input/output error"),
	} {
		if got := failureCause(err); got != want {
			t.Errorf("failureCause(%v) = %q, want %q", err, got, want)
		}
	}
	if got := failureCause(fmt.Errorf("wrapped: %w", fs.ErrPermission)); got != "permission" {
		t.Errorf("wrapped permission error classed as %q", got)
	}
}

// A --move preview must notice a source folder we cannot rename out of, and a copy-only preview
// must report how much could have been renamed instead.
func TestMarkMovesUnwritableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	open := filepath.Join(root, "open")
	lib := filepath.Join(root, "lib")
	for _, d := range []string{locked, open, lib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(locked, "a.mp4"), "aaaa")
	writeFile(t, filepath.Join(open, "b.mp4"), "bbbb")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	p := &organizePlan{copies: []plannedCopy{
		{src: filepath.Join(locked, "a.mp4"), dst: filepath.Join(lib, "a.mp4"), size: 4},
		{src: filepath.Join(open, "b.mp4"), dst: filepath.Join(lib, "b.mp4"), size: 4},
	}}
	p.markMoves(lib, true)
	if got := p.unwritableCount(); got != 1 || p.unwritable[locked] != 1 {
		t.Fatalf("want exactly the locked folder flagged, got %v", p.unwritable)
	}
	if p.moveBytes != 8 || p.movableBytes != 8 {
		t.Errorf("moveBytes=%d movableBytes=%d, want 8/8", p.moveBytes, p.movableBytes)
	}
	if err := finishApply(p, lib, false); err == nil {
		t.Error("apply must refuse while a source folder is not writable")
	}
	if _, err := os.Stat(filepath.Join(open, "b.mp4")); err != nil {
		t.Error("a refused apply must not have moved anything")
	}

	p.markMoves(lib, false) // no --move: nothing to flag, but the saving is reported
	if p.unwritableCount() != 0 || p.moveBytes != 0 || p.movableBytes != 8 {
		t.Errorf("copy mode: unwritable=%d moveBytes=%d movableBytes=%d", p.unwritableCount(), p.moveBytes, p.movableBytes)
	}
}

// A plan saved with --move keeps the flag, so applying it later does not silently copy.
func TestSavedPlanRemembersMove(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	src := filepath.Join(root, "src")
	planPath := filepath.Join(root, "plans", "p.json")
	os.MkdirAll(filepath.Dir(planPath), 0o755)
	for _, move := range []bool{true, false} {
		p := &organizePlan{move: move, copies: []plannedCopy{{src: filepath.Join(src, "a.mp4"), dst: filepath.Join(lib, "a.mp4"), size: 1}}}
		if err := savePlanFile(planPath, []string{src}, lib, p); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(planPath)
		var pf planFile
		if err := json.Unmarshal(data, &pf); err != nil {
			t.Fatal(err)
		}
		if pf.Move != move {
			t.Errorf("saved plan Move = %v, want %v", pf.Move, move)
		}
		if has := strings.Contains(string(data), `"move"`); has != move {
			t.Errorf("move=%v but \"move\" key present=%v", move, has)
		}
	}
}

func TestPlanApplyKeepsMoveFromPreview(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "lib")
	planPath := filepath.Join(home, "plan.json")
	writeFile(t, filepath.Join(src, "a", "VID_20240305_080000.mp4"), "clip-one")
	if out, code := runCLIWithHome(t, home, "organize", src, "--type", "video", "--dest", lib, "--layout", "folder", "--move", "--save-plan", planPath, "--no-progress"); code != 0 {
		t.Fatalf("save-plan exit %d\n%s", code, out)
	}
	// Note: no --move here. The plan remembers it.
	out, code := runCLIWithHome(t, home, "organize", "--plan", planPath, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("apply exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "Using --move from the saved plan") {
		t.Errorf("apply did not say it is using the plan's --move:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(src, "a", "VID_20240305_080000.mp4")); !os.IsNotExist(err) {
		t.Errorf("source still exists: the plan was copied, not moved (err=%v)", err)
	}
	if n := len(listFiles(t, lib)); n != 1 {
		t.Errorf("want 1 file in the library, got %v", listFiles(t, lib))
	}
}
