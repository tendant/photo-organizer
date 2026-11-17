package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// =============================================================================
// Lookup (Find manifests containing a folder)
// =============================================================================

// lookupCopy is one manifest row, with where it was found.
type lookupCopy struct {
	row  ManifestRow
	held bool // from a holding-folder manifest
}

func (c lookupCopy) path() string { return filepath.Join(c.row.ScanPath, c.row.RelativePath) }

func printLookupUsage() {
	fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP - every place a file is stored\n\n")
	fmt.Fprintf(os.Stderr, "Usage: photo-organizer lookup <file> [--name]\n\n")
	fmt.Fprintf(os.Stderr, "By default the file is found by CONTENT: its size and scan hash (first and last 32 KB) are\n")
	fmt.Fprintf(os.Stderr, "matched against every manifest, so renamed and moved copies show up too. <file> is a file on\n")
	fmt.Fprintf(os.Stderr, "this machine (hashed if it is not in a manifest) or a path as the manifests record it.\n")
	fmt.Fprintf(os.Stderr, "--name instead lists manifest entries whose name or path matches, without comparing content\n")
	fmt.Fprintf(os.Stderr, "(also how a folder such as \"Vacation/\" is searched; for the copies of a whole folder use check-backup).\n\n")
	fmt.Fprintf(os.Stderr, "Example:\n  photo-organizer lookup /data/Photos-All/IMG_0001.jpg\n  photo-organizer lookup --name \"vacation\"\n")
}

func runLookup(args []string) error {
	byName := false
	var rest []string
	for _, a := range args {
		if a == "--name" {
			byName = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		printLookupUsage()
		return errFailed
	}
	arg := rest[0]

	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return nil
	}
	held, _ := filepath.Glob(filepath.Join(manifestDir, heldDirName, "*.csv"))

	var all []lookupCopy
	for _, group := range []struct {
		files []string
		held  bool
	}{{matches, false}, {held, true}} {
		for _, mp := range group.files {
			src, err := readManifest(mp)
			if err != nil {
				continue
			}
			for _, row := range src.Rows {
				all = append(all, lookupCopy{row: row, held: group.held})
			}
		}
	}

	if byName {
		return lookupByName(arg, all)
	}
	return lookupByContent(arg, all)
}

// lookupByContent finds the file's size + hash, then lists every manifest row with the same content.
func lookupByContent(arg string, all []lookupCopy) error {
	if strings.HasSuffix(arg, "/") {
		fmt.Fprintf(os.Stderr, "\n%s is a folder. Use `photo-organizer check-backup %s` for where its files are copied,\n", arg, arg)
		fmt.Fprintf(os.Stderr, "or `lookup --name` to list entries by folder name.\n")
		return errFailed
	}
	abs, _ := filepath.Abs(arg)
	var seeds []ManifestRow
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		fmt.Fprintf(os.Stderr, "\n%s is a folder. Use `photo-organizer check-backup %s` for where its files are copied.\n", arg, arg)
		return errFailed
	} else if err == nil {
		// A file on this machine: its manifest row if still current, otherwise hash it now.
		for _, c := range all {
			if c.path() == abs && freshOnDisk(abs, c.row) && c.row.PartialHash != "" {
				seeds = append(seeds, c.row)
				break
			}
		}
		if len(seeds) == 0 {
			h, _ := processFile(abs)
			if h == "" {
				fmt.Fprintf(os.Stderr, "Cannot read %s\n", abs)
				return errFailed
			}
			seeds = append(seeds, ManifestRow{SizeBytes: st.Size(), PartialHash: h})
		}
	} else {
		// Not on disk here: a path as the manifests record it.
		clean := filepath.ToSlash(filepath.Clean(arg))
		for _, c := range all {
			full := filepath.ToSlash(c.path())
			rel := filepath.ToSlash(c.row.RelativePath)
			if c.row.PartialHash != "" && (full == clean || rel == clean || (strings.Contains(clean, "/") && strings.HasSuffix(full, "/"+strings.TrimPrefix(clean, "/")))) {
				seeds = append(seeds, c.row)
			}
		}
		if len(seeds) == 0 {
			fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n\n❌ No file at that path, and no manifest entry with that path.\n", arg)
			fmt.Fprintf(os.Stderr, "   Give a file that exists, a path as recorded in a manifest, or add --name to search by name.\n")
			return nil
		}
	}

	// One block per distinct content (a path can only be one file, but be safe).
	shown := map[string]bool{}
	fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n", arg)
	for _, seed := range seeds {
		k := hashKey(seed)
		if shown[k] {
			continue
		}
		shown[k] = true
		var copies []lookupCopy
		for _, c := range all {
			if rowsMatch(seed, c.row) {
				copies = append(copies, c)
			}
		}
		sort.Slice(copies, func(i, j int) bool {
			if copies[i].row.MachineName != copies[j].row.MachineName {
				return copies[i].row.MachineName < copies[j].row.MachineName
			}
			return copies[i].path() < copies[j].path()
		})
		hash := seed.PartialHash
		if len(hash) > 16 {
			hash = hash[:16] + "..."
		}
		fmt.Fprintf(os.Stderr, "\nSame content (%s, hash %s) found in %d place(s):\n\n", formatSize(seed.SizeBytes), hash, len(copies))
		if len(copies) == 0 {
			fmt.Fprintf(os.Stderr, "  Not in any manifest. Scan the folder to record it.\n")
		}
		for i, c := range copies {
			note := ""
			switch {
			case c.held:
				note = "  [holding folder]"
			case c.path() == abs:
				note = "  [this file]"
			}
			fmt.Fprintf(os.Stderr, "%d. %s: %s%s\n", i+1, c.row.MachineName, c.path(), note)
			fmt.Fprintf(os.Stderr, "   Modified: %s\n", c.row.FileModified)
		}
	}
	fmt.Fprintf(os.Stderr, "\nMatched by size and scan hash (first and last 32 KB) from the manifests; nothing was read on other machines.\n")
	return nil
}

// lookupByName lists manifest entries whose name or path matches (--name). Content is not compared.
func lookupByName(arg string, all []lookupCopy) error {
	lookupPath := filepath.Clean(arg)
	filename := filepath.Base(lookupPath)
	isFolder := strings.HasSuffix(arg, "/")

	var results []lookupCopy
	for _, c := range all {
		row := c.row
		var ok bool
		if isFolder {
			searchPath := strings.TrimSuffix(lookupPath, "/")
			ok = strings.HasPrefix(row.RelativePath, searchPath+"/") || row.RelativePath == searchPath
		} else {
			ok = strings.Contains(row.RelativePath, filename) || filepath.Base(row.RelativePath) == filename
		}
		if ok {
			results = append(results, c)
		}
	}

	fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP (by name): %s\n\n", lookupPath)
	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "❌ Not found in any manifests\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Found %d match(es) by name; content was not compared:\n\n", len(results))
	for i, c := range results {
		row := c.row
		hash := row.FullHash
		if hash == "" {
			hash = row.PartialHash
		}
		if len(hash) > 16 {
			hash = hash[:16] + "..."
		}
		note := ""
		if c.held {
			note = "  [holding folder]"
		}
		fmt.Fprintf(os.Stderr, "%d. %s%s\n", i+1, row.RelativePath, note)
		fmt.Fprintf(os.Stderr, "   Scan path:   %s\n", row.ScanPath)
		fmt.Fprintf(os.Stderr, "   Machine:     %s\n", row.MachineName)
		fmt.Fprintf(os.Stderr, "   Size:        %s\n", formatSize(row.SizeBytes))
		fmt.Fprintf(os.Stderr, "   Hash:        %s\n", hash)
		fmt.Fprintf(os.Stderr, "   Modified:    %s\n\n", row.FileModified)
	}
	return nil
}

func runSearch(args []string) error {
	return runSearchAnalyze(args)
}
