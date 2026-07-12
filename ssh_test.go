package main

import (
	"strings"
	"testing"
)

// =============================================================================
// sshTargetFor
// =============================================================================

func TestSshTargetFor(t *testing.T) {
	cfg := map[string]string{"nas": "admin@nas.local:/mnt/backup"}
	if got := sshTargetFor("nas", cfg); got != "admin@nas.local:/mnt/backup" {
		t.Errorf("configured machine = %q, want the SSH target", got)
	}
	// Unknown machine falls back to the machine id itself.
	if got := sshTargetFor("laptop", cfg); got != "laptop" {
		t.Errorf("unknown machine = %q, want laptop", got)
	}
}

// =============================================================================
// provideSshErrorHelp
// =============================================================================

func TestProvideSshErrorHelp(t *testing.T) {
	tests := []struct {
		name, errMsg, detail, wantSubstr string
	}{
		{"refused", "connection refused", "", "Connection refused"},
		{"timeout", "connection timeout", "", "Connection timeout"},
		{"denied", "Permission denied", "", "Permission denied"},
		{"notfound", "no such file or directory", "", "Remote path not found"},
		{"cmd", "bash: photo-organizer: command not found", "", "Remote command not found"},
		{"default", "weird unexpected error", "raw detail", "verbose debugging"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				provideSshErrorHelp(tt.errMsg, tt.detail, "user@host")
			})
			if !strings.Contains(out, tt.wantSubstr) {
				t.Errorf("provideSshErrorHelp(%q) missing %q\n%s", tt.errMsg, tt.wantSubstr, out)
			}
			// The default arm echoes any non-empty detail.
			if tt.detail != "" && !strings.Contains(out, tt.detail) {
				t.Errorf("expected detail %q echoed\n%s", tt.detail, out)
			}
		})
	}
}

// =============================================================================
// sshVerifyPaths
// =============================================================================

func TestSshVerifyPathsEmpty(t *testing.T) {
	if got := sshVerifyPaths("user@host", nil); len(got) != 0 {
		t.Errorf("no paths should yield empty map, got %v", got)
	}
}

func TestSshVerifyPathsUnreachable(t *testing.T) {
	// A .invalid host never resolves, so ssh fails fast; the short timeout caps
	// it regardless. Duplicate paths also exercise the dedup step.
	t.Setenv("PHOTO_ORGANIZER_SSH_TIMEOUT", "2s")
	out := captureStderr(t, func() {
		got := sshVerifyPaths("photo-organizer-nonexistent.invalid", []string{"/a", "/a", "/b"})
		if len(got) != 0 {
			t.Errorf("unreachable host should mark nothing verified, got %v", got)
		}
	})
	if !strings.Contains(out, "SSH verification") {
		t.Errorf("expected an SSH failure/timeout warning, got:\n%s", out)
	}
}
