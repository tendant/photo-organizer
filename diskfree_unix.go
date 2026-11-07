//go:build !windows

package main

import "syscall"

// diskFreeBytes returns the space available to an unprivileged user on the
// filesystem holding path (which must exist).
func diskFreeBytes(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
