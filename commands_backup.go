package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// BACKUP COMMANDS - Core backup/restore functionality
// ============================================================================

// runBackup backs up a folder to timestamped archive
func runBackup(args []string) error {
	// Parse arguments
	folderPath := ""
	archiveRoot := ""
	newOnlyFlag := false

	for i := 0; i < len(args); i++ {
		if args[i] == "--new-only" {
			newOnlyFlag = true
		} else if folderPath == "" {
			folderPath = args[i]
		} else if archiveRoot == "" {
			archiveRoot = args[i]
		}
	}

	if folderPath == "" || archiveRoot == "" {
		fmt.Fprintf(os.Stderr, "\n📦 BACKUP FILES TO ARCHIVE\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup <folder> <archive-root> [--new-only]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos /mnt/archive\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos nas:/backups/photos\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/iPhone ubuntu@192.168.1.100:/backups --new-only\n\n")
		fmt.Fprintf(os.Stderr, "Creates timestamped archive folder and copies files.\n")
		fmt.Fprintf(os.Stderr, "Archive root may be a local path or a remote machine-id:/path or user@host:/path.\n")
		fmt.Fprintf(os.Stderr, "--new-only: Only backup files not already backed up elsewhere.\n")
		return errFailed
	}

	// A remote archive root goes over SSH; a local one is a plain directory.
	var remoteUserHost, remoteMachineID, remotePath string
	remote := isRemoteDestination(archiveRoot)
	if remote {
		machines := loadMachinesConfig()
		var err error
		remoteUserHost, remoteMachineID, remotePath, err = resolveBackupDestination(archiveRoot, machines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			printBackupDestinationOptions(machines)
			fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos nas:/backups/photos\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos ubuntu@192.168.1.100:/backups\n")
			return errFailed
		}
	} else if _, err := os.Stat(archiveRoot); err != nil {
		// Verify archive root exists or create it
		if os.IsNotExist(err) {
			if err := os.MkdirAll(archiveRoot, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Cannot create archive root: %v\n", err)
				return errFailed
			}
			fmt.Printf("📁 Created archive root: %s\n\n", archiveRoot)
		} else {
			fmt.Fprintf(os.Stderr, "❌ Cannot access archive root: %v\n", err)
			return errFailed
		}
	}

	// Resolve to absolute path
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot resolve folder path: %v\n", err)
		return errFailed
	}

	// Verify folder exists
	if _, err := os.Stat(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Folder not found: %s\n", absPath)
		return errFailed
	}

	manifest, found := findManifestForScanPath(absPath)
	if !found {
		fmt.Fprintf(os.Stderr, "❌ No manifest found for %s\n", absPath)
		fmt.Fprintf(os.Stderr, "   Run 'photo-organizer scan %s' first\n", absPath)
		return errFailed
	}

	if len(manifest.Rows) == 0 {
		fmt.Fprintf(os.Stderr, "❌ Manifest is empty\n")
		return errFailed
	}

	fmt.Printf("📦 BACKUP WORKFLOW\n\n")
	fmt.Printf("Source:  %s (%d files)\n", manifest.ScanPath, len(manifest.Rows))
	fmt.Printf("Machine: %s\n", manifest.MachineName)
	fmt.Printf("Archive: %s\n", archiveRoot)
	if newOnlyFlag {
		fmt.Printf("Mode:    Only backup files not already backed up\n")
	}
	fmt.Printf("\n")

	// Select the rows this run will copy; --new-only drops files that already
	// have a copy on another durable machine.
	var rows []ManifestRow
	filesSkipped := 0
	if newOnlyFlag {
		sources := loadManifestSources(manifest.MachineName)
		idx := buildHashIndex(sources)
		machinesCfg := loadMachinesConfig()
		for _, row := range manifest.Rows {
			if hasIndependentBackup(sources, idx, machinesCfg, manifest.MachineName, row.PartialHash, row.SizeBytes) {
				filesSkipped++
				continue
			}
			rows = append(rows, row)
		}
	} else {
		rows = manifest.Rows
	}

	// Create timestamped archive folder
	timestamp := time.Now()
	archiveFolderName := generateArchiveFolderName(filepath.Base(manifest.ScanPath), timestamp)

	if remote {
		return backupToRemoteArchive(manifest, rows, filesSkipped, archiveFolderName, remoteUserHost, remoteMachineID, remotePath)
	}
	return backupToLocalArchive(manifest, rows, filesSkipped, filepath.Join(archiveRoot, archiveFolderName), archiveFolderName)
}

// backupToLocalArchive copies the selected rows into a local timestamped folder.
func backupToLocalArchive(manifest ManifestSource, rows []ManifestRow, filesSkipped int, archivePath, archiveFolderName string) error {
	if err := os.Mkdir(archivePath, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot create archive folder: %v\n", err)
		return errFailed
	}

	fmt.Printf("Archive folder: %s\n\n", archiveFolderName)

	filesToBackup := 0
	filesFailed := 0

	// Backup each file
	fmt.Printf("Backing up files...\n")
	for i, row := range rows {
		sourceFile := filepath.Join(manifest.ScanPath, row.RelativePath)
		destFile := filepath.Join(archivePath, row.RelativePath)

		// Create destination directory if needed
		destDir := filepath.Dir(destFile)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot create directory for %s: %v\n", row.RelativePath, err)
			filesFailed++
			continue
		}

		// Copy file
		if err := copyFile(sourceFile, destFile); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot backup %s: %v\n", row.RelativePath, err)
			filesFailed++
			continue
		}

		filesToBackup++

		// Progress
		if (i+1)%100 == 0 {
			fmt.Printf("  %d / %d files backed up...\n", i+1, len(rows))
		}
	}

	printBackupSummary(filesToBackup, filesSkipped, filesFailed, archivePath,
		formatSize(calculateFolderMetrics(archivePath).TotalSize))
	return nil
}

// backupToRemoteArchive rsyncs the selected rows into a timestamped folder on a
// remote machine, then refreshes that machine's manifest so the copy is visible
// to dedup and coverage checks.
func backupToRemoteArchive(manifest ManifestSource, rows []ManifestRow, filesSkipped int, archiveFolderName, remoteUserHost, remoteMachineID, remotePath string) error {
	remoteArchivePath := strings.TrimSuffix(remotePath, "/") + "/" + archiveFolderName
	fmt.Printf("Archive folder: %s\n\n", archiveFolderName)

	relPaths := make([]string, 0, len(rows))
	var totalSize int64
	for _, row := range rows {
		relPaths = append(relPaths, row.RelativePath)
		totalSize += row.SizeBytes
	}

	if len(relPaths) == 0 {
		printBackupSummary(0, filesSkipped, 0, remoteUserHost+":"+remoteArchivePath, formatSize(0))
		return nil
	}

	fmt.Printf("Copying %d files (%s) to %s...\n", len(relPaths), formatSize(totalSize), remoteUserHost)
	if err := rsyncRelPaths(manifest.ScanPath, relPaths, remoteUserHost, remoteArchivePath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ rsync failed: %v\n", err)
		return errFailed
	}

	printBackupSummary(len(relPaths), filesSkipped, 0, remoteUserHost+":"+remoteArchivePath, formatSize(totalSize))

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "⚠ Backup copied, but manifest refresh was skipped.\n")
		fmt.Fprintf(os.Stderr, "  Add this host first: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		return nil
	}

	if err := refreshRemoteManifest(remoteUserHost, remoteMachineID, remoteArchivePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}
	return nil
}

func printBackupSummary(filesToBackup, filesSkipped, filesFailed int, location, size string) {
	fmt.Printf("\n✓ BACKUP COMPLETE\n\n")
	fmt.Printf("Results:\n")
	fmt.Printf("  Files backed up: %d\n", filesToBackup)
	fmt.Printf("  Files skipped:   %d\n", filesSkipped)
	if filesFailed > 0 {
		fmt.Printf("  Files failed:    %d\n", filesFailed)
	}
	fmt.Printf("  Archive folder:  %s\n", location)
	fmt.Printf("  Archive size:    %s\n\n", size)

	switch {
	case filesFailed > 0:
		fmt.Printf("⚠️  Some files could not be backed up. Check permissions and disk space.\n")
	case filesToBackup == 0 && filesSkipped > 0:
		fmt.Printf("✓ Nothing to copy — every file is already backed up elsewhere.\n")
	default:
		fmt.Printf("✓ All files successfully backed up!\n")
	}
}

// runRestore restores files from archive to destination
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

// runListArchives shows all available backups
func runListArchives(args []string) error {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n📋 LIST AVAILABLE ARCHIVES\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer list-archives <archive-root>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer list-archives /mnt/archive\n\n")
		fmt.Fprintf(os.Stderr, "Shows all timestamped backup folders with their contents.\n")
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

// copyFile copies a single file from source to destination
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Copy file contents
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	// Preserve permissions
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}

// =============================================================================
// Backup Missing Command
// =============================================================================

func runBackupMissing(args []string) error {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup-missing <folder> --dest <target>\n\n")
		fmt.Fprintf(os.Stderr, "Ensures all files in folder are backed up to target destination.\n")
		fmt.Fprintf(os.Stderr, "Only copies missing files via rsync.\n\n")
		fmt.Fprintf(os.Stderr, "Target formats: machine-id:/path or user@host:/path\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		return errFailed
	}

	sourceFolder := args[0]
	destLocation := parseRequiredDestFlag(args)

	if destLocation == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required (e.g., user@host:/backups)\n")
		return errFailed
	}

	// Resolve source folder to absolute path
	absSourceFolder, err := resolveExistingFolder(sourceFolder)
	if err != nil {
		if _, statErr := os.Stat(sourceFolder); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		}
		return errFailed
	}

	fmt.Fprintf(os.Stderr, "Checking which files need backup...\n")

	// Get local machine ID to distinguish local from remote backups
	localMachineID := resolveMachineID("")

	// Load all manifests
	sources := loadManifestSources(localMachineID)

	// Build hash index
	idx := buildHashIndex(sources)

	// Config distinguishes durable machines from removable media
	machinesCfg := loadMachinesConfig()

	// Find missing files (files without non-removable backups)
	var missingFiles []string
	var missingCount int
	var missingSize int64

	filepath.WalkDir(absSourceFolder, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == absSourceFolder {
			return nil
		}

		info, _ := os.Stat(path)
		if info == nil {
			return nil
		}

		// Skip unwanted system/sync files
		if shouldSkipFile(path) {
			return nil
		}

		// Compute partial hash
		partialHash, _ := processFile(path)
		if partialHash == "" {
			return nil
		}

		if !hasIndependentBackup(sources, idx, machinesCfg, localMachineID, partialHash, info.Size()) {
			// This file needs backing up
			relPath, _ := filepath.Rel(absSourceFolder, path)
			missingFiles = append(missingFiles, relPath)
			missingCount++
			missingSize += info.Size()
		}

		return nil
	})

	if missingCount == 0 {
		fmt.Fprintf(os.Stderr, "✓ All files already backed up\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Backing up %d files (%.1f GB)...\n", missingCount, float64(missingSize)/(1024*1024*1024))

	// Look up remote machine config
	machines := loadMachinesConfig()
	remoteUserHost, remoteMachineID, remotePath, err := resolveBackupDestination(destLocation, machines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printBackupDestinationOptions(machines)
		fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max-acb605:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		return errFailed
	}

	// Copy files via rsync
	if err := rsyncRelPaths(absSourceFolder, missingFiles, remoteUserHost, remotePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
		return errFailed
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
