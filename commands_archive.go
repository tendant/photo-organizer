package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// ARCHIVE COMMANDS - Dated, immutable snapshots
// =============================================================================
//
// archive retires a folder into a timestamped folder that later runs never
// touch, so each snapshot stays restorable on its own. Use `backup` when you
// want a mirror that converges on every run.

func runArchive(args []string) error {
	folderPath := ""
	destLocation := ""
	keepSource := false
	progMode := progressAuto

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--keep":
			keepSource = true
		case "--progress":
			progMode = progressOn
		case "--no-progress":
			progMode = progressOff
		case "--dest":
			if i+1 < len(args) {
				destLocation = args[i+1]
				i++
			}
		default:
			if folderPath == "" {
				folderPath = args[i]
			} else if destLocation == "" {
				destLocation = args[i]
			}
		}
	}

	if folderPath == "" || destLocation == "" {
		fmt.Fprintf(os.Stderr, "\n🗄  ARCHIVE A FOLDER\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer archive <folder> --dest <target> [--keep] [--no-progress]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer archive ~/Photos/OldImport --dest /mnt/archive\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer archive ~/Photos/OldImport --dest nas:/archive\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer archive ~/Photos/OldImport --dest /mnt/archive --keep\n\n")
		fmt.Fprintf(os.Stderr, "Creates a dated snapshot such as 2026-06-20-143022-OldImport/ and\n")
		fmt.Fprintf(os.Stderr, "updates manifests so the files stay tracked at their new location.\n\n")
		fmt.Fprintf(os.Stderr, "Target: local path, or machine-id:/path or user@host:/path for SSH.\n")
		fmt.Fprintf(os.Stderr, "--keep: Copy instead of moving; the source folder stays where it is.\n\n")
		fmt.Fprintf(os.Stderr, "For an ongoing second copy, use 'backup' instead.\n")
		return errFailed
	}

	absSourceFolder, err := resolveExistingFolder(folderPath)
	if err != nil {
		if _, statErr := os.Stat(folderPath); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", folderPath)
		}
		return errFailed
	}

	// A remote destination goes over SSH; a local one is a plain directory.
	var remoteUserHost, remoteMachineID, remotePath, localArchiveDir string
	remote := isRemoteDestination(destLocation)
	if remote {
		machines := loadMachinesConfig()
		remoteUserHost, remoteMachineID, remotePath, err = resolveBackupDestination(destLocation, machines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			printBackupDestinationOptions(machines)
			fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer archive ~/Photos/OldImport --dest nas:/archive\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer archive ~/Photos/OldImport --dest ubuntu@192.168.1.100:/archive\n")
			return errFailed
		}
		// Deleting the source across the network is never automatic.
		keepSource = true
	} else {
		localArchiveDir, err = filepath.Abs(destLocation)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid archive directory %q\n", destLocation)
			return errFailed
		}
		if err := os.MkdirAll(localArchiveDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot create archive directory: %v\n", err)
			return errFailed
		}
	}

	// Timestamp keeps each snapshot distinct, so runs never overwrite each other.
	archiveFolderName := generateArchiveFolderName(filepath.Base(absSourceFolder), time.Now())

	relPaths, relSizes, totalSize := listFilesUnder(absSourceFolder)
	showProgress := progressEnabled(progMode)
	destDisplay := filepath.Join(localArchiveDir, archiveFolderName)
	if remote {
		destDisplay = remoteUserHost + ":" + remoteArchivePath(remotePath, archiveFolderName)
	}

	// === PREVIEW: Show what will happen ===
	fmt.Fprintf(os.Stderr, "\n📋 ARCHIVE PREVIEW\n")
	fmt.Fprintf(os.Stderr, "Source: %s\n", absSourceFolder)
	fmt.Fprintf(os.Stderr, "Files: %d, Size: %s\n", len(relPaths), formatBytes(totalSize))
	fmt.Fprintf(os.Stderr, "Archive to: %s\n", destDisplay)
	if keepSource {
		fmt.Fprintf(os.Stderr, "Source folder is kept (copy, not move)\n")
	}
	fmt.Fprintf(os.Stderr, "\n⚠️  SAFETY CHECK - Before archiving, verify:\n")
	fmt.Fprintf(os.Stderr, "  1. You have 2+ valid backup copies (remote machines)\n")
	fmt.Fprintf(os.Stderr, "  2. Run: photo-organizer check-backup %s\n", absSourceFolder)
	fmt.Fprintf(os.Stderr, "  3. Confirm status shows '✓ SAFE TO ARCHIVE'\n")

	if !confirmPrompt("\nProceed?") {
		fmt.Fprintf(os.Stderr, "Cancelled.\n")
		return nil
	}

	if remote {
		return archiveToRemote(absSourceFolder, relPaths, relSizes, totalSize, archiveFolderName,
			remoteUserHost, remoteMachineID, remotePath, showProgress)
	}
	return archiveToLocal(absSourceFolder, relPaths, totalSize,
		filepath.Join(localArchiveDir, archiveFolderName), keepSource, showProgress)
}

// archiveToRemote copies the folder into a dated snapshot on a remote machine
// and refreshes that machine's manifest. The source folder is left in place.
func archiveToRemote(absSourceFolder string, relPaths []string, relSizes []int64, totalSize int64,
	archiveFolderName, remoteUserHost, remoteMachineID, remotePath string, showProgress bool) error {

	archivePath := remoteArchivePath(remotePath, archiveFolderName)

	fmt.Fprintf(os.Stderr, "\n▶️  Copying %d files → %s:%s\n", len(relPaths), remoteUserHost, archivePath)
	prog := newProgressReporter("Copying", unitBytes, totalSize, int64(len(relPaths)), showProgress)
	// No plan pre-pass: an archive always targets a fresh dated folder, so
	// everything in the list is genuinely new work.
	if err := rsyncRelPaths(absSourceFolder, relPaths, relSizes, remoteUserHost, archivePath, nil, prog); err != nil {
		prog.Clear()
		fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
		return errFailed
	}
	prog.Done(fmt.Sprintf("Copied %s files (%s)", formatCount(len(relPaths)), formatSize(totalSize)))

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "\n✓ Folder archived to %s:%s\n", remoteUserHost, archivePath)
		fmt.Fprintf(os.Stderr, "⚠ Manifest refresh was skipped — the archive is not tracked yet.\n")
		fmt.Fprintf(os.Stderr, "  Add this host first: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		return nil
	}

	if err := refreshRemoteManifest(remoteUserHost, remoteMachineID, archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}

	fmt.Fprintf(os.Stderr, "\n✓ Folder archived to %s:%s\n", remoteUserHost, archivePath)
	fmt.Fprintf(os.Stderr, "  Source folder was kept. Verify with 'photo-organizer check-backup %s'\n", absSourceFolder)
	fmt.Fprintf(os.Stderr, "  before removing it.\n")
	return nil
}

// archiveToLocal moves (or copies, with keepSource) the folder into a dated
// snapshot on this machine and repoints the manifests at the new location.
func archiveToLocal(absSourceFolder string, relPaths []string, totalSize int64,
	archiveFolder string, keepSource, showProgress bool) error {

	if keepSource {
		fmt.Fprintf(os.Stderr, "\n▶️  Copying %s → %s\n", absSourceFolder, archiveFolder)
		prog := newProgressReporter("Copying", unitBytes, totalSize, int64(len(relPaths)), showProgress)
		if err := copyRelPaths(absSourceFolder, relPaths, archiveFolder, prog); err != nil {
			prog.Clear()
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return errFailed
		}
		prog.Done(fmt.Sprintf("Copied %s files (%s)", formatCount(len(relPaths)), formatSize(totalSize)))
	} else {
		fmt.Fprintf(os.Stderr, "\n▶️  Moving %s → %s\n", absSourceFolder, archiveFolder)
		if err := os.Rename(absSourceFolder, archiveFolder); err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to move folder: %v\n", err)
			return errFailed
		}
	}

	machineName := resolveMachineID("")
	manifestRoot := filepath.Join(userHomeDir(), "manifests")

	if !keepSource {
		// Rescan the parent of source folder to remove old entries (with --prune)
		sourceParent := filepath.Dir(absSourceFolder)
		fmt.Fprintf(os.Stderr, "\nUpdating manifest for %s (pruning removed files)...\n", sourceParent)
		manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, sourceParent))

		files, _, err := scanDirectory(sourceParent, make(map[string]CacheEntry), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not refresh parent folder scan: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Stale entries may remain. Run 'photo-organizer manifests --cleanup' to clean them up.\n")
		} else {
			stats, err := updateManifest(sourceParent, files, manifestFile, machineName, true) // prune=true
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update manifest: %v\n", err)
			} else if stats.Pruned > 0 {
				fmt.Fprintf(os.Stderr, "✓ Removed %d stale entries from parent manifest\n", stats.Pruned)
			}
		}

		pruneManifestsForMovedFolder(absSourceFolder)
	}

	// Scan archive folder and update manifest (partial hashes only, no full hash computation)
	fmt.Fprintf(os.Stderr, "Scanning archive folder (partial hashes only)...\n")
	archiveParent := filepath.Dir(archiveFolder)
	manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, archiveParent))

	files, _, err := scanDirectory(archiveParent, make(map[string]CacheEntry), nil)
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

// pruneManifestsForMovedFolder drops manifest entries that still point at a
// folder that has been moved away.
func pruneManifestsForMovedFolder(absSourceFolder string) {
	fmt.Fprintf(os.Stderr, "Cleaning up old manifest entries...\n")
	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	matches, err := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if err != nil {
		return
	}

	for _, manifestPath := range matches {
		src, err := readManifest(manifestPath)
		if err != nil {
			continue
		}

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

// runRestore restores files from an archive snapshot to a destination
func runRestore(args []string) error {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "\n📥 RESTORE FILES FROM ARCHIVE\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer restore <archive-path> <destination>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer restore /mnt/archive/2026-06-20-143022-Photos ~/Restored\n\n")
		fmt.Fprintf(os.Stderr, "Restores all files from archive to destination folder.\n")
		return errFailed
	}

	archivePath := args[0]
	destination := args[1]

	// Verify archive exists
	if _, err := os.Stat(archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Archive not found: %s\n", archivePath)
		return errFailed
	}

	// Create destination if needed
	if err := os.MkdirAll(destination, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot create destination: %v\n", err)
		return errFailed
	}

	fmt.Printf("📥 RESTORE FROM ARCHIVE\n\n")
	fmt.Printf("Source:      %s\n", archivePath)
	fmt.Printf("Destination: %s\n\n", destination)

	// Walk archive and restore files
	filesRestored := 0
	filesFailed := 0

	fmt.Printf("Restoring files...\n")
	filepath.Walk(archivePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || shouldSkipFile(path) {
			return nil
		}

		relPath, _ := filepath.Rel(archivePath, path)
		destFile := filepath.Join(destination, relPath)

		// Create destination directory
		destDir := filepath.Dir(destFile)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot create directory: %v\n", err)
			filesFailed++
			return nil
		}

		// Copy file
		if err := copyFile(path, destFile); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot restore %s: %v\n", relPath, err)
			filesFailed++
			return nil
		}

		filesRestored++
		return nil
	})

	fmt.Printf("\n✓ RESTORE COMPLETE\n\n")
	fmt.Printf("Results:\n")
	fmt.Printf("  Files restored: %d\n", filesRestored)
	fmt.Printf("  Files failed:   %d\n", filesFailed)
	fmt.Printf("  Destination:    %s\n\n", destination)

	if filesFailed > 0 {
		fmt.Printf("⚠️  Some files could not be restored.\n")
	} else {
		fmt.Printf("✓ All files successfully restored!\n")
	}
	return nil
}

// runListArchives shows all available snapshots under an archive root
func runListArchives(args []string) error {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n📋 LIST AVAILABLE ARCHIVES\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer list <archive-root>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer list /mnt/archive\n\n")
		fmt.Fprintf(os.Stderr, "Shows all timestamped archive folders with their contents.\n")
		return errFailed
	}

	archiveRoot := args[0]

	// Verify archive root exists
	if _, err := os.Stat(archiveRoot); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Archive root not found: %s\n", archiveRoot)
		return errFailed
	}

	fmt.Printf("📋 AVAILABLE ARCHIVES\n\n")
	fmt.Printf("Location: %s\n\n", archiveRoot)

	// List all archive folders
	entries, err := os.ReadDir(archiveRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot read archive root: %v\n", err)
		return errFailed
	}

	archives := make([]os.DirEntry, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			archives = append(archives, entry)
		}
	}

	if len(archives) == 0 {
		fmt.Printf("No archives found.\n")
		return nil
	}

	fmt.Printf("Archives Found:\n\n")
	for i, archive := range archives {
		archivePath := filepath.Join(archiveRoot, archive.Name())
		metrics := calculateFolderMetrics(archivePath)

		// Try to parse timestamp from folder name
		timestamp, _ := parseArchiveTimestamp(archive.Name())

		fmt.Printf("%d. %s\n", i+1, archive.Name())
		fmt.Printf("   Created:  %s\n", timestamp)
		fmt.Printf("   Files:    %d\n", metrics.FileCount)
		fmt.Printf("   Size:     %s\n\n", formatSize(metrics.TotalSize))
	}
	return nil
}
