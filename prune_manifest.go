package main

// Manifest-hash matching for prune-sources: decide that a source file is already in the
// library and in a backup from the hashes recorded by `scan`, without reading any photo data.
//
// A manifest row is trusted only while the file on disk still has the size and modified
// time the row recorded. Anything else (new, edited, moved or deleted since the scan) is
// not trusted, so the source file is kept.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const manifestTimeLayout = "2006-01-02 15:04:05" // how scan writes file_modified

// loadManifestForRoot finds the local manifest that was written for scanning root.
func loadManifestForRoot(root string) (ManifestSource, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return ManifestSource{}, err
	}
	dir := filepath.Join(defaultManifestRoot(), "_Manifest")
	suffix := strings.TrimPrefix(manifestFilename("x", abs), "photo_manifest_x_")
	candidates, _ := filepath.Glob(filepath.Join(dir, "photo_manifest_*_"+suffix))
	var best ManifestSource
	found := false
	for _, c := range candidates {
		src, err := readManifest(c)
		if err != nil || filepath.Clean(src.ScanPath) != filepath.Clean(abs) {
			continue
		}
		if !found || src.LastScanned > best.LastScanned {
			best, found = src, true
		}
	}
	if !found {
		return ManifestSource{}, fmt.Errorf("no manifest for %s: run 'photo-organizer scan %s' first", root, root)
	}
	return best, nil
}

func rowPath(src ManifestSource, row ManifestRow) string {
	return filepath.Join(src.ScanPath, row.RelativePath)
}

// freshOnDisk reports whether path still has the size and modified time of its manifest row.
func freshOnDisk(path string, row ManifestRow) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != row.SizeBytes {
		return false
	}
	return st.ModTime().Format(manifestTimeLayout) == row.FileModified
}

func hashKey(row ManifestRow) string {
	if row.PartialHash == "" {
		return ""
	}
	return fmt.Sprintf("%d:%s", row.SizeBytes, row.PartialHash)
}

// rowsMatch: same size and partial hash, and the same full hash when both rows have one.
func rowsMatch(a, b ManifestRow) bool {
	k := hashKey(a)
	if k == "" || k != hashKey(b) {
		return false
	}
	return a.FullHash == "" || b.FullHash == "" || a.FullHash == b.FullHash
}

type manifestCand struct {
	path string
	row  ManifestRow
}

type manifestIndex struct {
	byKey map[string][]manifestCand
	rows  int
	fresh map[string]bool // memo of freshOnDisk
}

func newManifestIndex(sources ...ManifestSource) *manifestIndex {
	idx := &manifestIndex{byKey: map[string][]manifestCand{}, fresh: map[string]bool{}}
	for _, src := range sources {
		for _, row := range src.Rows {
			k := hashKey(row)
			if k == "" {
				continue
			}
			idx.byKey[k] = append(idx.byKey[k], manifestCand{rowPath(src, row), row})
			idx.rows++
		}
	}
	return idx
}

func (m *manifestIndex) isFresh(path string, row ManifestRow) bool {
	if v, ok := m.fresh[path]; ok {
		return v
	}
	v := freshOnDisk(path, row)
	m.fresh[path] = v
	return v
}

// find returns a path of a file that matches row and is still as the manifest says.
func (m *manifestIndex) find(row ManifestRow) string {
	for _, c := range m.byKey[hashKey(row)] {
		if rowsMatch(row, c.row) && m.isFresh(c.path, c.row) {
			return c.path
		}
	}
	return ""
}

// verifyByManifest classifies the still-provisional items using manifests only.
func verifyByManifest(items []*pruneItem, opts pruneOptions) error {
	srcRows := map[string]ManifestRow{}
	for _, root := range opts.sources {
		src, err := loadManifestForRoot(root)
		if err != nil {
			return err
		}
		for _, row := range src.Rows {
			srcRows[rowPath(src, row)] = row
		}
	}
	libSrc, err := loadManifestForRoot(opts.library)
	if err != nil {
		return err
	}
	lib := newManifestIndex(libSrc)
	var bakSrcs []ManifestSource
	for _, b := range opts.backups {
		src, err := loadManifestForRoot(b)
		if err != nil {
			return err
		}
		bakSrcs = append(bakSrcs, src)
	}
	bak := newManifestIndex(bakSrcs...)
	fmt.Fprintf(os.Stderr, "Matching by manifest hashes (no photo data is read): library %s rows, backup %s rows.\n",
		formatCount(lib.rows), formatCount(bak.rows))
	if lib.rows == 0 || bak.rows == 0 {
		return fmt.Errorf("the library or backup manifest has no hashed files; scan them first")
	}
	for _, it := range items {
		if it.verdict != pruneSafe {
			continue
		}
		row, ok := srcRows[it.path]
		if !ok || !freshOnDisk(it.path, row) || hashKey(row) == "" {
			it.verdict = pruneNotFresh
			continue
		}
		if it.libCopy = lib.find(row); it.libCopy == "" {
			it.verdict = pruneNoLibrary
		} else if it.bakCopy = bak.find(row); it.bakCopy == "" {
			it.verdict = pruneNoBackup
		}
	}
	return nil
}
