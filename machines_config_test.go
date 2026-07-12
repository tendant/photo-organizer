package main

import (
	"runtime"
	"testing"
)

// =============================================================================
// blockDeviceName
// =============================================================================

func TestBlockDeviceName(t *testing.T) {
	tests := []struct {
		dev, want string
	}{
		{"/dev/sdb1", "sdb"},  // strip dir + partition number
		{"/dev/sda", "sda"},   // no partition suffix
		{"/dev/sda12", "sda"}, // multi-digit partition
		{"sdb1", "sdb"},       // already a bare device
		{"/dev/sdb", "sdb"},   // whole disk
	}
	for _, tt := range tests {
		if got := blockDeviceName(tt.dev); got != tt.want {
			t.Errorf("blockDeviceName(%q) = %q, want %q", tt.dev, got, tt.want)
		}
	}
}

// =============================================================================
// mountDeviceForPath
// =============================================================================

func TestMountDeviceForPath(t *testing.T) {
	mounts := []byte(`sysfs /sys sysfs rw 0 0
/dev/sda1 / ext4 rw 0 0
/dev/sdb1 /media/usb vfat rw 0 0
short-line
proc /proc proc rw 0 0
`)

	// Longest matching mount wins: /media/usb beats /.
	if got := mountDeviceForPath(mounts, "/media/usb/photos/a.jpg"); got != "/dev/sdb1" {
		t.Errorf("mountDeviceForPath usb = %q, want /dev/sdb1", got)
	}
	// Falls back to the root mount for unrelated paths.
	if got := mountDeviceForPath(mounts, "/home/user/pics"); got != "/dev/sda1" {
		t.Errorf("mountDeviceForPath home = %q, want /dev/sda1", got)
	}
	// No matching mount point -> empty.
	onlyUsb := []byte("/dev/sdb1 /media/usb vfat rw 0 0\n")
	if got := mountDeviceForPath(onlyUsb, "/home/user"); got != "" {
		t.Errorf("mountDeviceForPath no-match = %q, want empty", got)
	}
}

// =============================================================================
// isRemovablePath — the pure scan-path heuristic (fallback of isRemovableSource)
// =============================================================================

func TestIsRemovablePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// macOS removable mounts
		{"/Volumes/Untitled", true},
		{"/volumes/MyUSB", true},
		// Linux removable mounts
		{"/mnt/external", true},
		{"/media/pi", true}, // /media/<one> — 3 parts, treated as removable
		// Windows removable drives
		{"D:", true},
		{"E:", true},
		{"C:", false}, // system drive is not removable
		// Permanent storage
		{"/Users/lei/Photos", false},
		{"/home/user/Photos", false},
		{"/volume1/photos", false}, // NAS share, not /volumes/
		// /media paths with permanent-storage keywords
		{"/media/Photos/archived", false},
		{"/media/backups/2023", false},
		{"/media/tank/data", false},
	}
	for _, tt := range tests {
		if got := isRemovablePath(tt.path); got != tt.want {
			t.Errorf("isRemovablePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// =============================================================================
// isRemovableSource — config tag is authoritative, path heuristic is fallback
// =============================================================================

func TestIsRemovableSource(t *testing.T) {
	cfg := map[string]string{
		"sdcard": "[removable] scanned from: /Volumes/Untitled",
		"nas":    "admin@nas.local:/mnt/backup", // SSH target -> durable
		"mac":    "[local] scanned from: /Users/x/Photos",
	}
	tests := []struct {
		name, machine, scanPath string
		want                    bool
	}{
		// Config tag wins over the path heuristic — in both directions.
		{"tagged removable, durable-looking path", "sdcard", "/home/user/photos", true},
		{"ssh target, removable-looking path", "nas", "/Volumes/nas-mount", false},
		{"local tag, removable-looking path", "mac", "/Volumes/TimeMachine", false},
		// No config entry -> path heuristic decides.
		{"unknown machine, /Volumes path", "cam", "/Volumes/USB", true},
		{"unknown machine, home path", "laptop", "/home/user/photos", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRemovableSource(tt.machine, tt.scanPath, cfg); got != tt.want {
				t.Errorf("isRemovableSource(%q, %q) = %v, want %v", tt.machine, tt.scanPath, got, tt.want)
			}
		})
	}

	// Nil config degrades to the pure heuristic.
	if !isRemovableSource("x", "/Volumes/USB", nil) {
		t.Error("nil config with removable path should be removable")
	}
	if isRemovableSource("x", "/data/photos", nil) {
		t.Error("nil config with durable path should not be removable")
	}
}

// =============================================================================
// OS-integration contract: unknown paths are never flagged removable
// =============================================================================

func TestIsRemovableMediaSafeDefault(t *testing.T) {
	// The detectors deliberately fail closed — a path that can't be resolved
	// must return false rather than a spurious "removable" warning.
	if isRemovableMedia("/definitely/not/a/real/mount/xyz") {
		t.Error("isRemovableMedia should return false for an unresolvable path")
	}
}

func TestIsMacOSRemovableErrorPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("diskutil is macOS-only")
	}
	// diskutil errors on a non-existent path -> false.
	if isMacOSRemovable("/definitely/not/a/real/mount/xyz") {
		t.Error("isMacOSRemovable should return false when diskutil fails")
	}
}

func TestIsLinuxRemovableSafeDefault(t *testing.T) {
	// On non-Linux, /proc/mounts is absent -> false. On Linux, an unresolvable
	// or non-removable path likewise yields false. Either way: not removable.
	if isLinuxRemovable("/definitely/not/a/real/mount/xyz") {
		t.Error("isLinuxRemovable should return false for an unresolvable path")
	}
}
