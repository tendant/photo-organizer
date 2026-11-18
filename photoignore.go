package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PhotoIgnore loads and applies .photoignore patterns from scan root and subdirectories
type PhotoIgnore struct {
	root  string
	cache map[string][]string // dir → patterns loaded from that dir's .photoignore
}

func newPhotoIgnore(scanRoot string) *PhotoIgnore {
	return &PhotoIgnore{
		root:  filepath.Clean(scanRoot),
		cache: make(map[string][]string),
	}
}

func (ig *PhotoIgnore) loadDir(dir string) []string {
	if patterns, ok := ig.cache[dir]; ok {
		return patterns
	}
	var patterns []string
	data, err := os.ReadFile(filepath.Join(dir, ".photoignore"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, line)
			}
		}
	}
	ig.cache[dir] = patterns
	return patterns
}

// ShouldSkip checks if a file path matches any .photoignore patterns
func (ig *PhotoIgnore) ShouldSkip(path string) bool {
	name := filepath.Base(path)
	relPath, _ := filepath.Rel(ig.root, path)
	pathParts := strings.Split(filepath.ToSlash(relPath), "/")

	// Load patterns from root .photoignore
	for _, pattern := range ig.loadDir(ig.root) {
		isDir := strings.HasSuffix(pattern, "/")
		p := strings.TrimSuffix(pattern, "/")

		if isDir {
			// Directory pattern: check if any path component matches
			if len(pathParts) > 1 {
				for _, part := range pathParts[:len(pathParts)-1] { // exclude filename
					if matched, _ := filepath.Match(p, part); matched {
						return true
					}
				}
			}
		} else {
			// File pattern: match against filename
			if matched, _ := filepath.Match(p, name); matched {
				return true
			}
		}
	}

	// Check subdirectory .photoignore files (cascade)
	dir := filepath.Dir(path)
	for {
		if dir == ig.root || dir == filepath.Dir(dir) {
			break
		}
		for _, pattern := range ig.loadDir(dir) {
			isDir := strings.HasSuffix(pattern, "/")
			p := strings.TrimSuffix(pattern, "/")

			if isDir {
				// Match against any path component from this dir downward
				relFromHere, _ := filepath.Rel(dir, path)
				hereParts := strings.Split(filepath.ToSlash(relFromHere), "/")
				for _, part := range hereParts[:len(hereParts)-1] { // exclude filename
					if matched, _ := filepath.Match(p, part); matched {
						return true
					}
				}
			} else {
				if matched, _ := filepath.Match(p, name); matched {
					return true
				}
			}
		}
		dir = filepath.Dir(dir)
	}
	return false
}

// defaultPhotoIgnore is written to a scan root that has no .photoignore yet. It lists
// only folders that never hold someone's photos or videos: program files, caches,
// dependency trees, system folders. Edit the file to taste; scans never rewrite it.
const defaultPhotoIgnore = `# .photoignore - folders and files the scan skips (one pattern per line).
#   name/     skips any folder with that name, at any depth
#   *.ext     skips files matching the name
# Created automatically by the first scan of this folder; edit freely.

# Programs and games
Program Files/
Program Files (x86)/
ProgramData/
steamapps/
Windows/
node_modules/
__pycache__/
.venv/
site-packages/

# System and recycle folders
$RECYCLE.BIN/
System Volume Information/
Recovery/
lost+found/

# Caches and temporary files
Cache/
Caches/
CachedData/
*.tmp
*.part
*.crdownload
`

// ensurePhotoIgnore creates the default .photoignore in dir when there is none.
// It reports whether it wrote one. A folder we cannot write to (a read-only drive)
// is fine: the scan just runs without it.
func ensurePhotoIgnore(dir string) bool {
	path := filepath.Join(dir, ".photoignore")
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.WriteString(defaultPhotoIgnore); err != nil {
		return false
	}
	fmt.Fprintf(os.Stderr, "Created %s with default ignore patterns (edit it to change what scans skip)\n", path)
	return true
}
