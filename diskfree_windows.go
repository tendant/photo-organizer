//go:build windows

package main

// diskFreeBytes is not implemented on Windows; callers treat that as "unknown"
// and skip the free-space check.
func diskFreeBytes(path string) (uint64, bool) { return 0, false }
