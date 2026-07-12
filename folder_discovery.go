package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type FolderSample struct {
	MediaCount int
	TotalCount int
	FileNames  []string // up to 50 filenames for pattern detection
}

type ScoredFolder struct {
	Path    string
	Score   int
	Reasons []string
	Sample  FolderSample
}

// FileTypeStat holds aggregated stats for one file extension.
type FileTypeStat struct {
	Ext       string
	Count     int
	TotalSize int64
	IsScanned bool
}

// analyzeDirectoryTypes walks directory and shows what file types exist.
func analyzeDirectoryTypes(dir string) {
	stats := make(map[string]*FileTypeStat)
	var totalCount int
	var totalBytes int64
	var skippedSymlinks, skippedDotfiles, skippedJunk int

	fmt.Fprintf(os.Stderr, "Analyzing file types...\n")

	photoIgnore := newPhotoIgnore(dir)

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Skip directories
		if info.IsDir() {
			// Skip hidden directories
			if path != dir && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks (same as scan)
		if info.Mode()&os.ModeSymlink != 0 {
			skippedSymlinks++
			return nil
		}

		// Skip OS junk (same as scan)
		if shouldSkipFile(path) {
			skippedJunk++
			return nil
		}

		// Skip dotfiles except .photoignore (same as scan)
		if strings.HasPrefix(info.Name(), ".") && info.Name() != ".photoignore" {
			skippedDotfiles++
			return nil
		}

		// Apply .photoignore patterns (same as scan)
		if photoIgnore.ShouldSkip(path) {
			skippedDotfiles++
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		if ext == "" {
			ext = "(no extension)"
		}

		if _, exists := stats[ext]; !exists {
			stats[ext] = &FileTypeStat{Ext: ext}
		}
		stats[ext].Count++
		stats[ext].TotalSize += info.Size()
		totalCount++
		totalBytes += info.Size()

		// Show progress every 10,000 files
		if totalCount%10000 == 0 {
			fmt.Fprintf(os.Stderr, "  %d files analyzed...\n", totalCount)
		}
		return nil
	})

	// Sort by size descending
	var fileTypes []FileTypeStat
	for _, stat := range stats {
		fileTypes = append(fileTypes, *stat)
	}
	sort.Slice(fileTypes, func(i, j int) bool {
		return fileTypes[i].TotalSize > fileTypes[j].TotalSize
	})

	// Print report
	fmt.Printf("\n═════════════════════════════════════════════════════════\n")
	fmt.Printf("File Type Summary: %s\n", dir)
	fmt.Printf("═════════════════════════════════════════════════════════\n\n")

	fmt.Printf("All files will be scanned (except .DS_Store and .stfolder):\n")
	fmt.Printf("  %-10s %10s %12s\n", "Type", "Count", "Size")
	fmt.Printf("  %-10s %10s %12s\n", "────", "─────", "────")
	for _, s := range fileTypes {
		fmt.Printf("  %-10s %10d %12s\n", s.Ext, s.Count, formatBytes(s.TotalSize))
	}
	fmt.Printf("  %-10s %10d %12s\n", "TOTAL", totalCount, formatBytes(totalBytes))

	fmt.Printf("\n✓ Ready to scan %d files (%s)\n", totalCount, formatBytes(totalBytes))
	if skippedSymlinks > 0 || skippedDotfiles > 0 || skippedJunk > 0 {
		fmt.Printf("  (Excluded: %d symlinks, %d dotfiles, %d OS junk)\n\n", skippedSymlinks, skippedDotfiles, skippedJunk)
	} else {
		fmt.Printf("\n")
	}
}

// sampleFolder recursively counts files and collects sample filenames for pattern detection.
func sampleFolder(path string, maxDepth int) FolderSample {
	var result FolderSample
	sampleFolderRec(path, maxDepth, &result)
	return result
}

func sampleFolderRec(path string, maxDepth int, result *FolderSample) {
	if maxDepth <= 0 {
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			sampleFolderRec(filepath.Join(path, e.Name()), maxDepth-1, result)
		} else if !e.IsDir() {
			ext := filepath.Ext(e.Name())
			result.TotalCount++
			if isMediaFile(ext) {
				result.MediaCount++
			}
			// Collect up to 50 filenames for pattern detection.
			if len(result.FileNames) < 50 {
				result.FileNames = append(result.FileNames, e.Name())
			}
		}
	}
}

// scoreFolderSample computes a 0-100 folder score based on media ratio, file patterns, and path signals.
func scoreFolderSample(path string, s FolderSample) (score int, reasons []string) {
	if s.TotalCount == 0 {
		return 0, []string{"no files"}
	}

	mediaRatio := float64(s.MediaCount) / float64(s.TotalCount)

	// Positive signals.
	if mediaRatio >= 0.8 {
		score += 40
		reasons = append(reasons, "high media ratio")
	} else if mediaRatio >= 0.4 {
		score += 20
		reasons = append(reasons, "med media ratio")
	}

	if s.MediaCount > 100 {
		score += 20
		reasons = append(reasons, ">100 media files")
	} else if s.MediaCount > 10 {
		score += 10
		reasons = append(reasons, ">10 media files")
	}

	// Path-based signals (check full path).
	pathLower := strings.ToLower(path)
	// Also get just the folder name for less aggressive matching.
	baseName := strings.ToLower(filepath.Base(path))
	photoWords := []string{"photos", "pictures", "dcim", "camera", "gallery", "originals", "images", "fotos", "lightroom", "capture"}
	for _, word := range photoWords {
		if strings.Contains(pathLower, word) {
			score += 15
			reasons = append(reasons, fmt.Sprintf("path:%s", strings.ToUpper(word[:1])+word[1:]))
			break
		}
	}

	// Special library patterns.
	if strings.Contains(pathLower, ".photoslibrary") && strings.Contains(pathLower, "originals") {
		score += 20
		reasons = append(reasons, "Apple Photos library")
	}
	if strings.Contains(pathLower, "google photos") || strings.Contains(pathLower, "takeout") {
		score += 20
		reasons = append(reasons, "Google Photos export")
	}
	if strings.Contains(pathLower, "100apple") || (strings.Contains(pathLower, "dcim") && strings.Contains(pathLower, "100")) {
		score += 15
		reasons = append(reasons, "iPhone DCIM")
	}

	// Year detection (2000-2035).
	for year := 2000; year <= 2035; year++ {
		if strings.Contains(path, strconv.Itoa(year)) {
			score += 10
			reasons = append(reasons, fmt.Sprintf("year:%d", year))
			break
		}
	}

	// Camera filename prefixes.
	cameraPrefixes := []string{"IMG_", "DSC_", "DJI_", "PXL_", "VID_", "GoPro", "GX0", "GOPR"}
	for _, prefix := range cameraPrefixes {
		for _, fname := range s.FileNames {
			if strings.HasPrefix(fname, prefix) {
				score += 10
				reasons = append(reasons, fmt.Sprintf("camera:%s", prefix))
				goto cameraFound
			}
		}
	}
cameraFound:

	// Negative signals (check folder name, not full path to avoid /tmp/ false positives).
	if strings.Contains(pathLower, ".git") || strings.Contains(pathLower, "node_modules") {
		score -= 50
		reasons = append(reasons, "source code")
	}

	for _, cache := range []string{"cache", "tmp", "temp", "thumbnails", ".cache"} {
		if strings.Contains(baseName, cache) {
			score -= 40
			reasons = append(reasons, "cache folder")
			break
		}
	}

	for _, assets := range []string{"icons", "sprites", "assets", "public"} {
		if strings.Contains(baseName, assets) {
			score -= 30
			reasons = append(reasons, "app assets")
			break
		}
	}

	for _, sys := range []string{"library", "system", "windows", "program files"} {
		if strings.Contains(baseName, sys) {
			score -= 40
			reasons = append(reasons, "system folder")
			break
		}
	}

	if mediaRatio < 0.05 && s.TotalCount > 0 {
		score -= 30
		reasons = append(reasons, "low media ratio")
	}

	if s.MediaCount < 3 {
		score -= 15
		reasons = append(reasons, "few media files")
	}

	// Clamp to 0-100.
	if score < 0 {
		score = 0
	} else if score > 100 {
		score = 100
	}

	return score, reasons
}

// identifyPhotoFolders samples top-level subdirectories and scores them.
func identifyPhotoFolders(root string, minScore int) (qualifying []ScoredFolder, skipped []ScoredFolder, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}

	var (
		qualifyingFolders []ScoredFolder
		skippedFolders    []ScoredFolder
	)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirName := entry.Name()
		// Skip dot-prefixed directories (hidden).
		if strings.HasPrefix(dirName, ".") {
			continue
		}

		dirPath := filepath.Join(root, dirName)

		// Sample up to 4 levels deep to handle Archive/2020/Jan/Vacation style nesting.
		sample := sampleFolder(dirPath, 4)
		score, reasons := scoreFolderSample(dirPath, sample)

		scored := ScoredFolder{
			Path:    dirPath,
			Score:   score,
			Reasons: reasons,
			Sample:  sample,
		}

		if score >= minScore {
			qualifyingFolders = append(qualifyingFolders, scored)
		} else {
			skippedFolders = append(skippedFolders, scored)
		}
	}

	// Sort both by score descending.
	sort.Slice(qualifyingFolders, func(i, j int) bool {
		return qualifyingFolders[i].Score > qualifyingFolders[j].Score
	})
	sort.Slice(skippedFolders, func(i, j int) bool {
		return skippedFolders[i].Score > skippedFolders[j].Score
	})

	return qualifyingFolders, skippedFolders, nil
}
