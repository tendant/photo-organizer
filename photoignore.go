package main

import (
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
			for _, part := range pathParts[:len(pathParts)-1] { // exclude filename
				if matched, _ := filepath.Match(p, part); matched {
					return true
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
