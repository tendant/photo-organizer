package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasReason(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func writeFilesInto(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
}

// =============================================================================
// sampleFolder / sampleFolderRec
// =============================================================================

func TestSampleFolder(t *testing.T) {
	root := t.TempDir()
	writeFilesInto(t, root, "a.jpg", "b.txt")                  // media + non-media
	writeFilesInto(t, filepath.Join(root, "sub"), "d.mp4")     // media in nested dir
	writeFilesInto(t, filepath.Join(root, ".hidden"), "c.jpg") // hidden dir — skipped

	s := sampleFolder(root, 4)
	if s.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3 (hidden dir excluded)", s.TotalCount)
	}
	if s.MediaCount != 2 {
		t.Errorf("MediaCount = %d, want 2 (a.jpg, d.mp4)", s.MediaCount)
	}

	// Depth limit stops recursion into sub.
	shallow := sampleFolder(root, 1)
	if shallow.TotalCount != 2 || shallow.MediaCount != 1 {
		t.Errorf("depth-1 sample = %d/%d files, want 2/1 (no recursion)", shallow.TotalCount, shallow.MediaCount)
	}
}

func TestSampleFolderNameCap(t *testing.T) {
	root := t.TempDir()
	names := make([]string, 60)
	for i := range names {
		names[i] = "f" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".jpg"
	}
	writeFilesInto(t, root, names...)

	s := sampleFolder(root, 2)
	if s.TotalCount != 60 {
		t.Errorf("TotalCount = %d, want 60", s.TotalCount)
	}
	if len(s.FileNames) != 50 {
		t.Errorf("FileNames capped at %d, want 50", len(s.FileNames))
	}
}

// =============================================================================
// scoreFolderSample
// =============================================================================

func TestScoreFolderSample(t *testing.T) {
	// Empty folder.
	if score, reasons := scoreFolderSample("/x", FolderSample{}); score != 0 || !hasReason(reasons, "no files") {
		t.Errorf("empty = %d/%v, want 0/no-files", score, reasons)
	}

	// Strong photo folder: high ratio + >100 media + path word + year + camera prefix.
	score, reasons := scoreFolderSample("/home/user/Photos/2021", FolderSample{
		MediaCount: 200, TotalCount: 220, FileNames: []string{"IMG_0001.jpg"},
	})
	if score != 95 {
		t.Errorf("photo folder score = %d, want 95", score)
	}
	for _, want := range []string{"high media ratio", ">100 media files", "path:Photos", "year:2021", "camera:IMG_"} {
		if !hasReason(reasons, want) {
			t.Errorf("photo folder reasons %v missing %q", reasons, want)
		}
	}

	// Apple Photos library clamps at 100.
	if score, _ := scoreFolderSample("/Users/x/Pictures/Photos Library.photoslibrary/originals/0", FolderSample{
		MediaCount: 150, TotalCount: 150, FileNames: []string{"IMG_1.HEIC"},
	}); score != 100 {
		t.Errorf("apple library score = %d, want 100 (clamped)", score)
	}

	// Cache folder: negative signals clamp to 0.
	if score, reasons := scoreFolderSample("/home/user/thumbnails/cache", FolderSample{
		MediaCount: 0, TotalCount: 100,
	}); score != 0 || !hasReason(reasons, "cache folder") {
		t.Errorf("cache folder = %d/%v, want 0 with cache reason", score, reasons)
	}

	// Source tree: node_modules is penalized as source code.
	if _, reasons := scoreFolderSample("/proj/node_modules", FolderSample{
		MediaCount: 5, TotalCount: 10,
	}); !hasReason(reasons, "source code") {
		t.Errorf("node_modules reasons %v missing source-code penalty", reasons)
	}
}

// =============================================================================
// identifyPhotoFolders
// =============================================================================

func TestIdentifyPhotoFolders(t *testing.T) {
	root := t.TempDir()
	writeFilesInto(t, filepath.Join(root, "PhotosVac"),
		"IMG_1.jpg", "IMG_2.jpg", "IMG_3.jpg", "IMG_4.jpg", "IMG_5.jpg")
	writeFilesInto(t, filepath.Join(root, "junk"),
		"f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt")

	qualifying, skipped, err := identifyPhotoFolders(root, 50)
	if err != nil {
		t.Fatalf("identifyPhotoFolders: %v", err)
	}
	if len(qualifying) != 1 || filepath.Base(qualifying[0].Path) != "PhotosVac" {
		t.Errorf("qualifying = %+v, want [PhotosVac]", qualifying)
	}
	foundJunk := false
	for _, s := range skipped {
		if filepath.Base(s.Path) == "junk" {
			foundJunk = true
		}
	}
	if !foundJunk {
		t.Errorf("skipped = %+v, want junk present", skipped)
	}

	// Missing root -> error.
	if _, _, err := identifyPhotoFolders(filepath.Join(root, "nope"), 50); err == nil {
		t.Error("expected error for missing root")
	}
}

// =============================================================================
// analyzeDirectoryTypes
// =============================================================================

func TestAnalyzeDirectoryTypes(t *testing.T) {
	dir := t.TempDir()
	writeFilesInto(t, dir, "a.jpg", "b.txt", ".DS_Store", ".hidden.txt")

	out := captureStdout(t, func() { analyzeDirectoryTypes(dir) })

	for _, w := range []string{"File Type Summary", ".jpg", ".txt", "TOTAL", "Excluded"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n---\n%s", w, out)
		}
	}
	// Only a.jpg and b.txt count; .DS_Store (junk) and .hidden.txt (dotfile) are skipped.
	if !strings.Contains(out, "Ready to scan 2 files") {
		t.Errorf("expected 2 scannable files (junk/dotfiles excluded):\n%s", out)
	}
}
