package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApplePhotosPathHelpers(t *testing.T) {
	cases := []struct {
		path             string
		inside, internal bool
	}{
		{"/x/Photos Library.photoslibrary/originals/A/IMG.jpg", true, false},
		{"/x/Photos Library.photoslibrary/Masters/2019/10/a.jpg", true, false},
		{"/x/Photos Library.photoslibrary/resources/derivatives/a.jpg", true, true},
		{"/x/Photos Library.photoslibrary/database/Photos.sqlite", true, true},
		{"/x/Old.PHOTOSLIBRARY/thumbnails/a.jpg", true, true},
		{"/x/Pictures/IMG.jpg", false, false},
		{"/x/.photoslibrary/a.jpg", false, false},
	}
	for _, c := range cases {
		if got := inApplePhotos(c.path); got != c.inside {
			t.Errorf("inApplePhotos(%s) = %v", c.path, got)
		}
		if got := applePhotosInternal(c.path); got != c.internal {
			t.Errorf("applePhotosInternal(%s) = %v", c.path, got)
		}
	}
}

// Only the original photos are organised; thumbnails and derivatives stay out, and --move
// copies out of a package instead of renaming.
func TestOrganizeApplePhotosOnlyCopiesOriginalsAndNeverMoves(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	pkg := filepath.Join(src, "Photos Library.photoslibrary")
	orig := filepath.Join(pkg, "originals", "A", "IMG_20240315_101010.jpg")
	writeFile(t, orig, "original")
	writeFile(t, filepath.Join(pkg, "resources", "derivatives", "IMG_20240315_101010_thumb.jpg"), "thumb")

	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--move", "--apply", "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := strings.Join(listFiles(t, lib), ",")
	if !strings.Contains(got, "IMG_20240315_101010.jpg") || strings.Contains(got, "thumb") {
		t.Errorf("want only the original in the library, got %s", got)
	}
	if !exists(orig) {
		t.Error("--move must copy, not rename, a file out of a Photos library package")
	}
}
