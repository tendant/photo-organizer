package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// ============================================================================
// BACKUP COMMAND - Keep a second copy of a folder somewhere else
// ============================================================================
//
// backup mirrors the source tree into the destination: no timestamped folder,
// no moving, and repeated runs copy only what is missing. Use `archive` when
// you want a dated, immutable snapshot instead.

func runBackup(args []string) error {
	folderPath := ""
	destLocation := ""
	copyAll := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--all":
			copyAll = true
		case "--new-only":
			// Now the default; still accepted so old scripts keep working.
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
		fmt.Fprintf(os.Stderr, "\n📦 BACK UP A FOLDER\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup <folder> --dest <target> [--all]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest nas:/backups/photos\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest /Volumes/External/photos --all\n\n")
		fmt.Fprintf(os.Stderr, "Mirrors the folder into the destination. Repeated runs copy only what\n")
		fmt.Fprintf(os.Stderr, "is missing, so it is safe to run on a schedule.\n\n")
		fmt.Fprintf(os.Stderr, "Target: local path, or machine-id:/path or user@host:/path for SSH.\n")
		fmt.Fprintf(os.Stderr, "--all:  Copy every file, even ones already backed up on another machine.\n\n")
		fmt.Fprintf(os.Stderr, "For a dated snapshot that you keep untouched, use 'archive' instead.\n")
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
	var remoteUserHost, remoteMachineID, remotePath, localDest string
	remote := isRemoteDestination(destLocation)
	if remote {
		machines := loadMachinesConfig()
		remoteUserHost, remoteMachineID, remotePath, err = resolveBackupDestination(destLocation, machines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			printBackupDestinationOptions(machines)
			fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest nas:/backups/photos\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
			return errFailed
		}
	} else {
		localDest, err = filepath.Abs(destLocation)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid destination path %q\n", destLocation)
			return errFailed
		}
		if localDest == absSourceFolder {
			fmt.Fprintf(os.Stderr, "Error: destination is the source folder\n")
			return errFailed
		}
		if err := os.MkdirAll(localDest, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot create destination: %v\n", err)
			return errFailed
		}
	}

	selection := selectFilesToBackup(absSourceFolder, localDest, copyAll)

	if len(selection.relPaths) == 0 {
		fmt.Fprintf(os.Stderr, "✓ Nothing to copy — destination is up to date")
		if selection.backedUpElsewhere > 0 {
			fmt.Fprintf(os.Stderr, " (%d files already backed up elsewhere)", selection.backedUpElsewhere)
		}
		fmt.Fprintf(os.Stderr, "\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Backing up %d files (%s)...\n", len(selection.relPaths), formatSize(selection.totalSize))

	if remote {
		if err := rsyncRelPaths(absSourceFolder, selection.relPaths, remoteUserHost, remotePath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
			return errFailed
		}
	} else if err := copyRelPaths(absSourceFolder, selection.relPaths, localDest); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}

	printBackupSummary(selection, destLocation)

	if !remote {
		if err := refreshLocalManifest(localDest); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ Files were copied, but the destination manifest was not updated: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Backup complete\n")
		return nil
	}

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "⚠ Backup copied, but manifest refresh was skipped.\n")
		fmt.Fprintf(os.Stderr, "  Add this host first: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		return nil
	}

	if err := refreshRemoteManifest(remoteUserHost, remoteMachineID, remotePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}

	fmt.Fprintf(os.Stderr, "✓ Backup complete\n")
	return nil
}

// backupSelection is the outcome of deciding what a backup run needs to copy.
type backupSelection struct {
	relPaths          []string
	totalSize         int64
	backedUpElsewhere int // skipped: an independent copy already exists
	alreadyAtDest     int // skipped: same-size file already in the destination
}

// selectFilesToBackup walks the source folder and returns the files that still
// need copying. Unless copyAll is set, files that already have a copy on another
// durable machine are skipped. localDest is "" for remote destinations, where
// rsync does the already-present check itself.
func selectFilesToBackup(absSourceFolder, localDest string, copyAll bool) backupSelection {
	var selection backupSelection

	var sources []ManifestSource
	var idx map[string][]hashLocation
	machinesCfg := loadMachinesConfig()
	localMachineID := resolveMachineID("")
	if !copyAll {
		fmt.Fprintf(os.Stderr, "Checking which files need backup...\n")
		sources = loadManifestSources(localMachineID)
		idx = buildHashIndex(sources)
	}

	filepath.WalkDir(absSourceFolder, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == absSourceFolder || shouldSkipFile(path) {
			return nil
		}

		info, _ := os.Stat(path)
		if info == nil {
			return nil
		}

		relPath, err := filepath.Rel(absSourceFolder, path)
		if err != nil {
			return nil
		}

		// Already in the destination at the same size: nothing to do.
		if localDest != "" {
			if destInfo, err := os.Stat(filepath.Join(localDest, relPath)); err == nil && destInfo.Size() == info.Size() {
				selection.alreadyAtDest++
				return nil
			}
		}

		if !copyAll {
			partialHash, _ := processFile(path)
			if partialHash == "" {
				return nil
			}
			if hasIndependentBackup(sources, idx, machinesCfg, localMachineID, partialHash, info.Size()) {
				selection.backedUpElsewhere++
				return nil
			}
		}

		selection.relPaths = append(selection.relPaths, relPath)
		selection.totalSize += info.Size()
		return nil
	})

	return selection
}

// copyRelPaths copies source-relative paths into a local destination folder.
func copyRelPaths(srcRoot string, relPaths []string, destRoot string) error {
	for i, rel := range relPaths {
		destFile := filepath.Join(destRoot, rel)
		if err := os.MkdirAll(filepath.Dir(destFile), 0755); err != nil {
			return fmt.Errorf("cannot create directory for %s: %w", rel, err)
		}
		if err := copyFile(filepath.Join(srcRoot, rel), destFile); err != nil {
			return fmt.Errorf("cannot back up %s: %w", rel, err)
		}
		if (i+1)%100 == 0 {
			fmt.Fprintf(os.Stderr, "  %d / %d files copied...\n", i+1, len(relPaths))
		}
	}
	return nil
}

func printBackupSummary(selection backupSelection, destLocation string) {
	fmt.Fprintf(os.Stderr, "\nResults:\n")
	fmt.Fprintf(os.Stderr, "  Files copied:        %d (%s)\n", len(selection.relPaths), formatSize(selection.totalSize))
	if selection.alreadyAtDest > 0 {
		fmt.Fprintf(os.Stderr, "  Already at dest:     %d\n", selection.alreadyAtDest)
	}
	if selection.backedUpElsewhere > 0 {
		fmt.Fprintf(os.Stderr, "  Backed up elsewhere: %d\n", selection.backedUpElsewhere)
	}
	fmt.Fprintf(os.Stderr, "  Destination:         %s\n\n", destLocation)
}

// runBackupMissing is the former name of the mirroring backup; backup now does
// this by default.
func runBackupMissing(args []string) error {
	fmt.Fprintf(os.Stderr, "⚠ 'backup-missing' is deprecated — 'backup' now copies only missing files.\n")
	fmt.Fprintf(os.Stderr, "  Use: photo-organizer backup <folder> --dest <target>\n\n")
	return runBackup(args)
}
