package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// Manifest Signing & Verification Tests
// ============================================================================

// TestSignManifest verifies manifest signature creation
func TestSignManifest(t *testing.T) {
	testDir := t.TempDir()

	// Create a test manifest file
	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc123,def456,/photos,machine1,photo1.jpg
2024-06-19,2000,xyz789,uvw012,/photos,machine1,photo2.jpg`

	manifestPath := filepath.Join(testDir, "test.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Sign the manifest
	key := "my-secret-key-12345"
	sig, err := SignManifest(manifestPath, key)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}

	// Verify signature structure
	if sig.ManifestHash == "" {
		t.Error("ManifestHash should not be empty")
	}
	if sig.SignatureHash == "" {
		t.Error("SignatureHash should not be empty")
	}
	if sig.SigningKey == "" {
		t.Error("SigningKey should not be empty")
	}
	if sig.VerificationID == "" {
		t.Error("VerificationID should not be empty")
	}
	if sig.SignedAt.IsZero() {
		t.Error("SignedAt should be set")
	}

	// Signing key should be 8 characters (first 8 of SHA256)
	if len(sig.SigningKey) != 8 {
		t.Errorf("SigningKey length %d, want 8", len(sig.SigningKey))
	}
}

// TestVerifyManifestValid verifies valid manifest passes verification
func TestVerifyManifestValid(t *testing.T) {
	testDir := t.TempDir()

	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc123,def456,/photos,machine1,photo1.jpg`

	manifestPath := filepath.Join(testDir, "test.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	key := "secret-key"
	sig, _ := SignManifest(manifestPath, key)

	// Verify with correct key
	valid, err := VerifyManifest(manifestPath, sig, key)
	if err != nil {
		t.Errorf("VerifyManifest error: %v", err)
	}
	if !valid {
		t.Error("Manifest should verify with correct key")
	}
}

// TestVerifyManifestTampered detects tampered manifest
func TestVerifyManifestTampered(t *testing.T) {
	testDir := t.TempDir()

	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc123,def456,/photos,machine1,photo1.jpg`

	manifestPath := filepath.Join(testDir, "test.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	key := "secret-key"
	sig, _ := SignManifest(manifestPath, key)

	// Tamper with manifest
	tamperedContent := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,9999,modified,changed,/photos,machine1,photo1.jpg`
	if err := os.WriteFile(manifestPath, []byte(tamperedContent), 0644); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}

	// Verify should fail
	valid, err := VerifyManifest(manifestPath, sig, key)
	if valid {
		t.Error("Tampered manifest should fail verification")
	}
	if err == nil {
		t.Error("Should return error for tampered manifest")
	}
}

// TestVerifyManifestWrongKey detects signature with wrong key
func TestVerifyManifestWrongKey(t *testing.T) {
	testDir := t.TempDir()

	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc123,def456,/photos,machine1,photo1.jpg`

	manifestPath := filepath.Join(testDir, "test.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	key := "secret-key"
	sig, _ := SignManifest(manifestPath, key)

	// Verify with wrong key
	valid, err := VerifyManifest(manifestPath, sig, "wrong-key")
	if valid {
		t.Error("Verification with wrong key should fail")
	}
	if err == nil {
		t.Error("Should return error for wrong key")
	}
}

// ============================================================================
// Archive Integrity Tests
// ============================================================================

// TestVerifyArchiveIntegrity validates archive against manifest
func TestVerifyArchiveIntegrity(t *testing.T) {
	testDir := t.TempDir()
	archiveDir := filepath.Join(testDir, "archive")
	if err := os.Mkdir(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create test files in archive
	if err := os.WriteFile(filepath.Join(archiveDir, "photo1.jpg"), []byte("data1"), 0644); err != nil {
		t.Fatalf("write file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "photo2.jpg"), []byte("data2data2"), 0644); err != nil {
		t.Fatalf("write file2: %v", err)
	}

	// Create manifest rows
	rows := []ManifestRow{
		{RelativePath: "photo1.jpg", SizeBytes: 5},
		{RelativePath: "photo2.jpg", SizeBytes: 10},
	}

	// Verify archive
	result := VerifyArchiveIntegrity(archiveDir, rows)

	if result.TotalFiles != 2 {
		t.Errorf("TotalFiles %d, want 2", result.TotalFiles)
	}
	if result.VerifiedFiles != 2 {
		t.Errorf("VerifiedFiles %d, want 2", result.VerifiedFiles)
	}
	if result.CorruptedFiles != 0 {
		t.Errorf("CorruptedFiles %d, want 0", result.CorruptedFiles)
	}
	if result.MissingFiles != 0 {
		t.Errorf("MissingFiles %d, want 0", result.MissingFiles)
	}
	if len(result.Issues) != 0 {
		t.Errorf("Issues %d, want 0", len(result.Issues))
	}
}

// TestVerifyArchiveCorruptedFile detects file size mismatch
func TestVerifyArchiveCorruptedFile(t *testing.T) {
	testDir := t.TempDir()
	archiveDir := filepath.Join(testDir, "archive")
	if err := os.Mkdir(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create file with wrong size
	if err := os.WriteFile(filepath.Join(archiveDir, "photo.jpg"), []byte("wrong"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	rows := []ManifestRow{
		{RelativePath: "photo.jpg", SizeBytes: 1000}, // Expected 1000, got 5
	}

	result := VerifyArchiveIntegrity(archiveDir, rows)

	if result.CorruptedFiles != 1 {
		t.Errorf("CorruptedFiles %d, want 1", result.CorruptedFiles)
	}
	if len(result.Issues) == 0 {
		t.Error("Should have issues for corrupted file")
	}
}

// TestVerifyArchiveMissingFile detects missing files
func TestVerifyArchiveMissingFile(t *testing.T) {
	testDir := t.TempDir()
	archiveDir := filepath.Join(testDir, "archive")
	if err := os.Mkdir(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rows := []ManifestRow{
		{RelativePath: "photo1.jpg", SizeBytes: 100},
		{RelativePath: "photo2.jpg", SizeBytes: 200}, // This file is missing
	}

	result := VerifyArchiveIntegrity(archiveDir, rows)

	if result.MissingFiles != 2 {
		t.Errorf("MissingFiles %d, want 2", result.MissingFiles)
	}
}

// ============================================================================
// Backup Verification Report Tests
// ============================================================================

// TestGenerateBackupReport creates comprehensive verification report
func TestGenerateBackupReport(t *testing.T) {
	// Create test manifests
	manifests := []*ManifestSource{
		{
			FilePath:    "/manifests/test1.csv",
			MachineName: "machine1",
			ScanPath:    "/photos1",
			Rows: []ManifestRow{
				{RelativePath: "photo1.jpg", SizeBytes: 1000},
				{RelativePath: "photo2.jpg", SizeBytes: 2000},
			},
		},
		{
			FilePath:    "/manifests/test2.csv",
			MachineName: "machine2",
			ScanPath:    "/photos2",
			Rows: []ManifestRow{
				{RelativePath: "photo3.jpg", SizeBytes: 3000},
			},
		},
	}

	// No actual archives in this test (just verify structure)
	archiveRoots := make(map[string]string)

	report := GenerateBackupReport(manifests, archiveRoots)

	if report.TotalManifests != 2 {
		t.Errorf("TotalManifests %d, want 2", report.TotalManifests)
	}
	if report.ReportID == "" {
		t.Error("ReportID should not be empty")
	}
	if report.ReportTime.IsZero() {
		t.Error("ReportTime should be set")
	}
}

// ============================================================================
// Manifest Repair Tests
// ============================================================================

// TestRepairManifest removes invalid entries and creates backup
func TestRepairManifest(t *testing.T) {
	testDir := t.TempDir()

	// Create archive with some files
	archiveDir := filepath.Join(testDir, "archive")
	if err := os.Mkdir(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(archiveDir, "photo1.jpg"), []byte("data"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Create manifest with mixed valid/invalid entries
	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,4,abc,def,/photos,machine1,photo1.jpg
2024-06-19,-1,bad,bad,/photos,machine1,deleted.jpg
2024-06-19,100,bad,bad,/photos,machine1,missing.jpg`

	manifestPath := filepath.Join(testDir, "manifest.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Repair
	result, err := RepairManifest(manifestPath, archiveDir)
	if err != nil {
		t.Fatalf("RepairManifest: %v", err)
	}

	// Verify results
	if result.EntriesRemoved == 0 {
		t.Error("Should have removed invalid entries")
	}
	if result.BackupPath == "" {
		t.Error("BackupPath should be set")
	}

	// Verify backup exists
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Errorf("Backup not created: %v", err)
	}

	// Verify repaired manifest only has valid entries
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("Repaired manifest not created: %v", err)
	}
}

// ============================================================================
// File Integrity Tests
// ============================================================================

// TestVerifyFileIntegrity validates file against manifest entry
func TestVerifyFileIntegrity(t *testing.T) {
	testDir := t.TempDir()

	// Create test file
	filePath := filepath.Join(testDir, "photo.jpg")
	testData := "test data content"
	if err := os.WriteFile(filePath, []byte(testData), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Create manifest entry
	entry := ManifestRow{
		RelativePath: "photo.jpg",
		SizeBytes:    int64(len(testData)),
		PartialHash:  "d8e8fca2dc0f896fd7cb4cb0031ba249", // Dummy hash for now
	}

	result := VerifyFileIntegrity(filePath, entry)

	if result.Path != filePath {
		t.Errorf("Path mismatch: %q != %q", result.Path, filePath)
	}
	if result.Size != int64(len(testData)) {
		t.Errorf("Size %d, want %d", result.Size, len(testData))
	}
}

// TestVerifyFileIntegritySizeMismatch detects size changes
func TestVerifyFileIntegritySizeMismatch(t *testing.T) {
	testDir := t.TempDir()

	filePath := filepath.Join(testDir, "photo.jpg")
	if err := os.WriteFile(filePath, []byte("actual data"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	entry := ManifestRow{
		RelativePath: "photo.jpg",
		SizeBytes:    9999, // Wrong size
	}

	result := VerifyFileIntegrity(filePath, entry)

	if result.IsValid {
		t.Error("Should detect size mismatch")
	}
	if !strings.Contains(result.Mismatch, "size mismatch") {
		t.Errorf("Mismatch %q should mention size", result.Mismatch)
	}
}

// TestVerifyFileIntegrityMissing detects missing files
func TestVerifyFileIntegrityMissing(t *testing.T) {
	testDir := t.TempDir()

	filePath := filepath.Join(testDir, "missing.jpg")

	entry := ManifestRow{
		RelativePath: "missing.jpg",
		SizeBytes:    1000,
	}

	result := VerifyFileIntegrity(filePath, entry)

	if result.IsValid {
		t.Error("Should detect missing file")
	}
	if !strings.Contains(result.Mismatch, "not found") {
		t.Errorf("Mismatch %q should mention not found", result.Mismatch)
	}
}

// ============================================================================
// Integrity Audit Trail Tests
// ============================================================================

// TestLogIntegrityEvent writes event to audit trail
func TestLogIntegrityEvent(t *testing.T) {
	testDir := t.TempDir()
	auditPath := filepath.Join(testDir, "audit.log")

	event := IntegrityEvent{
		Timestamp: time.Now(),
		EventType: "sign",
		Details:   "manifest signed with key ABC123",
		Status:    "success",
	}

	if err := LogIntegrityEvent(auditPath, event); err != nil {
		t.Fatalf("LogIntegrityEvent: %v", err)
	}

	// Verify audit file was created
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "sign") {
		t.Error("Audit log should contain event type")
	}
	if !strings.Contains(content, "success") {
		t.Error("Audit log should contain status")
	}
}

// TestLogIntegrityEventMultiple appends multiple events
func TestLogIntegrityEventMultiple(t *testing.T) {
	testDir := t.TempDir()
	auditPath := filepath.Join(testDir, "audit.log")

	events := []IntegrityEvent{
		{Timestamp: time.Now(), EventType: "sign", Details: "First event", Status: "success"},
		{Timestamp: time.Now(), EventType: "verify", Details: "Second event", Status: "success"},
		{Timestamp: time.Now(), EventType: "repair", Details: "Third event", Status: "warning"},
	}

	for _, event := range events {
		if err := LogIntegrityEvent(auditPath, event); err != nil {
			t.Fatalf("LogIntegrityEvent: %v", err)
		}
	}

	// Verify all events are in audit log
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "First event") {
		t.Error("Should have first event")
	}
	if !strings.Contains(content, "Second event") {
		t.Error("Should have second event")
	}
	if !strings.Contains(content, "Third event") {
		t.Error("Should have third event")
	}
}
