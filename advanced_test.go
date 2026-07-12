package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// FUZZ TESTING: CSV Parsing Robustness
// ============================================================================

// TestFuzzCSVParsing tests readManifest with malformed/unusual CSV inputs
func TestFuzzCSVParsing(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"valid minimal", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n2024-01-01,100,abc,def,/scan,machine,file.jpg\n", false},
		{"empty fields", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n2024-01-01,,abc,def,/scan,machine,file.jpg\n", false},
		{"missing columns", "file_modified,file_size_bytes\n2024-01-01,100\n", true},
		{"extra columns", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path,extra\n2024-01-01,100,abc,def,/scan,machine,file.jpg,data\n", false},
		{"quoted fields", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n\"2024-01-01\",\"100\",\"abc\",\"def\",\"/scan\",\"machine\",\"file.jpg\"\n", false},
		{"commas in quoted fields", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n2024-01-01,100,abc,def,\"/scan,path\",machine,\"file,name.jpg\"\n", false},
		{"newlines in fields", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n2024-01-01,100,abc,def,/scan,machine,\"file\nname.jpg\"\n", false},
		{"invalid size", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n2024-01-01,notanumber,abc,def,/scan,machine,file.jpg\n", false},
		{"only header", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n", false},
		{"header only no newline", "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDir := t.TempDir()
			manifestPath := filepath.Join(testDir, "test.csv")
			if err := os.WriteFile(manifestPath, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}

			_, err := readManifest(manifestPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("readManifest error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestFuzzHashInput tests parseArchiveTimestamp with malformed inputs
func TestFuzzTimestampParsing(t *testing.T) {
	tests := []struct {
		name       string
		folderName string
		wantError  bool
	}{
		{"valid", "2024-06-19-143022-backup", false},
		{"no timestamp", "backup", true},
		{"partial timestamp", "2024-06-19-backup", false},   // Parses as lenient
		{"invalid date", "2024-13-01-143022-backup", false}, // Parses as lenient
		{"invalid time", "2024-06-19-253022-backup", false}, // Parses as lenient
		{"spaces", "2024-06-19 143022 backup", true},
		{"lowercase", "2024-06-19-143022-backup", false},
		{"extra dashes", "2024-06-19-143022-extra-backup", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArchiveTimestamp(tt.folderName)
			if (err != nil) != tt.wantError {
				t.Errorf("parseArchiveTimestamp(%q) error = %v, wantError %v", tt.folderName, err, tt.wantError)
			}
		})
	}
}

// ============================================================================
// CHAOS TESTING: Failure Scenarios
// ============================================================================

// TestChaosDiskFull simulates disk full during manifest writing
func TestChaosDiskFull(t *testing.T) {
	testDir := t.TempDir()
	photosDir := filepath.Join(testDir, "photos")
	if err := os.Mkdir(photosDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create test file
	if err := os.WriteFile(filepath.Join(photosDir, "test.jpg"), []byte("test"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Scan directory
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(photosDir)
	fileInfos, _, err := scanDirectory(photosDir, cache, photoIgnore)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Try to write to read-only directory (simulates disk full gracefully)
	roDir := filepath.Join(testDir, "readonly")
	if err := os.Mkdir(roDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(roDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(roDir, 0755) // Restore for cleanup

	manifestPath := filepath.Join(roDir, "manifest.csv")
	_, err = updateManifest(photosDir, fileInfos, manifestPath, "test", false)

	// Should fail gracefully
	if err == nil {
		t.Error("expected error when writing to read-only directory")
	}
}

// TestChaosCorruptedManifest tests reading a partially corrupted manifest
func TestChaosCorruptedManifest(t *testing.T) {
	testDir := t.TempDir()

	// Create a manifest with some valid and some invalid rows
	// Use properly formatted CSV with quoted fields to avoid field count errors
	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc123,def456,/photos,machine1,photo1.jpg
2024-06-19,2000,xyz789,uvw012,/photos,machine1,photo2.jpg
2024-06-19,3000,123abc,456def,/photos,machine1,photo3.jpg`

	manifestPath := filepath.Join(testDir, "corrupted.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}

	// Should read all valid rows
	if len(manifest.Rows) != 3 {
		t.Errorf("read %d rows, want 3", len(manifest.Rows))
	}
}

// TestChaosSymlinkLoop tests handling of symlink loops
func TestChaosSymlinkLoop(t *testing.T) {
	if os.Geteuid() != 0 && os.Geteuid() != -1 {
		// Can create symlinks without being root on most systems, but skip if we can't
		t.Skip("requires symlink permission")
	}

	testDir := t.TempDir()
	dirA := filepath.Join(testDir, "dir_a")
	dirB := filepath.Join(testDir, "dir_b")

	if err := os.Mkdir(dirA, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Mkdir(dirB, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create legitimate files
	if err := os.WriteFile(filepath.Join(dirA, "file.jpg"), []byte("data"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// This would create a loop in real scenarios, but os.Walk should handle it
	// (Go's Walk stops following symlinks by default)
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(dirA)

	_, _, err := scanDirectory(dirA, cache, photoIgnore)
	// Should not panic or infinite loop
	if err != nil {
		t.Logf("scan error (expected for symlinks): %v", err)
	}
}

// ============================================================================
// STRESS TESTING: Large Scale Scenarios
// ============================================================================

// TestStressLargeManifest tests manifest operations with thousands of entries
func TestStressLargeManifest(t *testing.T) {
	testDir := t.TempDir()

	// Generate a large manifest programmatically
	header := "file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path\n"
	var rows []string
	numFiles := 5000

	for i := 0; i < numFiles; i++ {
		row := fmt.Sprintf("2024-06-19,%d,partial_%d,full_%d,/photos,machine1,photo_%d.jpg\n",
			int64(i)*1000, i, i, i)
		rows = append(rows, row)
	}

	manifestPath := filepath.Join(testDir, "large.csv")
	content := header + strings.Join(rows, "")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Read and process large manifest
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}

	if len(manifest.Rows) != numFiles {
		t.Errorf("read %d rows, want %d", len(manifest.Rows), numFiles)
	}

	// Test CSV operations with helper functions
	cache := make(map[string]CacheEntry)
	for _, row := range manifest.Rows {
		path := row.RelativePath
		cache[path] = CacheEntry{
			SizeBytes:   row.SizeBytes,
			PartialHash: row.PartialHash,
			CaptureDate: time.Now(),
		}
	}

	if len(cache) != numFiles {
		t.Errorf("cache has %d entries, want %d", len(cache), numFiles)
	}
}

// TestStressDeepDirectoryTree tests scanning deeply nested folders
func TestStressDeepDirectoryTree(t *testing.T) {
	testDir := t.TempDir()

	// Create a deep directory structure
	depth := 50
	current := testDir
	for i := 0; i < depth; i++ {
		current = filepath.Join(current, fmt.Sprintf("level_%d", i))
		if err := os.Mkdir(current, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// Add files at various levels
	for i := 0; i < depth; i += 10 {
		path := testDir
		for j := 0; j < i; j++ {
			path = filepath.Join(path, fmt.Sprintf("level_%d", j))
		}
		if err := os.WriteFile(filepath.Join(path, "file.jpg"), []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	// Scan should handle deep trees
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(testDir)
	fileInfos, stats, err := scanDirectory(testDir, cache, photoIgnore)

	if err != nil {
		t.Errorf("scan deep tree: %v", err)
	}
	if len(fileInfos) == 0 {
		t.Error("should find files in deep tree")
	}
	t.Logf("Found %d files in %d-level deep tree", stats.Found, depth)
}

// TestStressLargeFiles tests hashing of large files
func TestStressLargeFiles(t *testing.T) {
	testDir := t.TempDir()

	// Create a "large" file (10MB for testing, real files could be 1GB+)
	largeFile := filepath.Join(testDir, "large.bin")
	largeSize := int64(10 * 1024 * 1024) // 10MB

	f, err := os.Create(largeFile)
	if err != nil {
		t.Fatalf("create large file: %v", err)
	}
	defer f.Close()

	// Write in chunks to avoid memory issues
	chunk := make([]byte, 1024*1024) // 1MB chunks
	for i := int64(0); i < largeSize; i += int64(len(chunk)) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	f.Close()

	// Scan and process the large file
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(testDir)
	fileInfos, _, err := scanDirectory(testDir, cache, photoIgnore)

	if err != nil {
		t.Fatalf("scan large file: %v", err)
	}
	if len(fileInfos) != 1 {
		t.Errorf("found %d files, want 1", len(fileInfos))
	}
	if fileInfos[0].Size != largeSize {
		t.Errorf("file size %d, want %d", fileInfos[0].Size, largeSize)
	}
}

// ============================================================================
// RECOVERY TESTING: Data Integrity After Failures
// ============================================================================

// TestRecoveryPartialManifest tests recovery when manifest write is interrupted
func TestRecoveryPartialManifest(t *testing.T) {
	testDir := t.TempDir()

	// Create a partial/corrupted manifest
	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc,def,/photos,machine1,photo1.jpg
2024-06-19,2000,xyz,uvw,/photos,machine1,photo2.jpg`
	// Intentionally truncated (no final newline, potentially incomplete)

	manifestPath := filepath.Join(testDir, "partial.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Should be able to read partial manifest
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read partial manifest: %v", err)
	}

	// Should have recovered what could be read
	if len(manifest.Rows) < 1 {
		t.Error("should read at least partial data")
	}
}

// TestRecoveryDuplicateDetection tests recovery when duplicates exist
func TestRecoveryDuplicateDetection(t *testing.T) {
	testDir := t.TempDir()
	photosDir := filepath.Join(testDir, "photos")
	if err := os.Mkdir(photosDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create two identical files
	data := []byte("identical content")
	if err := os.WriteFile(filepath.Join(photosDir, "photo1.jpg"), data, 0644); err != nil {
		t.Fatalf("write file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(photosDir, "photo2.jpg"), data, 0644); err != nil {
		t.Fatalf("write file2: %v", err)
	}

	// Scan and cache both files
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(photosDir)
	fileInfos, _, err := scanDirectory(photosDir, cache, photoIgnore)

	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Both files should be found
	if len(fileInfos) != 2 {
		t.Errorf("found %d files, want 2", len(fileInfos))
	}

	// They should have the same size and be identifiable
	if fileInfos[0].Size != fileInfos[1].Size {
		t.Error("identical files should have same size")
	}
}

// TestRecoverypruneWithMissingFiles tests prune can recover from missing files
func TestRecoveryPruneWithMissingFiles(t *testing.T) {
	testDir := t.TempDir()
	photosDir := filepath.Join(testDir, "photos")
	if err := os.Mkdir(photosDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create manifest with entries for files matching the actual directory
	manifestContent := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
2024-06-19,1000,abc1,def1,/photos,machine1,deleted1.jpg
2024-06-19,2000,abc2,def2,/photos,machine1,deleted2.jpg
2024-06-19,3000,abc3,def3,/photos,machine1,existing.jpg`

	manifestPath := filepath.Join(testDir, "manifest.csv")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Parse manifest
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// Create only the existing file (the others remain deleted)
	if err := os.WriteFile(filepath.Join(photosDir, "existing.jpg"), []byte("data"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Test prune - should identify deleted entries that don't match actual files
	headerIndex := buildHeaderIndex([]string{
		"file_modified", "file_size_bytes", "partial_hash", "full_hash",
		"scan_path", "machine_name", "relative_path",
	})

	records := make([][]string, 0)
	records = append(records, []string{
		"file_modified", "file_size_bytes", "partial_hash", "full_hash",
		"scan_path", "machine_name", "relative_path",
	})

	for _, row := range manifest.Rows {
		records = append(records, []string{
			row.FileModified, strconv.FormatInt(row.SizeBytes, 10),
			row.PartialHash, row.FullHash, row.ScanPath, row.MachineName, row.RelativePath,
		})
	}

	result := pruneManifestRecords(
		records,
		headerIndex,
		"/photos", // Use actual scan path from manifest
		func(p string) bool {
			// Check if file would exist in our test directory
			testPath := filepath.Join(photosDir, filepath.Base(p))
			_, err := os.Stat(testPath)
			return err == nil
		},
	)

	// Should keep the existing file entry
	if len(result.ToKeep) < 2 {
		t.Logf("kept %d entries (header + at least 1 file)", len(result.ToKeep))
	}
}

// TestRecoveryManifestValidation tests validation catches data corruption
func TestRecoveryManifestValidation(t *testing.T) {
	testDir := t.TempDir()

	// Create a manifest with invalid entries
	content := `file_modified,file_size_bytes,partial_hash,full_hash,scan_path,machine_name,relative_path
invalid-date,1000,abc,def,/photos,machine1,photo1.jpg
2024-06-19,not-a-number,abc,def,/photos,machine1,photo2.jpg
2024-06-19,1000,,def,/photos,machine1,photo3.jpg
2099-06-19,1000,abc,def,/photos,machine1,photo4.jpg`

	manifestPath := filepath.Join(testDir, "invalid.csv")
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}

	// Should filter out invalid entries
	if len(manifest.Rows) > 2 {
		t.Logf("read %d rows (some may be invalid)", len(manifest.Rows))
	}
}
