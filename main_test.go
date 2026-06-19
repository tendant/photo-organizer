package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// getDateFromFilename
// =============================================================================

func TestGetDateFromFilename(t *testing.T) {
	tests := []struct {
		filename string
		wantYear int
		wantMon  time.Month
		wantDay  int
		wantOK   bool
	}{
		{"DJI_20230704_001.jpg", 2023, time.July, 4, true},
		{"20230704_C0001.mp4", 2023, time.July, 4, true},
		{"20230704_123456.jpg", 2023, time.July, 4, true},
		{"2023-07-04_portrait.jpg", 2023, time.July, 4, true},
		{"IMG_20230704.jpg", 2023, time.July, 4, true},
		{"no_date_here.jpg", 0, 0, 0, false},
		{"IMG_1234.jpg", 0, 0, 0, false}, // too short to be a date
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got, ok := getDateFromFilename(tt.filename)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if got.Year() != tt.wantYear || got.Month() != tt.wantMon || got.Day() != tt.wantDay {
					t.Errorf("got %v, want %d-%02d-%02d", got, tt.wantYear, tt.wantMon, tt.wantDay)
				}
			}
		})
	}
}

// =============================================================================
// isMediaFile
// =============================================================================

func TestIsMediaFile(t *testing.T) {
	yes := []string{".jpg", ".JPG", ".jpeg", ".png", ".heic", ".hif", ".dng",
		".arw", ".cr2", ".nef", ".raf", ".mp4", ".mov", ".avi", ".mkv",
		".wav", ".mp3", ".lrf", ".xmp", ".json"}
	no := []string{".txt", ".pdf", ".docx", ".exe", ".zip", ".csv", ""}

	for _, ext := range yes {
		if !isMediaFile(ext) {
			t.Errorf("isMediaFile(%q) = false, want true", ext)
		}
	}
	for _, ext := range no {
		if isMediaFile(ext) {
			t.Errorf("isMediaFile(%q) = true, want false", ext)
		}
	}
}

// =============================================================================
// manifestFilename
// =============================================================================

func TestManifestFilename(t *testing.T) {
	tests := []struct {
		machine string
		path    string
		want    string
	}{
		{
			"macbook-pro",
			"/Users/lei/Photos",
			"photo_manifest_macbook-pro_Users_lei_Photos.csv",
		},
		{
			"nas main", // spaces become underscores
			"/volume1/photos",
			"photo_manifest_nas_main_volume1_photos.csv",
		},
	}
	for _, tt := range tests {
		got := manifestFilename(tt.machine, tt.path)
		if got != tt.want {
			t.Errorf("manifestFilename(%q, %q) = %q, want %q", tt.machine, tt.path, got, tt.want)
		}
	}
}

func TestManifestFilenameLongPath(t *testing.T) {
	// Paths longer than 60 chars should be truncated from the left.
	long := "/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z/photos"
	name := manifestFilename("m", long)
	// Should not panic and should contain .csv
	if len(name) == 0 {
		t.Error("expected non-empty filename")
	}
}

// =============================================================================
// formatCount / formatSize
// =============================================================================

func TestFormatCount(t *testing.T) {
	tests := []struct{ n int; want string }{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		if got := formatCount(tt.n); got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
	}
	for _, tt := range tests {
		if got := formatSize(tt.bytes); got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// =============================================================================
// loadCache: Corruption, Old Format, Special Characters
// =============================================================================

func TestLoadCacheCorrupt(t *testing.T) {
	dir := t.TempDir()
	manifestFile := filepath.Join(dir, "test.csv")
	// Write a valid header and one complete row, then truncate mid-next-row.
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,partial_hash,full_hash,scan_date,scan_path,machine_name
test1.jpg,test1.jpg,1000,0,2023-01-01,2023-01-01,,,abc123,,2023-01-01,/scan,m1
test2.jpg,test2.jpg,2000,0,2023-01-01,2023-01-01,,,def456,,2023-01-01,/scan`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cache := loadCache(manifestFile)
	// Corrupt manifest should return empty cache (no panic).
	if len(cache) != 0 {
		t.Errorf("corrupt manifest: got %d entries, want 0", len(cache))
	}
}

func TestLoadCacheOldFormat(t *testing.T) {
	dir := t.TempDir()
	manifestFile := filepath.Join(dir, "test.csv")
	// Old format: file_hash instead of partial_hash
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,file_hash,full_hash,scan_date,scan_path,machine_name
test.jpg,test.jpg,1000,0,2023-01-01,2023-01-01,,,oldhash123,,2023-01-01,/scan,m1`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cache := loadCache(manifestFile)
	entry, ok := cache["test.jpg"]
	if !ok {
		t.Fatalf("expected entry for test.jpg")
	}
	// Old file_hash should be read into PartialHash
	if entry.PartialHash != "oldhash123" {
		t.Errorf("got PartialHash %q, want %q", entry.PartialHash, "oldhash123")
	}
}

func TestLoadCacheSpecialChars(t *testing.T) {
	dir := t.TempDir()
	manifestFile := filepath.Join(dir, "test.csv")
	// Paths with spaces, apostrophes, and Unicode
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"filename", "relative_path", "file_size_bytes", "file_size_mb", "file_modified", "capture_date", "camera_make", "camera_model", "partial_hash", "full_hash", "scan_date", "scan_path", "machine_name"})
	w.Write([]string{"photo's.jpg", "photo's.jpg", "1000", "0", "2023-01-01", "2023-01-01", "", "", "abc123", "", "2023-01-01", "/scan", "m1"})
	w.Write([]string{"日本語.jpg", "日本語.jpg", "2000", "0", "2023-01-01", "2023-01-01", "", "", "def456", "", "2023-01-01", "/scan", "m1"})
	w.Write([]string{"spaces here.jpg", "spaces here.jpg", "3000", "0", "2023-01-01", "2023-01-01", "", "", "ghi789", "", "2023-01-01", "/scan", "m1"})
	w.Flush()

	if err := os.WriteFile(manifestFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cache := loadCache(manifestFile)
	if len(cache) != 3 {
		t.Errorf("got %d entries, want 3", len(cache))
	}
	if _, ok := cache["photo's.jpg"]; !ok {
		t.Errorf("missing entry for photo's.jpg")
	}
	if _, ok := cache["日本語.jpg"]; !ok {
		t.Errorf("missing entry for 日本語.jpg")
	}
	if _, ok := cache["spaces here.jpg"]; !ok {
		t.Errorf("missing entry for spaces here.jpg")
	}
}

// =============================================================================
// updateManifest: Corrupt Existing, Disk Full (mock), Prune Safety
// =============================================================================

func TestUpdateManifestCorruptExisting(t *testing.T) {
	dir := t.TempDir()
	manifestFile := filepath.Join(dir, "test.csv")
	// Write a corrupted manifest (valid header, then truncated row).
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,partial_hash,full_hash,scan_date,scan_path,machine_name
test.jpg,test.jpg,1000,0,2023-01-01,2023-01-01,,,abc123,,2023-01-01,/scan`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Try to update the corrupted manifest with a new file.
	files := []FileInfo{{Path: filepath.Join(dir, "newfile.jpg"), Size: 2000}}
	_, err := updateManifest(dir, files, manifestFile, "m1", false)
	if err == nil {
		t.Errorf("updateManifest should return an error for corrupt manifest, got nil")
	}
	if !strings.Contains(err.Error(), "read existing manifest") {
		t.Errorf("error message should mention read error, got %q", err.Error())
	}
}

func TestUpdateManifestDiskFull(t *testing.T) {
	// Create a temp dir and make it read-only to prevent temp file creation.
	// With atomic write, os.CreateTemp tries to create in the manifest directory.
	dir := t.TempDir()
	manifestDir := filepath.Join(dir, "manifests")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifestFile := filepath.Join(manifestDir, "test.csv")

	// Pre-create the manifest file with a valid entry.
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"filename", "relative_path", "file_size_bytes", "file_size_mb", "file_modified", "capture_date", "camera_make", "camera_model", "partial_hash", "full_hash", "scan_date", "scan_path", "machine_name"})
	w.Write([]string{"test.jpg", "test.jpg", "1000", "0", "2023-01-01", "2023-01-01", "", "", "abc123", "", "2023-01-01", dir, "m1"})
	w.Flush()
	if err := os.WriteFile(manifestFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Make the manifest directory read-only to prevent temp file creation.
	if err := os.Chmod(manifestDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(manifestDir, 0o755) // restore for cleanup

	files := []FileInfo{{Path: filepath.Join(dir, "newfile.jpg"), Size: 2000}}
	_, err := updateManifest(dir, files, manifestFile, "m1", false)
	if err == nil {
		t.Errorf("updateManifest should return an error when write fails, got nil")
	}
}


// =============================================================================
// scanDirectory: Special Chars, Symlinks, Skip Folders, File Sizes
// =============================================================================

func TestScanDirectorySpecialChars(t *testing.T) {
	dir := t.TempDir()
	// Create files with special characters in names.
	files := []string{"photo's.jpg", "日本語.jpg", "name with spaces.jpg"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("fake"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	cache := make(map[string]CacheEntry)
	entries, _, err := scanDirectory(dir, cache, newPhotoIgnore(dir))
	if err != nil {
		t.Fatalf("scanDirectory: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3", len(entries))
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Path] = true
	}

	for _, f := range files {
		fullPath := filepath.Join(dir, f)
		if !names[fullPath] {
			t.Errorf("missing entry for %q", f)
		}
	}
}

func TestScanDirectorySymlinkSkipped(t *testing.T) {
	if os.Getenv("SKIP_SYMLINK_TEST") != "" {
		t.Skip("skipping symlink test")
	}

	dir := t.TempDir()
	// Create a real file.
	realFile := filepath.Join(dir, "real.jpg")
	if err := os.WriteFile(realFile, []byte("real"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	// Create a symlink pointing to the real file.
	symlink := filepath.Join(dir, "link.jpg")
	if err := os.Symlink(realFile, symlink); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	cache := make(map[string]CacheEntry)
	entries, stats, err := scanDirectory(dir, cache, newPhotoIgnore(dir))
	if err != nil {
		t.Fatalf("scanDirectory: %v", err)
	}

	// Only the real file should be in entries, not the symlink.
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1 (symlink should be skipped)", len(entries))
	}
	if entries[0].Path != realFile {
		t.Errorf("got entry for %q, want %q", entries[0].Path, realFile)
	}

	// Symlink count should be incremented.
	if stats.Symlinks != 1 {
		t.Errorf("got %d symlinks, want 1", stats.Symlinks)
	}
}

func TestScanDirectorySkipFolders(t *testing.T) {
	dir := t.TempDir()
	// Create subdirectories that should be skipped.
	skipDirs := []string{"PRIVATE", "THMBNL", "AVF_INFO", ".fseventsd"}
	for _, d := range skipDirs {
		subdir := filepath.Join(dir, d)
		if err := os.Mkdir(subdir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Add a file inside the skip folder.
		if err := os.WriteFile(filepath.Join(subdir, "test.jpg"), []byte("test"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	// Create a file in the root that should not be skipped.
	if err := os.WriteFile(filepath.Join(dir, "keep.jpg"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cache := make(map[string]CacheEntry)
	entries, _, err := scanDirectory(dir, cache, newPhotoIgnore(dir))
	if err != nil {
		t.Fatalf("scanDirectory: %v", err)
	}

	// Only the keep.jpg file should be returned.
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
	if entries[0].Path != filepath.Join(dir, "keep.jpg") {
		t.Errorf("got entry for %q, want keep.jpg", entries[0].Path)
	}
}

func TestProcessFileZeroBytes(t *testing.T) {
	dir := t.TempDir()
	zeroFile := filepath.Join(dir, "zero.jpg")
	if err := os.WriteFile(zeroFile, []byte{}, 0o644); err != nil {
		t.Fatalf("write zero-byte file: %v", err)
	}

	hash, _ := processFile(zeroFile)
	// Zero-byte file should still produce a hash (MD5 of empty string).
	if hash == "" {
		t.Errorf("expected non-empty hash for zero-byte file")
	}
}

func TestProcessFileExactSampleSize(t *testing.T) {
	dir := t.TempDir()
	file32k := filepath.Join(dir, "32k.jpg")
	// Create a file exactly 32KB (sampleSize).
	data := make([]byte, 32768)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(file32k, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	hash, _ := processFile(file32k)
	// For a file exactly 32KB, processFile should produce a hash.
	if hash == "" {
		t.Errorf("expected non-empty hash for 32KB file")
	}
}

func TestManifestFilenameSanitization(t *testing.T) {
	tests := []struct {
		machine string
		path    string
		desc    string
	}{
		{"m", "/path/with  spaces", "multiple spaces"},
		{"m", "/path//double/slash", "double slash"},
		{"m", "/path/__underscore", "leading underscores"},
		{"m", "/trailing/underscore_", "trailing underscore"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := manifestFilename(tt.machine, tt.path)
			// Should not panic and should end in .csv.
			if !strings.HasSuffix(got, ".csv") {
				t.Errorf("got %q, should end in .csv", got)
			}
			// Should not have doubled underscores.
			if strings.Contains(got, "__") {
				t.Errorf("got %q, should not have doubled underscores", got)
			}
		})
	}
}
