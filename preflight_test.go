package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr mirrors captureStdout for helpers that write to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// =============================================================================
// RunPreflightChecks
// =============================================================================

func TestRunPreflightChecks(t *testing.T) {
	// No checks -> trivially passes.
	if !RunPreflightChecks(nil) {
		t.Error("empty checks should pass")
	}

	// All passing.
	out := captureStderr(t, func() {
		if !RunPreflightChecks([]PreflightCheck{{Name: "a", Pass: true}, {Name: "b", Pass: true}}) {
			t.Error("all-passing checks should return true")
		}
	})
	if !strings.Contains(out, "✓ a") || !strings.Contains(out, "Pre-flight checks passed") {
		t.Errorf("expected pass output, got:\n%s", out)
	}

	// A failing check (with hint) aborts; passing ones still print.
	out = captureStderr(t, func() {
		checks := []PreflightCheck{
			{Name: "ok", Pass: true},
			{Name: "bad", Pass: false, Error: "boom", Hint: "try again"},
			{Name: "nohint", Pass: false, Error: "nope"},
		}
		if RunPreflightChecks(checks) {
			t.Error("a failing check should return false")
		}
	})
	for _, w := range []string{"✓ ok", "✗ bad: boom", "→ try again", "✗ nohint: nope", "Aborting operation"} {
		if !strings.Contains(out, w) {
			t.Errorf("failure output missing %q\n---\n%s", w, out)
		}
	}
}

// =============================================================================
// CheckDirWritable
// =============================================================================

func TestCheckDirWritable(t *testing.T) {
	dir := t.TempDir()
	if c := CheckDirWritable(dir, "d"); !c.Pass {
		t.Errorf("temp dir should be writable: %+v", c)
	}

	// A path under a non-existent directory cannot be written to.
	if c := CheckDirWritable(filepath.Join(dir, "nope"), "d"); c.Pass || c.Error != "not writable" {
		t.Errorf("non-existent dir = %+v, want fail/not-writable", c)
	}
}

// =============================================================================
// MonitorDiskSpace
// =============================================================================

func TestMonitorDiskSpace(t *testing.T) {
	dir := t.TempDir()
	if !MonitorDiskSpace(dir) {
		t.Error("writable temp dir should report space available")
	}

	out := captureStderr(t, func() {
		if MonitorDiskSpace(filepath.Join(dir, "nope")) {
			t.Error("non-existent dir should report low space")
		}
	})
	if !strings.Contains(out, "Low disk space") {
		t.Errorf("expected warning, got:\n%s", out)
	}
}

// =============================================================================
// extractDeviceInfo
// =============================================================================

func TestExtractDeviceInfo(t *testing.T) {
	tests := []struct {
		info, want string
	}{
		{"machine [local]", ""},
		{"machine [removable]", "Removable media"},
		{"x [removable] scanned from: /Volumes/Untitled", "Camera/USB: Untitled"},
		{"x [removable] camera scanned from: /Volumes/DCIM", "Camera/USB: DCIM"},
		{"x [removable] scanned from: /Volumes/SDCARD", "SD Card: SDCARD"},
		{"x [removable] scanned from: /Volumes/Backup", "Removable: Backup"},
	}
	for _, tt := range tests {
		if got := extractDeviceInfo(tt.info); got != tt.want {
			t.Errorf("extractDeviceInfo(%q) = %q, want %q", tt.info, got, tt.want)
		}
	}
}
