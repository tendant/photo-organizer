package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func printIgnoreUsage() {
	fmt.Fprintln(os.Stderr, "Usage: photo-organizer ignore --init <folder>...")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Writes the default .photoignore (program files, games, caches, system folders)")
	fmt.Fprintln(os.Stderr, "into each folder that has none. An existing .photoignore is never changed.")
	fmt.Fprintln(os.Stderr, "Nothing is scanned.")
}

func runIgnore(args []string) error {
	fs := flag.NewFlagSet("ignore", flag.ContinueOnError)
	fs.Usage = printIgnoreUsage
	initFlag := fs.Bool("init", false, "write the default .photoignore into each folder that has none")
	if err := fs.Parse(args); err != nil {
		return errFailed
	}
	if !*initFlag || fs.NArg() == 0 {
		printIgnoreUsage()
		return errFailed
	}
	failed := false
	for _, dir := range fs.Args() {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s: %v\n", dir, err)
			failed = true
			continue
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "Error: %s is not a folder\n", dir)
			failed = true
			continue
		}
		path := filepath.Join(abs, ".photoignore")
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("Already there, left alone: %s\n", path)
			continue
		}
		if !ensurePhotoIgnore(abs) {
			fmt.Fprintf(os.Stderr, "Error: could not write %s (read-only?)\n", path)
			failed = true
		}
	}
	if failed {
		return errFailed
	}
	return nil
}
