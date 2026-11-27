package main

import (
	"path/filepath"
	"strings"
)

// Apple Photos libraries (Photos Library.photoslibrary) are packages: the app owns the database
// and the image files together, so renaming or removing a file inside one corrupts the library.
// photo-organizer therefore only ever reads from them:
//   - organize copies from the original-photo folders (originals/, Masters/) and ignores the
//     rest (thumbnails, derivatives, resources, database), which are generated, not your photos;
//   - organize --move never renames a file out of a package; it copies it instead;
//   - prune-sources never moves a file out of a package.

var applePhotosSuffixes = []string{".photoslibrary", ".photolibrary", ".migratedphotolibrary", ".aplibrary"}

// applePhotosRest returns the path components below the Apple Photos package that contains path.
// ok is false when path is not inside one. The package folder itself gives an empty rest.
func applePhotosRest(path string) (rest []string, ok bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, part := range parts {
		l := strings.ToLower(part)
		for _, suffix := range applePhotosSuffixes {
			if strings.HasSuffix(l, suffix) && len(l) > len(suffix) {
				return parts[i+1:], true
			}
		}
	}
	return nil, false
}

// inApplePhotos reports whether path is inside an Apple Photos library package.
func inApplePhotos(path string) bool {
	_, ok := applePhotosRest(path)
	return ok
}

// applePhotosInternal reports whether path is inside a package but outside the original-photo
// folders: thumbnails, derivatives, resources and the database. Those are never copied.
func applePhotosInternal(path string) bool {
	rest, ok := applePhotosRest(path)
	if !ok || len(rest) == 0 {
		return false
	}
	switch strings.ToLower(rest[0]) {
	case "originals", "masters":
		return false
	}
	return true
}
