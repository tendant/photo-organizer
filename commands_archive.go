package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// Archive and Delete Commands
// =============================================================================

func runArchive(args []string) error {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer archive <folder-path> --dest <archive-dir>\n\n")
		fmt.Fprintf(os.Stderr, "Move folder to local archive directory and update manifests.\n")
		return errFailed
	}

	sourceFolder := args[0]
	archiveDir := parseRequiredDestFlag(args)

	if archiveDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required\n")
		return errFailed
	}

	// Resolve paths to absolute
	absSourceFolder, err := resolveExistingFolder(sourceFolder)
	if err != nil {
		if _, statErr := os.Stat(sourceFolder); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		}
		return errFailed
	}
	absArchiveDir, err := filepath.Abs(archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid archive directory %q\n", archiveDir)
		return errFailed
	}

	// Check archive directory exists or create it
	if err := os.MkdirAll(absArchiveDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create archive directory: %v\n", err)
		return errFailed
	}

	// Create timestamped archive folder name (include time to avoid conflicts)
	folderName := filepath.Base(absSourceFolder)
	archiveFolderName := generateArchiveFolderName(folderName, time.Now())
	archiveFolder := filepath.Join(absArchiveDir, archiveFolderName)

	// === PREVIEW: Show what will happen ===
	fmt.Fprintf(os.Stderr, "\n📋 ARCHIVE PREVIEW\n")
	fmt.Fprintf(os.Stderr, "Source: %s\n", absSourceFolder)

	// Scan folder to show stats
	folderFiles, _, err := scanDirectory(absSourceFolder, make(map[string]CacheEntry), nil)
	if err == nil && len(folderFiles) > 0 {
		totalSize := int64(0)
		for _, f := range folderFiles {
			totalSize += f.Size
		}
		fmt.Fprintf(os.Stderr, "Files: %d, Size: %s\n", len(folderFiles), formatBytes(totalSize))
	}

	fmt.Fprintf(os.Stderr, "Archive to: %s\n", archiveFolder)
	fmt.Fprintf(os.Stderr, "\n⚠️  SAFETY CHECK - Before archiving, verify:\n")
	fmt.Fprintf(os.Stderr, "  1. You have 2+ valid backup copies (remote machines)\n")
	fmt.Fprintf(os.Stderr, "  2. Run: photo-organizer verify %s\n", absSourceFolder)
	fmt.Fprintf(os.Stderr, "  3. Confirm status shows '✓ SAFE TO ARCHIVE'\n")

	if !confirmPrompt("\nProceed?") {
		fmt.Fprintf(os.Stderr, "Cancelled.\n")
		return nil
	}

	// Move the folder
	fmt.Fprintf(os.Stderr, "\n▶️  Moving %s → %s\n", absSourceFolder, archiveFolder)
	if err := os.Rename(absSourceFolder, archiveFolder); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to move folder: %v\n", err)
		return errFailed
	}

	// Get machine name
	machineName := resolveMachineID("")

	// Rescan the parent of source folder to remove old entries (with --prune)
	sourceParent := filepath.Dir(absSourceFolder)
	fmt.Fprintf(os.Stderr, "\nUpdating manifest for %s (pruning removed files)...\n", sourceParent)
	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, sourceParent))

	files, _, err := scanDirectory(sourceParent, make(map[string]CacheEntry), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Could not refresh parent folder scan: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Stale entries may remain. Run 'photo-organizer cleanup-manifests' to clean them up.\n")
	} else {
		stats, err := updateManifest(sourceParent, files, manifestFile, machineName, true) // prune=true
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update manifest: %v\n", err)
		} else if stats.Pruned > 0 {
			fmt.Fprintf(os.Stderr, "✓ Removed %d stale entries from parent manifest\n", stats.Pruned)
		}
	}

	// Remove or prune old manifests for the archived folder
	fmt.Fprintf(os.Stderr, "Cleaning up old manifest entries...\n")
	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	if matches, err := filepath.Glob(filepath.Join(manifestDir, "*.csv")); err == nil {
		for _, manifestPath := range matches {
			// Read manifest to check if it references the old source folder
			if src, err := readManifest(manifestPath); err == nil {
				// Case 1: Manifest scanned the old folder or its subdirectories - remove it entirely
				if src.ScanPath == absSourceFolder || strings.HasPrefix(src.ScanPath, absSourceFolder+string(filepath.Separator)) {
					if err := os.Remove(manifestPath); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not remove old manifest %s: %v\n", manifestPath, err)
					}
					continue
				}

				// Case 2: Manifest scanned a parent directory - prune entries for the old folder
				if strings.HasPrefix(absSourceFolder, src.ScanPath+string(filepath.Separator)) {
					if prunedCount, err := pruneManifestEntries(manifestPath, absSourceFolder); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not prune manifest %s: %v\n", manifestPath, err)
					} else if prunedCount > 0 {
						fmt.Fprintf(os.Stderr, "✓ Pruned %d entries from %s\n", prunedCount, filepath.Base(manifestPath))
					}
				}
			}
		}
	}

	// Scan archive folder and update manifest (partial hashes only, no full hash computation)
	fmt.Fprintf(os.Stderr, "Scanning archive folder (partial hashes only)...\n")
	archiveParent := filepath.Dir(archiveFolder)
	manifestFile = filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, archiveParent))

	files, _, err = scanDirectory(archiveParent, make(map[string]CacheEntry), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Could not scan archive folder: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Updating manifest for archive location...\n")
		fmt.Fprintf(os.Stderr, "  Processing %d files...\n", len(files))
		startTime := time.Now()
		stats, err := updateManifest(archiveParent, files, manifestFile, machineName, false)
		elapsed := time.Since(startTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update archive manifest: %v\n", err)
		} else if stats.New > 0 || stats.Updated > 0 {
			fmt.Fprintf(os.Stderr, "✓ Updated archive manifest (%d new, %d updated, %d pruned) in %v\n", stats.New, stats.Updated, stats.Pruned, elapsed)
		} else {
			fmt.Fprintf(os.Stderr, "✓ Archive manifest up to date in %v\n", elapsed)
		}
	}

	fmt.Fprintf(os.Stderr, "\n✓ Folder archived to %s\n", archiveFolder)
	fmt.Fprintf(os.Stderr, "  Files are still tracked in manifest at new location\n")
	return nil
}
