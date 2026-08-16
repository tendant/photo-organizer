package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	progMode := progressAuto

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--all":
			copyAll = true
		case "--progress":
			progMode = progressOn
		case "--no-progress":
			progMode = progressOff
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

	showProgress := progressEnabled(progMode)
	runStart := time.Now()

	if folderPath == "" || destLocation == "" {
		fmt.Fprintf(os.Stderr, "\n📦 BACK UP A FOLDER\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup <folder> --dest <target> [--all] [--no-progress]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest nas:/backups/photos\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos --dest /Volumes/External/photos --all\n\n")
		fmt.Fprintf(os.Stderr, "Mirrors the folder into the destination. Repeated runs copy only what\n")
		fmt.Fprintf(os.Stderr, "is missing, so it is safe to run on a schedule.\n\n")
		fmt.Fprintf(os.Stderr, "Target: local path, or machine-id:/path or user@host:/path for SSH.\n")
		fmt.Fprintf(os.Stderr, "--all:  Copy every file, even ones already backed up on another machine.\n")
		fmt.Fprintf(os.Stderr, "--no-progress: Suppress the progress lines (also PHOTO_ORGANIZER_PROGRESS=0).\n\n")
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

	selection := selectFilesToBackup(absSourceFolder, localDest, copyAll, showProgress)

	if len(selection.relPaths) == 0 {
		fmt.Fprintf(os.Stderr, "✓ Nothing to copy — destination is up to date")
		if selection.backedUpElsewhere > 0 {
			fmt.Fprintf(os.Stderr, " (%d files already backed up elsewhere)", selection.backedUpElsewhere)
		}
		fmt.Fprintf(os.Stderr, "\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Backing up %d files (%s)...\n", len(selection.relPaths), formatSize(selection.totalSize))

	// For a remote destination the selection could not check the target, so ask
	// rsync what it will really send before denominating progress against it.
	var plan map[string]bool
	progTotal, progItems := selection.totalSize, int64(len(selection.relPaths))
	if remote {
		fmt.Fprintf(os.Stderr, "Checking what the destination already has...\n")
		if plan = rsyncPlanTransfers(absSourceFolder, selection.relPaths, remoteUserHost, remotePath); plan != nil {
			progTotal, progItems = 0, 0
			for i, rel := range selection.relPaths {
				if plan[rel] {
					progTotal += selection.relSizes[i]
					progItems++
				}
			}
			fmt.Fprintf(os.Stderr, "  %s of %s files need sending (%s)\n",
				formatCount(int(progItems)), formatCount(len(selection.relPaths)), formatSize(progTotal))
		}
	}

	prog := newProgressReporter("Copying", unitBytes, progTotal, progItems, showProgress)

	var copyErr error
	if remote {
		copyErr = rsyncRelPaths(absSourceFolder, selection.relPaths, selection.relSizes,
			remoteUserHost, remotePath, plan, prog)
		if copyErr != nil {
			// Clear first, so the error does not land on a half-drawn line.
			prog.Clear()
			fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", copyErr)
			return errFailed
		}
	} else if copyErr = copyRelPaths(absSourceFolder, selection.relPaths, localDest, prog); copyErr != nil {
		prog.Clear()
		fmt.Fprintf(os.Stderr, "Error: %v\n", copyErr)
		return errFailed
	}
	prog.Done(fmt.Sprintf("Copied %s files (%s)",
		formatCount(len(selection.relPaths)), formatSize(selection.totalSize)))

	printBackupSummary(selection, destLocation)

	if !remote {
		fmt.Fprintf(os.Stderr, "Updating destination manifest (%s)...\n", localDest)
		if err := refreshLocalManifest(localDest); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ Files were copied, but the destination manifest was not updated: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Backup complete in %s\n", formatDuration(time.Since(runStart)))
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

	fmt.Fprintf(os.Stderr, "✓ Backup complete in %s\n", formatDuration(time.Since(runStart)))
	return nil
}

// backupSelection is the outcome of deciding what a backup run needs to copy.
type backupSelection struct {
	relPaths          []string
	relSizes          []int64 // parallel to relPaths; sizes the copy is accounted in
	totalSize         int64
	backedUpElsewhere int // skipped: an independent copy already exists
	alreadyAtDest     int // skipped: same-size file already in the destination
}

// selectFilesToBackup walks the source folder and returns the files that still
// need copying. Unless copyAll is set, files that already have a copy on another
// durable machine are skipped. localDest is "" for remote destinations, where
// rsync does the already-present check itself.
func selectFilesToBackup(absSourceFolder, localDest string, copyAll, showProgress bool) backupSelection {
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

	// One cheap stat-only walk first, so the expensive per-file check below has
	// a denominator to report against. listFilesUnder applies the same filter
	// this loop used to, and a stat per file is nothing next to a 64KB read
	// plus MD5 plus EXIF.
	candidates, _, _ := listFilesUnder(absSourceFolder)

	// With --all there is no per-file hashing, so the walk IS the work and a
	// second pass would double it — no reporter in that case.
	prog := newProgressReporter("Checking", unitFiles, int64(len(candidates)), 0,
		showProgress && !copyAll)

	for _, relPath := range candidates {
		prog.Add(1, 1)

		path := filepath.Join(absSourceFolder, relPath)
		info, _ := os.Stat(path)
		if info == nil {
			continue
		}

		// Already in the destination at the same size: nothing to do.
		if localDest != "" {
			if destInfo, err := os.Stat(filepath.Join(localDest, relPath)); err == nil && destInfo.Size() == info.Size() {
				selection.alreadyAtDest++
				continue
			}
		}

		if !copyAll {
			partialHash, _ := processFile(path)
			if partialHash == "" {
				continue
			}
			if hasIndependentBackup(sources, idx, machinesCfg, localMachineID, partialHash, info.Size()) {
				selection.backedUpElsewhere++
				continue
			}
		}

		selection.relPaths = append(selection.relPaths, relPath)
		selection.relSizes = append(selection.relSizes, info.Size())
		selection.totalSize += info.Size()
	}
	prog.Done(fmt.Sprintf("Checked %s files", formatCount(len(candidates))))

	return selection
}

// copyRelPaths copies source-relative paths into a local destination folder.
// prog may be nil; bytes are credited inside copyFileProgress as they are
// written, so this only has to count files.
func copyRelPaths(srcRoot string, relPaths []string, destRoot string, prog *progressReporter) error {
	for _, rel := range relPaths {
		destFile := filepath.Join(destRoot, rel)
		if err := os.MkdirAll(filepath.Dir(destFile), 0755); err != nil {
			return fmt.Errorf("cannot create directory for %s: %w", rel, err)
		}
		if err := copyFileProgress(filepath.Join(srcRoot, rel), destFile, prog); err != nil {
			return fmt.Errorf("cannot back up %s: %w", rel, err)
		}
		prog.Add(0, 1)
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
