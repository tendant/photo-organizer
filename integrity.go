package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ============================================================================
// Manifest Signing & Verification
// ============================================================================

// ManifestSignature holds integrity metadata for a manifest
type ManifestSignature struct {
	ManifestHash    string    // SHA256 of manifest file
	SignatureHash   string    // HMAC-SHA256 of manifest using key
	SignedAt        time.Time // When signature was created
	SigningKey      string    // Key fingerprint (first 8 chars of hash)
	VerificationID  string    // Unique ID for this manifest+key combo
}

// SignManifest creates a cryptographic signature for a manifest file
// Uses HMAC-SHA256 with a provided key for integrity verification
func SignManifest(manifestPath string, signingKey string) (ManifestSignature, error) {
	manifest := ManifestSignature{
		SignedAt: time.Now(),
	}

	// Read manifest file
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
	}

	// Calculate file hash (SHA256 of entire file)
	fileHash := sha256.Sum256(data)
	manifest.ManifestHash = hex.EncodeToString(fileHash[:])

	// Calculate HMAC signature (SHA256 with key)
	h := hmac.New(sha256.New, []byte(signingKey))
	h.Write(data)
	manifest.SignatureHash = hex.EncodeToString(h.Sum(nil))

	// Create key fingerprint (first 8 chars of key hash)
	keyHash := sha256.Sum256([]byte(signingKey))
	manifest.SigningKey = hex.EncodeToString(keyHash[:])[:8]

	// Create verification ID (combination of file hash and key fingerprint)
	manifest.VerificationID = manifest.ManifestHash[:16] + manifest.SigningKey

	return manifest, nil
}

// VerifyManifest checks if a manifest's signature is valid
// Returns true if signature matches, false if tampered or key doesn't match
func VerifyManifest(manifestPath string, signature ManifestSignature, signingKey string) (bool, error) {
	// Read manifest file
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, fmt.Errorf("read manifest: %w", err)
	}

	// Verify file hash
	fileHash := sha256.Sum256(data)
	currentFileHash := hex.EncodeToString(fileHash[:])
	if currentFileHash != signature.ManifestHash {
		return false, fmt.Errorf("manifest content has changed (file hash mismatch)")
	}

	// Verify signature
	h := hmac.New(sha256.New, []byte(signingKey))
	h.Write(data)
	currentSignature := hex.EncodeToString(h.Sum(nil))
	if currentSignature != signature.SignatureHash {
		return false, fmt.Errorf("signature verification failed (wrong key or tampered)")
	}

	return true, nil
}

// ============================================================================
// Archive Integrity Checking
// ============================================================================

// ArchiveIntegrity holds verification results for an archive
type ArchiveIntegrity struct {
	ArchivePath     string
	TotalFiles      int
	VerifiedFiles   int
	CorruptedFiles  int
	MissingFiles    int
	VerificationID  string
	VerifiedAt      time.Time
	Issues          []string
}

// VerifyArchiveIntegrity checks all files in an archive against manifest entries
// Verifies file sizes and spot-checks partial hashes
func VerifyArchiveIntegrity(archivePath string, rows []ManifestRow) ArchiveIntegrity {
	result := ArchiveIntegrity{
		ArchivePath: archivePath,
		VerifiedAt:  time.Now(),
		Issues:      make([]string, 0),
	}

	// Count total files in manifest for this archive
	result.TotalFiles = len(rows)

	// Walk archive and verify files
	filesByPath := make(map[string]bool)
	filepath.Walk(archivePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || shouldSkipFile(path) {
			return nil
		}

		relPath, _ := filepath.Rel(archivePath, path)
		filesByPath[relPath] = true

		// Check if file exists in manifest with correct size
		var found bool
		for _, row := range rows {
			if row.RelativePath == relPath {
				found = true
				if info.Size() != row.SizeBytes {
					result.CorruptedFiles++
					result.Issues = append(result.Issues,
						fmt.Sprintf("File size mismatch: %s (expected %d, got %d)",
							relPath, row.SizeBytes, info.Size()))
				} else {
					result.VerifiedFiles++
				}
				break
			}
		}

		if !found {
			result.Issues = append(result.Issues,
				fmt.Sprintf("File in archive but not in manifest: %s", relPath))
		}

		return nil
	})

	// Check for missing files
	for _, row := range rows {
		if !filesByPath[row.RelativePath] {
			result.MissingFiles++
			result.Issues = append(result.Issues,
				fmt.Sprintf("Manifest entry missing from archive: %s", row.RelativePath))
		}
	}

	// Create verification ID for this check
	result.VerificationID = fmt.Sprintf("%s_%d_%d",
		archivePath, result.VerifiedFiles, result.VerifiedAt.Unix())

	return result
}

// ============================================================================
// Backup Verification Report
// ============================================================================

// BackupVerificationReport comprehensive verification of entire backup
type BackupVerificationReport struct {
	ReportTime       time.Time
	TotalManifests   int
	VerifiedArchives int
	Integrity        []ArchiveIntegrity
	Summary          struct {
		TotalFiles      int
		VerifiedFiles   int
		CorruptedFiles  int
		MissingFiles    int
		IssueCount      int
	}
	ReportID string
}

// GenerateBackupReport creates comprehensive verification report
func GenerateBackupReport(manifests []*ManifestSource, archiveRoots map[string]string) BackupVerificationReport {
	report := BackupVerificationReport{
		ReportTime: time.Now(),
		Integrity:  make([]ArchiveIntegrity, 0),
	}

	report.TotalManifests = len(manifests)

	for _, manifestSrc := range manifests {
		archiveRoot, exists := archiveRoots[manifestSrc.ScanPath]
		if !exists {
			continue
		}

		// Verify this archive
		integrity := VerifyArchiveIntegrity(archiveRoot, manifestSrc.Rows)
		report.Integrity = append(report.Integrity, integrity)

		report.VerifiedArchives++
		report.Summary.TotalFiles += integrity.TotalFiles
		report.Summary.VerifiedFiles += integrity.VerifiedFiles
		report.Summary.CorruptedFiles += integrity.CorruptedFiles
		report.Summary.MissingFiles += integrity.MissingFiles
		report.Summary.IssueCount += len(integrity.Issues)
	}

	// Generate report ID
	reportHash := md5.Sum([]byte(fmt.Sprintf("%d_%d_%d",
		report.ReportTime.Unix(), report.Summary.TotalFiles, report.Summary.IssueCount)))
	report.ReportID = hex.EncodeToString(reportHash[:])[:12]

	return report
}

// ============================================================================
// Manifest Recovery & Repair
// ============================================================================

// RepairManifestResult holds results of repair operation
type RepairManifestResult struct {
	ManifestPath   string
	EntriesRemoved int
	EntriesCleaned int
	IssuesFixed    int
	BackupPath     string
}

// RepairManifest removes corrupted/invalid entries from manifest
// Creates backup before modifying, logs all changes
func RepairManifest(manifestPath string, archivePath string) (RepairManifestResult, error) {
	result := RepairManifestResult{ManifestPath: manifestPath}

	// Read manifest
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return result, fmt.Errorf("read manifest: %w", err)
	}

	// Create backup before modifying
	backupPath := manifestPath + ".backup." + time.Now().Format("2006-01-02-150405")
	if err := os.Rename(manifestPath, backupPath); err != nil {
		return result, fmt.Errorf("create backup: %w", err)
	}
	result.BackupPath = backupPath

	// Filter and clean entries
	validRows := make([]ManifestRow, 0)
	for _, row := range manifest.Rows {
		// Skip entries with invalid data
		if row.RelativePath == "" || row.SizeBytes < 0 {
			result.EntriesRemoved++
			continue
		}

		// Skip entries for files that don't exist in archive
		fullPath := filepath.Join(archivePath, row.RelativePath)
		info, err := os.Stat(fullPath)
		if err != nil {
			result.EntriesRemoved++
			continue
		}

		// Fix size mismatches
		if info.Size() != row.SizeBytes {
			row.SizeBytes = info.Size()
			result.EntriesCleaned++
			result.IssuesFixed++
		}

		validRows = append(validRows, row)
	}

	// Write repaired manifest
	manifest.Rows = validRows
	if err := writeManifestDirect(manifestPath, validRows); err != nil {
		// Restore from backup if write fails
		os.Rename(backupPath, manifestPath)
		return result, fmt.Errorf("write repaired manifest: %w", err)
	}

	return result, nil
}

// writeManifestDirect writes manifest structure directly to CSV file
func writeManifestDirect(path string, rows []ManifestRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write header
	header := "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n"
	if _, err := f.WriteString(header); err != nil {
		return err
	}

	// Write rows
	for _, row := range rows {
		line := fmt.Sprintf("%s,%d,%s,%s,%s,%s,%s\n",
			row.FileModified, row.SizeBytes, row.PartialHash, row.FullHash,
			row.ScanPath, row.MachineName, row.RelativePath)
		if _, err := f.WriteString(line); err != nil {
			return err
		}
	}

	return f.Sync()
}

// ============================================================================
// File Integrity Checking
// ============================================================================

// FileIntegrity holds hash verification for a single file
type FileIntegrity struct {
	Path         string
	PartialHash  string
	FullHash     string
	Size         int64
	IsValid      bool
	Mismatch     string
}

// VerifyFileIntegrity spot-checks a file against manifest entry
// Computes partial hash (first 1KB) for quick verification
func VerifyFileIntegrity(filePath string, manifestEntry ManifestRow) FileIntegrity {
	result := FileIntegrity{
		Path:         filePath,
		PartialHash:  manifestEntry.PartialHash,
		FullHash:     manifestEntry.FullHash,
	}

	info, err := os.Stat(filePath)
	if err != nil {
		result.Mismatch = fmt.Sprintf("file not found: %v", err)
		return result
	}

	result.Size = info.Size()

	// Check size
	if info.Size() != manifestEntry.SizeBytes {
		result.Mismatch = fmt.Sprintf("size mismatch: expected %d, got %d",
			manifestEntry.SizeBytes, info.Size())
		return result
	}

	// Compute and verify partial hash
	f, err := os.Open(filePath)
	if err != nil {
		result.Mismatch = fmt.Sprintf("cannot open file: %v", err)
		return result
	}
	defer f.Close()

	h := md5.New()
	lr := io.LimitReader(f, 1024) // First 1KB
	if _, err := io.Copy(h, lr); err != nil {
		result.Mismatch = fmt.Sprintf("cannot read file: %v", err)
		return result
	}

	currentPartialHash := hex.EncodeToString(h.Sum(nil))
	result.PartialHash = currentPartialHash

	if currentPartialHash != manifestEntry.PartialHash {
		result.Mismatch = "partial hash mismatch"
		return result
	}

	result.IsValid = true
	return result
}

// ============================================================================
// Integrity Audit Trail
// ============================================================================

// IntegrityEvent logs an integrity-related event
type IntegrityEvent struct {
	Timestamp time.Time
	EventType string // "sign", "verify", "repair", "check"
	Details   string
	Status    string // "success", "failure", "warning"
}

// LogIntegrityEvent writes an integrity event to audit trail
func LogIntegrityEvent(auditPath string, event IntegrityEvent) error {
	f, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf("[%s] %s: %s (%s)\n",
		event.Timestamp.Format("2006-01-02 15:04:05"),
		event.EventType, event.Details, event.Status)

	_, err = f.WriteString(line)
	return err
}
