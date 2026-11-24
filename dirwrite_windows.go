//go:build windows

package main

// canModifyDir is not checked on Windows; callers treat every folder as writable and
// let the move report any error.
func canModifyDir(dir string) bool { return true }
