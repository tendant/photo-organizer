package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// Lookup (Find manifests containing a folder)
// =============================================================================

func runLookup(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP FILE/FOLDER - Show complete details\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer lookup <name-or-path>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"vacation.jpg\"\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"Vacation/\"\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"2026/06/\"\n\n")
		fmt.Fprintf(os.Stderr, "Shows: location, scan path, machine, date, size, hash, backup status\n")
		os.Exit(1)
	}

	lookupPath := filepath.Clean(args[0])

	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return
	}

	type FileMatch struct {
		relativePath string
		scanPath     string
		machine      string
		fileSize     string
		hash         string
		fileModified string
		manifestPath string
	}
	var results []FileMatch

	// Search manifests for matching file/folder
	filename := filepath.Base(lookupPath)
	isFolder := strings.HasSuffix(lookupPath, "/")

	for _, manifestPath := range matches {
		src, err := readManifest(manifestPath)
		if err != nil || len(src.Rows) == 0 {
			continue
		}

		// Search for matching entries
		for _, row := range src.Rows {
			var matches bool
			if isFolder {
				// Folder search: check if entry starts with folder path
				searchPath := strings.TrimSuffix(lookupPath, "/")
				matches = strings.HasPrefix(row.RelativePath, searchPath+"/") || row.RelativePath == searchPath
			} else {
				// File search: match filename or path contains
				matches = strings.Contains(row.RelativePath, filename) || filepath.Base(row.RelativePath) == filename
			}

			if matches {
				hash := row.FullHash
				if hash == "" {
					hash = row.PartialHash
				}
				if len(hash) > 16 {
					hash = hash[:16] + "..."
				}
				results = append(results, FileMatch{
					relativePath: row.RelativePath,
					scanPath:     row.ScanPath,
					machine:      row.MachineName,
					fileSize:     formatSize(row.SizeBytes),
					hash:         hash,
					fileModified: row.FileModified,
					manifestPath: manifestPath,
				})
			}
		}
	}

	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n\n", lookupPath)
		fmt.Fprintf(os.Stderr, "❌ Not found in any manifests\n")
		return
	}

	// Display results
	fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n\n", lookupPath)
	fmt.Fprintf(os.Stderr, "Found %d match(es):\n\n", len(results))

	for i, match := range results {
		fmt.Fprintf(os.Stderr, "%d. %s\n", i+1, match.relativePath)
		fmt.Fprintf(os.Stderr, "   Scan path:   %s\n", match.scanPath)
		fmt.Fprintf(os.Stderr, "   Machine:     %s\n", match.machine)
		fmt.Fprintf(os.Stderr, "   Size:        %s\n", match.fileSize)
		fmt.Fprintf(os.Stderr, "   Hash:        %s\n", match.hash)
		fmt.Fprintf(os.Stderr, "   Modified:    %s\n\n", match.fileModified)
	}
}

func runSearch(args []string) {
	runSearchAnalyze(args)
}
