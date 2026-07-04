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
