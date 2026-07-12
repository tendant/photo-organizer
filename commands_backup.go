package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// BACKUP COMMANDS - Core backup/restore functionality
// ============================================================================

// runBackup backs up a folder to timestamped archive
func runBackup(args []string) {
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
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos /mnt/archive\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/iPhone /mnt/archive --new-only\n\n")
		fmt.Fprintf(os.Stderr, "Creates timestamped archive folder and copies files.\n")
		fmt.Fprintf(os.Stderr, "--new-only: Only backup files not already backed up elsewhere.\n")
		os.Exit(1)
	}

	// Verify archive root exists or create it
	if _, err := os.Stat(archiveRoot); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(archiveRoot, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Cannot create archive root: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("📁 Created archive root: %s\n\n", archiveRoot)
		} else {
			fmt.Fprintf(os.Stderr, "❌ Cannot access archive root: %v\n", err)
			os.Exit(1)
		}
	}

	// Resolve to absolute path
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot resolve folder path: %v\n", err)
		os.Exit(1)
	}

	// Verify folder exists
	if _, err := os.Stat(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Folder not found: %s\n", absPath)
		os.Exit(1)
	}

	manifest, found := findManifestForScanPath(absPath)
	if !found {
		fmt.Fprintf(os.Stderr, "❌ No manifest found for %s\n", absPath)
		fmt.Fprintf(os.Stderr, "   Run 'photo-organizer scan %s' first\n", absPath)
		os.Exit(1)
	}

	if len(manifest.Rows) == 0 {
		fmt.Fprintf(os.Stderr, "❌ Manifest is empty\n")
		os.Exit(1)
	}

	fmt.Printf("📦 BACKUP WORKFLOW\n\n")
	fmt.Printf("Source:  %s (%d files)\n", manifest.ScanPath, len(manifest.Rows))
	fmt.Printf("Machine: %s\n", manifest.MachineName)
	fmt.Printf("Archive: %s\n", archiveRoot)
	if newOnlyFlag {
		fmt.Printf("Mode:    Only backup files not already backed up\n")
	}
	fmt.Printf("\n")

	var idx map[string][]hashLocation
	var sources []ManifestSource
	if newOnlyFlag {
		sources = loadManifestSources(manifest.MachineName)
		idx = buildHashIndex(sources)
	}

	// Create timestamped archive folder
	timestamp := time.Now()
	archiveFolderName := generateArchiveFolderName(filepath.Base(manifest.ScanPath), timestamp)
	archivePath := filepath.Join(archiveRoot, archiveFolderName)

	if err := os.Mkdir(archivePath, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot create archive folder: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Archive folder: %s\n\n", archiveFolderName)

	// Count files to backup
	filesToBackup := 0
	filesSkipped := 0

	// Backup each file
	fmt.Printf("Backing up files...\n")
	machinesCfg := loadMachinesConfig()
	for i, row := range manifest.Rows {
		if newOnlyFlag && hasIndependentBackup(sources, idx, machinesCfg, manifest.MachineName, row.PartialHash, row.SizeBytes) {
			filesSkipped++
			continue
		}

		sourceFile := filepath.Join(manifest.ScanPath, row.RelativePath)
		destFile := filepath.Join(archivePath, row.RelativePath)

		// Create destination directory if needed
		destDir := filepath.Dir(destFile)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot create directory for %s: %v\n", row.RelativePath, err)
			filesSkipped++
			continue
		}

		// Copy file
		if err := copyFile(sourceFile, destFile); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot backup %s: %v\n", row.RelativePath, err)
			filesSkipped++
			continue
		}

		filesToBackup++

		// Progress
		if (i+1)%100 == 0 {
			fmt.Printf("  %d / %d files backed up...\n", i+1, len(manifest.Rows))
		}
	}

	fmt.Printf("\n✓ BACKUP COMPLETE\n\n")
	fmt.Printf("Results:\n")
	fmt.Printf("  Files backed up: %d\n", filesToBackup)
	fmt.Printf("  Files skipped:   %d\n", filesSkipped)
	fmt.Printf("  Archive folder:  %s\n", archivePath)
	fmt.Printf("  Archive size:    %s\n\n", formatSize(calculateFolderMetrics(archivePath).TotalSize))

	if filesSkipped > 0 {
		fmt.Printf("⚠️  Some files could not be backed up. Check permissions and disk space.\n")
	} else {
		fmt.Printf("✓ All files successfully backed up!\n")
	}
}

// runRestore restores files from archive to destination
func runRestore(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "\n📥 RESTORE FILES FROM ARCHIVE\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer restore <archive-path> <destination>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer restore /mnt/archive/2026-06-20-143022-Photos ~/Restored\n\n")
		fmt.Fprintf(os.Stderr, "Restores all files from archive to destination folder.\n")
		os.Exit(1)
	}

	archivePath := args[0]
	destination := args[1]

	// Verify archive exists
	if _, err := os.Stat(archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Archive not found: %s\n", archivePath)
		os.Exit(1)
	}

	// Create destination if needed
	if err := os.MkdirAll(destination, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot create destination: %v\n", err)
		os.Exit(1)
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
}

// runListArchives shows all available backups
func runListArchives(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n📋 LIST AVAILABLE ARCHIVES\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer list-archives <archive-root>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer list-archives /mnt/archive\n\n")
		fmt.Fprintf(os.Stderr, "Shows all timestamped backup folders with their contents.\n")
		os.Exit(1)
	}

	archiveRoot := args[0]

	// Verify archive root exists
	if _, err := os.Stat(archiveRoot); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Archive root not found: %s\n", archiveRoot)
		os.Exit(1)
	}

	fmt.Printf("📋 AVAILABLE ARCHIVES\n\n")
	fmt.Printf("Location: %s\n\n", archiveRoot)

	// List all archive folders
	entries, err := os.ReadDir(archiveRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot read archive root: %v\n", err)
		os.Exit(1)
	}

	archives := make([]os.DirEntry, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			archives = append(archives, entry)
		}
	}

	if len(archives) == 0 {
		fmt.Printf("No archives found.\n")
		return
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

func runBackupMissing(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup-missing <folder> --dest <target>\n\n")
		fmt.Fprintf(os.Stderr, "Ensures all files in folder are backed up to target destination.\n")
		fmt.Fprintf(os.Stderr, "Only copies missing files via rsync.\n\n")
		fmt.Fprintf(os.Stderr, "Target formats: machine-id:/path or user@host:/path\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		os.Exit(1)
	}

	sourceFolder := args[0]
	destLocation := parseRequiredDestFlag(args)

	if destLocation == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required (e.g., user@host:/backups)\n")
		os.Exit(1)
	}

	// Resolve source folder to absolute path
	absSourceFolder, err := resolveExistingFolder(sourceFolder)
	if err != nil {
		if _, statErr := os.Stat(sourceFolder); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		}
		os.Exit(1)
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
		return
	}

	fmt.Fprintf(os.Stderr, "Backing up %d files (%.1f GB)...\n", missingCount, float64(missingSize)/(1024*1024*1024))

	// Look up remote machine config
	machines := loadMachinesConfig()
	remoteUserHost, remoteMachineID, remotePath, err := resolveBackupDestination(destLocation, machines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Available options:\n")
		for machID, sshHost := range machines {
			if !strings.HasPrefix(sshHost, "[removable]") {
				fmt.Fprintf(os.Stderr, "  - %s (machine-id: %s)\n", sshHost, machID)
			}
		}
		fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max-acb605:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		os.Exit(1)
	}

	// Step 2: Create temporary file list for rsync
	tmpFile, err := os.CreateTemp("", "backup-missing-*.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmpFile.Name())

	for _, f := range missingFiles {
		fmt.Fprintln(tmpFile, f)
	}
	tmpFile.Close()

	// Copy files via rsync
	rsyncCmd := exec.Command("rsync", "-az", "--files-from="+tmpFile.Name(), absSourceFolder+"/", remoteUserHost+":"+remotePath+"/")
	rsyncCmd.Stdout = os.Stderr
	rsyncCmd.Stderr = os.Stderr
	if err := rsyncCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
		os.Exit(1)
	}

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "⚠ Backup copied, but manifest refresh was skipped.\n")
		fmt.Fprintf(os.Stderr, "  Add this host first: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		return
	}

	// Scan remote location
	scanCmd := fmt.Sprintf("cd %s && for path in photo-organizer ~/bin/photo-organizer /usr/local/bin/photo-organizer; do if command -v $path &>/dev/null || [ -f $path ]; then $path scan . --machine %s >/dev/null 2>&1; exit $?; fi; done; exit 1", shellQuote(remotePath), shellQuote(remoteMachineID))
	sshCmd := exec.Command("ssh", remoteUserHost, scanCmd)
	if err := sshCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: remote scan failed after copying files. photo-organizer must be installed on remote machine.\n")
		os.Exit(1)
	}

	// Collect updated manifests
	collectCmd := exec.Command("photo-organizer", "collect", "--from", remoteMachineID)
	collectCmd.Stdout = os.Stderr
	collectCmd.Stderr = os.Stderr
	if err := collectCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: files were copied, but manifest collection failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "✓ Backup complete\n")
}
