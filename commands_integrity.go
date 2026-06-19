package main

import (
	"fmt"
	"os"
)

// ============================================================================
// INTEGRITY COMMANDS
// ============================================================================

// runSignManifest signs a manifest for integrity verification
func runSignManifest(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n🔐 SIGN MANIFEST FOR INTEGRITY VERIFICATION\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer sign-manifest <manifest-path> --key <secret-key>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer sign-manifest ~/manifests/photos.csv --key \"my-secret-key\"\n\n")
		fmt.Fprintf(os.Stderr, "This creates a cryptographic signature (HMAC-SHA256) to prevent tampering.\n")
		os.Exit(1)
	}

	manifestPath := args[0]

	// Find --key flag
	var key string
	for i := 1; i < len(args); i++ {
		if args[i] == "--key" && i+1 < len(args) {
			key = args[i+1]
			break
		}
	}

	if key == "" {
		fmt.Fprintf(os.Stderr, "❌ --key flag required\n")
		os.Exit(1)
	}

	// Sign the manifest
	sig, err := SignManifest(manifestPath, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot sign manifest: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Manifest signed successfully\n\n")
	fmt.Printf("Signature Details:\n")
	fmt.Printf("  File Hash:       %s\n", sig.ManifestHash[:32])
	fmt.Printf("  Signature Hash:  %s\n", sig.SignatureHash[:32])
	fmt.Printf("  Signing Key:     %s (fingerprint)\n", sig.SigningKey)
	fmt.Printf("  Verification ID: %s\n", sig.VerificationID)
	fmt.Printf("  Signed At:       %s\n\n", sig.SignedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("💡 Save this information to verify the manifest later.\n")
	fmt.Printf("   Use: photo-organizer verify-backup <manifest-path> --sig-hash <hash> --key <key>\n")
}

// runVerifyBackup verifies backup integrity
func runVerifyBackup(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n✓ VERIFY BACKUP INTEGRITY\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer verify-backup <manifest-path>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer verify-backup ~/manifests/photos.csv\n\n")
		fmt.Fprintf(os.Stderr, "This checks that all files in the archive match the manifest.\n")
		os.Exit(1)
	}

	manifestPath := args[0]

	// Read manifest
	manifest, err := readManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot read manifest: %v\n", err)
		os.Exit(1)
	}

	// Get archive path from first manifest entry
	if len(manifest.Rows) == 0 {
		fmt.Fprintf(os.Stderr, "❌ Manifest is empty\n")
		os.Exit(1)
	}

	archivePath := manifest.ScanPath
	if archivePath == "" {
		fmt.Fprintf(os.Stderr, "❌ Cannot determine archive path from manifest\n")
		os.Exit(1)
	}

	// Verify archive integrity
	result := VerifyArchiveIntegrity(archivePath, manifest.Rows)

	// Display results
	fmt.Printf("✓ BACKUP VERIFICATION REPORT\n\n")
	fmt.Printf("Archive: %s\n\n", result.ArchivePath)
	fmt.Printf("Summary:\n")
	fmt.Printf("  Total Files:     %d\n", result.TotalFiles)
	fmt.Printf("  Verified:        %d ✓\n", result.VerifiedFiles)
	fmt.Printf("  Corrupted:       %d ✗\n", result.CorruptedFiles)
	fmt.Printf("  Missing:         %d ✗\n", result.MissingFiles)
	fmt.Printf("  Verified At:     %s\n\n", result.VerifiedAt.Format("2006-01-02 15:04:05"))

	// Show issues if any
	if len(result.Issues) > 0 {
		fmt.Printf("⚠️  Issues Found:\n")
		for i, issue := range result.Issues {
			if i >= 10 { // Show first 10 issues
				fmt.Printf("  ... and %d more issues\n", len(result.Issues)-10)
				break
			}
			fmt.Printf("  - %s\n", issue)
		}
	}

	// Overall status
	if result.CorruptedFiles == 0 && result.MissingFiles == 0 {
		fmt.Printf("\n✓ Backup is healthy and ready for use\n")
		os.Exit(0)
	} else {
		fmt.Printf("\n❌ Backup has issues. Use 'repair-manifest' to fix.\n")
		os.Exit(1)
	}
}

// runRepairManifest repairs a corrupted manifest
func runRepairManifest(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "\n🔧 REPAIR CORRUPTED MANIFEST\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer repair-manifest <manifest-path> <archive-path>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer repair-manifest ~/manifests/photos.csv /mnt/archive/2026-06-20-143022-Photos\n\n")
		fmt.Fprintf(os.Stderr, "This removes entries for missing/corrupted files and fixes issues.\n")
		fmt.Fprintf(os.Stderr, "A backup is created before modifications.\n")
		os.Exit(1)
	}

	manifestPath := args[0]
	archivePath := args[1]

	// Verify archive path exists
	if _, err := os.Stat(archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Archive path not found: %s\n", archivePath)
		os.Exit(1)
	}

	fmt.Printf("🔧 Repairing manifest...\n\n")

	// Repair the manifest
	result, err := RepairManifest(manifestPath, archivePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot repair manifest: %v\n", err)
		os.Exit(1)
	}

	// Display results
	fmt.Printf("✓ Repair Complete\n\n")
	fmt.Printf("Actions Taken:\n")
	fmt.Printf("  Entries Removed:  %d (missing files, invalid data)\n", result.EntriesRemoved)
	fmt.Printf("  Entries Fixed:    %d (size mismatches, etc.)\n", result.EntriesCleaned)
	fmt.Printf("  Total Issues:     %d\n\n", result.IssuesFixed)
	fmt.Printf("Backup Created: %s\n", result.BackupPath)
	fmt.Printf("Repaired File:  %s\n\n", manifestPath)
	fmt.Printf("✓ Manifest is now repaired and ready to use\n")
}
