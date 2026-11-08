package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planFixture(t *testing.T) (home, src, lib, plan string) {
	t.Helper()
	home = t.TempDir()
	src = filepath.Join(home, "src")
	lib = filepath.Join(home, "Library")
	plan = filepath.Join(home, "plan.json")
	writeFile(t, filepath.Join(src, "a/IMG_20240315_101010.jpg"), "one")
	writeFile(t, filepath.Join(src, "a/IMG_20240315_101010.xmp"), "<x/>")
	writeFile(t, filepath.Join(src, "b/IMG_20230102_090000.jpg"), "two")
	return
}

func TestSavePlanWritesOnlyThePlanThenApplyCopiesExactlyThat(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	before := snapshotTree(t, src)

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")
	if code != 0 || !strings.Contains(out, "Plan saved") {
		t.Fatalf("save-plan: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(lib); err == nil {
		t.Fatalf("saving a plan created the library")
	}

	// A file that appears after the preview is NOT part of the plan.
	writeFile(t, filepath.Join(src, "c/IMG_20220101_120000.jpg"), "late")
	before = snapshotTree(t, src)

	out, code = runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("apply plan: exit %d\n%s", code, out)
	}
	got := strings.Join(listFiles(t, lib), ",")
	want := "2023/2023-01-02/IMG_20230102_090000.jpg,2024/2024-03-15/IMG_20240315_101010.jpg,2024/2024-03-15/IMG_20240315_101010.xmp"
	if got != want {
		t.Errorf("library = %s\nwant      %s", got, want)
	}
	for k, v := range before {
		if snapshotTree(t, src)[k] != v {
			t.Errorf("source entry %s changed", k)
		}
	}

	// Replaying the same plan finds everything in place and copies nothing.
	out, code = runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code != 0 || !strings.Contains(out, "already in place") || !strings.Contains(out, "Will copy 0 files") {
		t.Errorf("replay: exit %d\n%s", code, out)
	}
}

func TestPlanWithoutApplyWritesNothing(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")
	out, code := runCLIWithHome(t, home, "organize", "--plan", plan, "--no-progress")
	if code != 0 || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(lib); err == nil {
		t.Errorf("plan preview created the library")
	}
}

func TestPlanRefusesWhenASourceChanged(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")

	writeFile(t, filepath.Join(src, "b/IMG_20230102_090000.jpg"), "two but edited")
	out, code := runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "source changed since the preview") {
		t.Fatalf("expected refusal, exit %d\n%s", code, out)
	}
	if _, err := os.Stat(lib); err == nil {
		t.Errorf("a refused plan still wrote to the library")
	}

	os.Remove(filepath.Join(src, "b/IMG_20230102_090000.jpg"))
	out, code = runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "source is gone") {
		t.Errorf("expected 'source is gone', exit %d\n%s", code, out)
	}
}

func TestPlanRefusesWhenTargetNameTakenByOtherContent(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")

	taken := filepath.Join(lib, "2023/2023-01-02/IMG_20230102_090000.jpg")
	writeFile(t, taken, "someone else's photo")
	out, code := runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "now taken by other content") {
		t.Fatalf("expected refusal, exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(taken); string(b) != "someone else's photo" {
		t.Errorf("existing library file was overwritten")
	}
	if len(listFiles(t, lib)) != 1 {
		t.Errorf("refused plan copied other files: %v", listFiles(t, lib))
	}
}

func TestPlanResumesAfterPartialApply(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")
	// Simulate an interrupted run that finished one file.
	writeFile(t, filepath.Join(lib, "2023/2023-01-02/IMG_20230102_090000.jpg"), "two")
	out, code := runCLIWithHome(t, home, "organize", "--plan", plan, "--apply", "--no-progress")
	if code != 0 || !strings.Contains(out, "1 files from the plan are already in place") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if n := len(listFiles(t, lib)); n != 3 {
		t.Errorf("want 3 files after resume, got %d", n)
	}
}

func TestPlanRejectsTamperedTargetsAndBadUsage(t *testing.T) {
	home, src, lib, plan := planFixture(t)
	runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--no-progress")

	data, _ := os.ReadFile(plan)
	evil := strings.Replace(string(data), filepath.Join(lib, "2023"), filepath.Join(home, "outside"), 1)
	bad := filepath.Join(home, "evil.json")
	os.WriteFile(bad, []byte(evil), 0o644)
	out, code := runCLIWithHome(t, home, "organize", "--plan", bad, "--apply", "--no-progress")
	if code == 0 || !strings.Contains(out, "outside the library") {
		t.Errorf("tampered plan accepted: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, "outside")); err == nil {
		t.Errorf("wrote outside the library")
	}

	if _, code := runCLIWithHome(t, home, "organize", src, "--plan", plan, "--no-progress"); code == 0 {
		t.Errorf("folders combined with --plan should be rejected")
	}
	if _, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", plan, "--apply", "--no-progress"); code == 0 {
		t.Errorf("--save-plan with --apply should be rejected")
	}
	if _, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--save-plan", filepath.Join(src, "plan.json"), "--no-progress"); code == 0 {
		t.Errorf("plan file inside a source should be rejected")
	}
	if _, code := runCLIWithHome(t, home, "organize", "--plan", filepath.Join(home, "missing.json")); code == 0 {
		t.Errorf("missing plan should fail")
	}
	if _, code := runCLIWithHome(t, home, "organize", "--plan", plan, "--dest", filepath.Join(home, "Other"), "--no-progress"); code == 0 {
		t.Errorf("mismatched --dest should be rejected")
	}
}
