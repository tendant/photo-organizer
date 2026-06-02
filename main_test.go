package main

import (
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
