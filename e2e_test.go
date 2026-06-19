package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// E2E Test Suite: Real-world workflow scenarios
// These tests verify complete workflows from scan through archive to cleanup

// TestE2EFullWorkflow: scan → manifest → verify deletion detection
func TestE2EFullWorkflow(t *testing.T) {
	// Setup: Create realistic folder structure
	testDir := t.TempDir()
	manifestDir := t.TempDir()

	// Create test photos folder
	photosDir := filepath.Join(testDir, "Photos")
	if err := os.Mkdir(photosDir, 0755); err != nil {
		t.Fatalf("mkdir photos: %v", err)
	}

	// Create sample media files
	files := []struct {
		name string
		size int
	}{
		{"photo1.jpg", 1000},
		{"photo2.jpg", 2000},
		{"video1.mp4", 5000},
		{"document.txt", 100},
	}

	for _, f := range files {
		data := make([]byte, f.size)
		if err := os.WriteFile(filepath.Join(photosDir, f.name), data, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	// Step 1: Scan the folder
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(photosDir)
	fileInfos, scanStats, err := scanDirectory(photosDir, cache, photoIgnore)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	// Verify scan results
	if len(fileInfos) != 4 {
		t.Errorf("scan found %d files, want 4", len(fileInfos))
	}
	if scanStats.Found != 4 {
		t.Errorf("scan stats found %d, want 4", scanStats.Found)
	}

	// Step 2: Create manifest
	manifestPath := filepath.Join(manifestDir, "test_manifest.csv")
	_, err = updateManifest(photosDir, fileInfos, manifestPath, "test-machine", false)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}

	// Verify manifest was created
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest not created: %v", err)
	}

	// Step 3: Verify manifest contents
	manifestData, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	if len(manifestData.Rows) != 4 {
		t.Errorf("manifest has %d rows, want 4", len(manifestData.Rows))
	}

	// Step 4: Delete two files to simulate file removal
	if err := os.Remove(filepath.Join(photosDir, "photo1.jpg")); err != nil {
		t.Fatalf("delete photo1: %v", err)
	}
	if err := os.Remove(filepath.Join(photosDir, "video1.mp4")); err != nil {
		t.Fatalf("delete video1: %v", err)
	}

	// Step 5: Verify prune detects deleted entries
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer manifestFile.Close()

	reader := csv.NewReader(manifestFile)
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}

	headerIndex := buildHeaderIndex(records[0])
	result := pruneManifestRecords(
		records,
		headerIndex,
		photosDir,
		func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
	)

	// After deletion, prune should find 2 entries as deleted
	if result.ToDelete != 2 {
		t.Errorf("prune found %d deleted entries, want 2", result.ToDelete)
	}

	// Should keep 3 entries (header + 2 existing files)
	if len(result.ToKeep) != 3 {
		t.Errorf("prune kept %d entries, want 3 (header + 2 existing files)", len(result.ToKeep))
	}
}

// TestE2EPhotoIgnoreIntegration: .photoignore affects scan results
func TestE2EPhotoIgnoreIntegration(t *testing.T) {
	testDir := t.TempDir()

	// Create folder structure
	if err := os.Mkdir(filepath.Join(testDir, ".claude"), 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.Mkdir(filepath.Join(testDir, "photos"), 0755); err != nil {
		t.Fatalf("mkdir photos: %v", err)
	}

	// Create files
	files := map[string]string{
		"photo.jpg":              filepath.Join(testDir, "photo.jpg"),
		"backup.jpg":             filepath.Join(testDir, "backup.jpg"),
		"config.json":            filepath.Join(testDir, ".claude", "config.json"),
		"nested.jpg":             filepath.Join(testDir, "photos", "nested.jpg"),
	}

	for _, path := range files {
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	// Scan without .photoignore (should find all non-dotfile files)
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(testDir)
	results1, _, err := scanDirectory(testDir, cache, photoIgnore)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Should find 3 files (photo.jpg, backup.jpg, nested.jpg)
	// .claude is a dotfile so it should be skipped
	if len(results1) != 3 {
		t.Errorf("scan without .photoignore found %d files, want 3", len(results1))
		for i, r := range results1 {
			t.Logf("  file %d: %s", i, r.Path)
		}
	}

	// Create .photoignore to exclude backup.jpg and itself
	photoignorePath := filepath.Join(testDir, ".photoignore")
	if err := os.WriteFile(photoignorePath, []byte("backup.jpg\n.photoignore\n"), 0644); err != nil {
		t.Fatalf("write .photoignore: %v", err)
	}

	// Scan with .photoignore
	cache2 := make(map[string]CacheEntry)
	photoIgnore3 := newPhotoIgnore(testDir)
	results2, _, err := scanDirectory(testDir, cache2, photoIgnore3)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Should now find 2 files (photo.jpg, nested.jpg)
	if len(results2) != 2 {
		t.Errorf("scan with .photoignore found %d files, want 2", len(results2))
		for i, r := range results2 {
			t.Logf("  file %d: %s", i, r.Path)
		}
	}
}

// TestE2EMultipleManifests: Handle multiple manifests from different machines
func TestE2EMultipleManifests(t *testing.T) {
	testDir := t.TempDir()

	// Create manifests from different machines
	machines := []struct {
		name      string
		scanPath  string
		fileCount int
	}{
		{"machine-a", "/data/photos-a", 10},
		{"machine-b", "/data/photos-b", 15},
		{"machine-c", "/data/photos-c", 8},
	}

	var allManifests []*ManifestSource

	for _, m := range machines {
		// Create mock manifest
		rows := []ManifestRow{
			{MachineName: m.name, ScanPath: m.scanPath, FileModified: "2026-06-19"},
		}

		// Add mock file entries
		for i := 0; i < m.fileCount; i++ {
			rows = append(rows, ManifestRow{
				MachineName:  m.name,
				ScanPath:     m.scanPath,
				RelativePath: "photo_" + string(rune(i)) + ".jpg",
				FileModified: "2026-06-19",
			})
		}

		allManifests = append(allManifests, &ManifestSource{
			FilePath:    filepath.Join(testDir, m.name+".csv"),
			MachineName: m.name,
			ScanPath:    m.scanPath,
			Rows:        rows,
		})
	}

	// Test: Find stalled manifests (machines that don't exist)
	pathExists := func(p string) bool {
		return false // All paths "missing"
	}

	stalled := findStalledManifests(allManifests, "", true, pathExists)

	if len(stalled) != 3 {
		t.Errorf("found %d stalled manifests, want 3", len(stalled))
	}

	// Test: Filter by local machine
	stalled2 := findStalledManifests(allManifests, "machine-a", false, pathExists)

	if len(stalled2) != 1 {
		t.Errorf("found %d local stalled manifests, want 1", len(stalled2))
	}
	if stalled2[0].Machine != "machine-a" {
		t.Errorf("stalled manifest machine %q, want machine-a", stalled2[0].Machine)
	}
}

// TestE2EArchiveTimestampParsing: Archive folder naming and parsing
func TestE2EArchiveTimestampParsing(t *testing.T) {
	// Generate archive folder name with timestamp
	folderName := "MyPhotos"
	timestamp := time.Date(2026, 6, 19, 6, 0, 12, 0, time.UTC)
	archiveFolderName := generateArchiveFolderName(folderName, timestamp)

	// Should format as YYYY-MM-DD-HHMMSSname
	if !strings.Contains(archiveFolderName, folderName) {
		t.Errorf("archive folder name %q doesn't contain %q", archiveFolderName, folderName)
	}

	// Parse it back
	parsed, err := parseArchiveTimestamp(archiveFolderName)
	if err != nil {
		t.Fatalf("parse timestamp: %v", err)
	}

	if parsed == "" {
		t.Error("parsed timestamp is empty")
	}

	// Verify timestamp format (HH:MM:SS)
	if !strings.Contains(parsed, ":") {
		t.Errorf("parsed timestamp %q doesn't have time format", parsed)
	}
}

// TestE2EManifestPersistence: Manifest written and read back correctly
func TestE2EManifestPersistence(t *testing.T) {
	testDir := t.TempDir()

	// Create test files
	photosDir := filepath.Join(testDir, "photos")
	if err := os.Mkdir(photosDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	testFiles := []string{"photo1.jpg", "photo2.jpg", "video.mp4"}
	for _, f := range testFiles {
		if err := os.WriteFile(filepath.Join(photosDir, f), []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	// Scan and create manifest
	cache := make(map[string]CacheEntry)
	photoIgnore := newPhotoIgnore(photosDir)
	fileInfos, _, err := scanDirectory(photosDir, cache, photoIgnore)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	manifestPath := filepath.Join(testDir, "manifest.csv")
	_, err = updateManifest(photosDir, fileInfos, manifestPath, "test-machine", false)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}

	// Read manifest back
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// Verify round-trip
	if manifest.MachineName != "test-machine" {
		t.Errorf("machine name %q, want test-machine", manifest.MachineName)
	}
	if len(manifest.Rows) != 3 {
		t.Errorf("manifest has %d rows, want 3", len(manifest.Rows))
	}

	// Verify CSV format is valid
	file, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}

	// Header + 3 data rows
	if len(records) != 4 {
		t.Errorf("CSV has %d records, want 4", len(records))
	}
}
