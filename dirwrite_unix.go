//go:build !windows

package main

import "syscall"

// canModifyDir reports whether the current user may add and remove entries in dir
// (write + search permission). Renaming a file out of a folder needs this on the
// source folder; copying it needs only read access.
func canModifyDir(dir string) bool {
	return syscall.Access(dir, 0x2|0x1) == nil // W_OK|X_OK
}
