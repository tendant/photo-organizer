// photo-organizer: scan a folder and generate a photo manifest CSV.
//
// Usage:
//
//	photo-organizer [directory]            scan directory, manifest written inside it
//	photo-organizer scan [directory]       explicit scan subcommand
//	photo-organizer dups a.csv b.csv       compare manifests across machines
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// =============================================================================
// Helper Functions for Consistency
// =============================================================================

// confirmPrompt asks user for y/n confirmation
func confirmPrompt(message string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", message)
	var response string
	fmt.Scanln(&response)
	return response == "y" || response == "Y"
}

// userHomeDir returns the user's home directory (cross-platform safe)
func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot determine home directory: %v\n", err)
		return "."
	}
	return home
}

// =============================================================================
// Supported File Types
// =============================================================================

var photoExts = map[string]bool{
	".jpg":     true,
	".jpeg":    true,
	".png":     true,
	".gif":     true,
	".heic":    true,
	".heif":    true,
	".hif":     true,
	".dng":     true,
	".arw":     true,
	".cr2":     true,
	".cr3":     true,
	".nef":     true,
	".raf":     true,
	".orf":     true,
	".rw2":     true,
	".tif":     true,
	".tiff":    true,
	".webp":    true,
	".raw":     true,
	".afphoto": true, // Affinity Photo project file
}

var videoExts = map[string]bool{
	".mp4":  true,
	".mov":  true,
	".m4v":  true,
	".avi":  true,
	".mkv":  true,
	".3gp":  true,
	".hevc": true,
	".mts":  true,
}

var audioExts = map[string]bool{
	".wav": true,
	".mp3": true,
}

var sidecarExts = map[string]bool{
	".lrf":  true, // Lightroom catalog
	".xmp":  true, // XMP metadata
	".aae":  true, // Apple photo edits
	".json": true, // JSON metadata/config
	".xml":  true, // XML metadata/config
}

// =============================================================================
// Date Extraction
// =============================================================================

var datePatterns = []struct {
	regex  *regexp.Regexp
	layout string
}{
	{regexp.MustCompile(`DJI_(\d{8})`), "20060102"},
	{regexp.MustCompile(`^(\d{8})_C\d+`), "20060102"},
	{regexp.MustCompile(`(\d{8})_\d{6}`), "20060102"},
	{regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`), "2006-01-02"},
	{regexp.MustCompile(`(\d{8})`), "20060102"},
}

func getDateFromFilename(filename string) (time.Time, bool) {
	for _, p := range datePatterns {
		if m := p.regex.FindStringSubmatch(filename); len(m) >= 2 {
			if t, err := time.Parse(p.layout, m[1]); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// =============================================================================
// Hashing + Date (single file open)
// =============================================================================

const sampleSize = 32 * 1024 // 32KB per sample position

// processFile computes a sampled partial hash by reading the first 32KB and
// last 32KB of the file (64KB total). Sampling the tail captures actual image
// data, which is unique per shot even when camera headers are identical —
// dramatically reducing false collisions for RAW/HIF files from the same camera.
func processFile(path string) (hash string, captureDate time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", getDateFallback(path)
	}
	defer f.Close()

	// Read first 32KB — used for both the hash and EXIF extraction.
	firstBuf := make([]byte, sampleSize)
	firstN, _ := f.Read(firstBuf)
	firstBuf = firstBuf[:firstN]

	h := md5.New()
	h.Write(firstBuf)

	// Also sample the last 32KB for files larger than 32KB.
	if info, err := f.Stat(); err == nil && info.Size() > int64(sampleSize) {
		lastBuf := make([]byte, sampleSize)
		if _, err := f.Seek(-int64(sampleSize), io.SeekEnd); err == nil {
			n, _ := f.Read(lastBuf)
			h.Write(lastBuf[:n])
		}
	}

	hash = fmt.Sprintf("%x", h.Sum(nil))

	// Try EXIF from the first 32KB buffer.
	if photoExts[strings.ToLower(filepath.Ext(path))] {
		if x, err := exif.Decode(bytes.NewReader(firstBuf)); err == nil {
			if t, err := x.DateTime(); err == nil {
				return hash, t
			}
		}
	}

	return hash, getDateFallback(path)
}

// computeFullHash hashes the entire file and returns its MD5.
func getDateFallback(path string) time.Time {
	if t, ok := getDateFromFilename(filepath.Base(path)); ok {
		return t
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Now()
}

// CacheEntry holds previously computed values for a file keyed by relative path.
type CacheEntry struct {
	SizeBytes   int64
	PartialHash string
	FullHash    string
	CaptureDate time.Time
}

// loadCache reads an existing manifest and returns a map of relPath → CacheEntry
// so unchanged files can skip re-processing.
func loadCache(manifestFile string) map[string]CacheEntry {
	cache := make(map[string]CacheEntry)
	f, err := os.Open(manifestFile)
	if err != nil {
		return cache
	}
	defer f.Close()
	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil || len(records) < 2 {
		return cache
	}
	colIdx := make(map[string]int)
	for i, h := range records[0] {
		colIdx[h] = i
	}

	// Validate that required columns are present.
	for _, required := range []string{"relative_path", "partial_hash"} {
		if _, ok := colIdx[required]; !ok {
			// Also accept file_hash as fallback for partial_hash in very old manifests.
			if required == "partial_hash" {
				if _, ok := colIdx["file_hash"]; ok {
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "Warning: %s: missing required column %q (not a valid manifest?)\n", manifestFile, required)
			return cache
		}
	}

	col := func(row []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}
	for _, row := range records[1:] {
		relPath := col(row, "relative_path")
		if relPath == "" {
			continue
		}
		sizeStr := col(row, "file_size_bytes")
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || sizeStr == "" {
			continue
		}
		var captureDate time.Time
		if s := col(row, "capture_date"); s != "" {
			captureDate, _ = time.Parse("2006:01:02 15:04:05", s)
		}
		// partial_hash is the new column; fall back to file_hash for old manifests.
		partialHash := col(row, "partial_hash")
		if partialHash == "" {
			partialHash = col(row, "file_hash")
		}
		if partialHash == "" {
			continue
		}
		cache[relPath] = CacheEntry{
			SizeBytes:   size,
			PartialHash: partialHash,
			FullHash:    col(row, "full_hash"),
			CaptureDate: captureDate,
		}
	}
	return cache
}

// =============================================================================
// Scanning
// =============================================================================

type FileInfo struct {
	Path        string
	Size        int64
	ModTime     time.Time
	CaptureDate time.Time
	PartialHash string
	FullHash    string // empty until a collision is detected
}

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

func isMediaFile(ext string) bool {
	ext = strings.ToLower(ext)
	return photoExts[ext] || videoExts[ext] || audioExts[ext] || sidecarExts[ext]
}

func shouldSkipFile(path string) bool {
	name := filepath.Base(path)
	// Skip macOS system files
	if name == ".DS_Store" {
		return true
	}
	// Skip Syncthing sync metadata (cross-platform check)
	if strings.HasPrefix(name, ".stfolder") {
		return true
	}
	normPath := filepath.ToSlash(path)
	if strings.Contains(normPath, "/.stfolder/") {
		return true
	}
	return false
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

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
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

type rawFile struct {
	path    string
	size    int64
	modTime time.Time
}

type ScanStats struct {
	Found      int
	Cached     int
	New        int
	Updated    int
	Symlinks   int
	FullHashed int
	TotalBytes int64
}

func scanDirectory(dir string, cache map[string]CacheEntry, photoIgnore *PhotoIgnore) ([]FileInfo, ScanStats, error) {
	var stats ScanStats
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, stats, fmt.Errorf("directory not found: %s", dir)
	}

	// Phase 1: walk the directory tree and collect file paths (fast).
	var raw []rawFile
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// Skip symlinks — they may point outside the scan tree or cause loops.
		if info.Mode()&os.ModeSymlink != 0 {
			// Clear the current progress line before printing the warning.
			fmt.Fprintf(os.Stderr, "\r%-80s\n  skipping symlink: %s\n", "", path)
			stats.Symlinks++
			return nil
		}
		if info.IsDir() {
			// Skip root directory and all other directories
			if path == dir {
				// Skip the root directory itself (don't process as a file)
				return nil
			}
			// Skip dotfiles and system/camera folders
			name := info.Name()
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			// Skip camera-specific system folders
			if name == "PRIVATE" || name == "THMBNL" || name == "AVF_INFO" || name == ".fseventsd" {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(dir, path)
			// Truncate long paths so they don't overflow the line width.
			label := "walking: " + rel
			if len(label) > 78 {
				label = "walking: ..." + rel[len(rel)-65:]
			}
			fmt.Fprintf(os.Stderr, "\r  %-78s", label)
			return nil
		}
		// Skip OS junk (hardcoded)
		if shouldSkipFile(path) {
			return nil
		}
		// Skip other dotfiles, but always include .photoignore
		if strings.HasPrefix(info.Name(), ".") && info.Name() != ".photoignore" {
			return nil
		}
		// Apply .photoignore patterns (user-defined exclusions)
		if photoIgnore != nil && photoIgnore.ShouldSkip(path) {
			return nil
		}
		raw = append(raw, rawFile{path, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, stats, err
	}
	stats.Found = len(raw)
	fmt.Fprintf(os.Stderr, "\r  %-78s\n", fmt.Sprintf("%s files found, processing...", formatCount(len(raw))))

	// Phase 2: extract EXIF dates and compute hashes using work queue.
	// Send work as (index, rawFile) pairs, receive results with explicit index pairing.
	numWorkers := runtime.NumCPU()
	if numWorkers > 4 {
		numWorkers = 4
	}

	type workItem struct {
		idx int
		rf  rawFile
	}

	type workResult struct {
		idx int
		fi  FileInfo
	}

	files := make([]FileInfo, len(raw))
	workQueue := make(chan workItem, numWorkers)
	resultQueue := make(chan workResult, numWorkers)

	var wg sync.WaitGroup
	var processed atomic.Int64

	// Start fixed number of workers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for work := range workQueue {
				relPath, _ := filepath.Rel(dir, work.rf.path)
				fi := FileInfo{Path: work.rf.path, Size: work.rf.size, ModTime: work.rf.modTime}

				entry, hasCached := cache[relPath]
				if hasCached && entry.SizeBytes == work.rf.size {
					fi.PartialHash = entry.PartialHash
					fi.FullHash = entry.FullHash
					fi.CaptureDate = entry.CaptureDate
				} else {
					fi.PartialHash, fi.CaptureDate = processFile(work.rf.path)
					fi.FullHash = ""
				}

				resultQueue <- workResult{work.idx, fi}

				n := processed.Add(1)
				if n%100 == 0 {
					fmt.Fprintf(os.Stderr, "\r  %-78s",
						fmt.Sprintf("%s / %s processed...", formatCount(int(n)), formatCount(len(raw))))
				}
			}
		}()
	}

	// Send work to queue
	go func() {
		for i, rf := range raw {
			workQueue <- workItem{i, rf}
		}
		close(workQueue)
	}()

	// Collect results with explicit index pairing (must be part of wg)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < len(raw); i++ {
			res := <-resultQueue
			files[res.idx] = res.fi
		}
	}()

	wg.Wait()
	fmt.Fprintf(os.Stderr, "\r  %-78s\n",
		fmt.Sprintf("%s / %s processed", formatCount(len(raw)), formatCount(len(raw))))

	// No full hash phase - we only use partial hash for deduplication
	// (size + partial hash = unique signature)

	for _, fi := range files {
		stats.TotalBytes += fi.Size
	}

	return files, stats, nil
}

// =============================================================================
// Manifest
// =============================================================================

// machineID returns a stable, unique machine identifier of the form
// "Ls-MBP-a3f7c2". It is computed once and cached in ~/manifests/machine-id
// so it never changes even if the hostname is later renamed.
// resolveMachineID returns the machine ID with priority:
// 1. Explicit flag value (if provided and non-empty)
// 2. ./machine-id file in current working directory (if exists)
// 3. ~/manifests/machine-id (via machineID())
func resolveMachineID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}

	// Check for machine-id file in current directory
	cwd, err := os.Getwd()
	if err == nil {
		localPath := filepath.Join(cwd, "machine-id")
		if data, err := os.ReadFile(localPath); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}

	// Fall back to global machine ID
	return machineID()
}

func machineID() string {
	newPath := machineIDFile()
	oldPath := filepath.Join(userHomeDir(), ".photo-organizer-id")

	// Migrate old dotfile to new location if needed.
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if data, err := os.ReadFile(oldPath); err == nil {
			os.MkdirAll(filepath.Dir(newPath), 0755)
			if os.WriteFile(newPath, data, 0644) == nil {
				os.Remove(oldPath)
			}
		}
	}

	// Return cached ID if it exists.
	if data, err := os.ReadFile(newPath); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}

	// Build a new ID: short hostname + 6-char hardware UUID suffix.
	id := buildMachineID()

	// Persist it so it never changes.
	os.MkdirAll(filepath.Dir(newPath), 0755)
	os.WriteFile(newPath, []byte(id+"\n"), 0644)
	return id
}

func buildMachineID() string {
	name := localHostName()
	suffix := hardwareUUIDSuffix()
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// localHostName returns the user-set computer name, falling back to the
// short hostname (first label only, stripping domain/Tailscale suffixes).
func localHostName() string {
	// macOS: LocalHostName is stable and not network-dependent.
	if out, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	if h, err := os.Hostname(); err == nil {
		return strings.SplitN(h, ".", 2)[0]
	}
	return "unknown"
}

// hardwareUUIDSuffix returns the first 6 hex chars of the hardware UUID.
// On macOS this comes from ioreg; on Linux from /etc/machine-id.
func hardwareUUIDSuffix() string {
	// macOS: line format is:  "IOPlatformUUID" = "XXXXXXXX-XXXX-..."
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, "IOPlatformUUID") {
				continue
			}
			// Extract the value between the last pair of quotes.
			if i := strings.LastIndex(line, "\""); i > 0 {
				if j := strings.LastIndex(line[:i], "\""); j >= 0 {
					uuid := line[j+1 : i]
					raw := strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
					if len(raw) >= 6 {
						return raw[:6]
					}
				}
			}
		}
	}
	// Linux
	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(data)); len(id) >= 6 {
			return id[:6]
		}
	}
	return ""
}

// manifestFilename builds a unique CSV filename encoding the machine name and
// scan path, so manifests from different machines/folders don't overwrite each
// other when collected in the same directory.
// Example: photo_manifest_macbook-pro_Users_lei_Photos.csv
func manifestFilename(machine, absPath string) string {
	// Sanitize: replace path separators and non-alphanumeric chars with underscores.
	sanitize := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		// Collapse consecutive underscores and trim leading/trailing ones.
		result := strings.Trim(b.String(), "_")
		for strings.Contains(result, "__") {
			result = strings.ReplaceAll(result, "__", "_")
		}
		return result
	}
	m := sanitize(machine)
	p := sanitize(absPath)
	// Cap total length to keep filenames reasonable.
	if len(p) > 60 {
		p = p[len(p)-60:]
	}
	return fmt.Sprintf("photo_manifest_%s_%s.csv", m, p)
}

type ManifestStats struct {
	New     int
	Updated int
	Pruned  int
}

// backupManifest copies manifestFile to _backups/<name>_YYYYMMDD_HHMMSS.csv
// and keeps only the 5 most recent backups for that manifest.
func backupManifest(manifestFile string) error {
	if _, err := os.Stat(manifestFile); os.IsNotExist(err) {
		return nil // nothing to back up yet
	}

	backupDir := filepath.Join(filepath.Dir(manifestFile), "_backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return err
	}

	stem := strings.TrimSuffix(filepath.Base(manifestFile), ".csv")
	timestamp := time.Now().Format("20060102_150405")
	dst := filepath.Join(backupDir, stem+"_"+timestamp+".csv")

	src, err := os.Open(manifestFile)
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, src); err != nil {
		return err
	}

	// Prune old backups — keep only the 5 most recent for this manifest.
	pattern := filepath.Join(backupDir, stem+"_*.csv")
	backups, _ := filepath.Glob(pattern)
	sort.Strings(backups)
	const keepN = 5
	if len(backups) > keepN {
		for _, old := range backups[:len(backups)-keepN] {
			os.Remove(old)
		}
	}
	return nil
}

func updateManifest(scanDir string, files []FileInfo, manifestFile string, machineName string, prune bool) (ManifestStats, error) {
	var mstats ManifestStats
	fmt.Fprintf(os.Stderr, "Updating manifest: %s\n", filepath.Base(manifestFile))
	manifestDir := filepath.Dir(manifestFile)
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return mstats, err
	}

	// Always use the current header definition — never preserve old headers.
	headers := []string{
		"filename",
		"relative_path",
		"file_size_bytes",
		"file_size_mb",
		"file_modified",
		"capture_date",
		"camera_make",
		"camera_model",
		"partial_hash",
		"full_hash",
		"extension",
		"scan_date",
		"scan_path",
		"machine_name",
	}

	// Build a column index for the new (current) headers.
	headerIdx := make(map[string]int)
	for i, h := range headers {
		headerIdx[h] = i
	}

	// Load existing entries, normalising each row to the current schema.
	// Old manifests used different column names/positions; we re-map by header
	// name so the on-disk data is always correct after the first re-scan.
	existing := make(map[string][]string)
	if f, err := os.Open(manifestFile); err == nil {
		r := csv.NewReader(f)
		readStart := time.Now()
		records, err := r.ReadAll()
		f.Close()
		if err != nil {
			return mstats, fmt.Errorf("read existing manifest %s: %w", manifestFile, err)
		}
		if len(records) > 1 {
			fmt.Fprintf(os.Stderr, "  Loaded existing manifest (%d entries) in %v\n", len(records)-1, time.Since(readStart))
		}
		if len(records) < 1 {
			goto doneLoading
		}
		// Build source column index from the manifest's own header row.
		srcIdx := make(map[string]int)
		for i, h := range records[0] {
			srcIdx[h] = i
		}
		srcCol := func(row []string, name string) string {
			i, ok := srcIdx[name]
			if !ok || i >= len(row) {
				return ""
			}
			return row[i]
		}
		for _, row := range records[1:] {
			if len(row) < 2 {
				continue
			}
			relPath := srcCol(row, "relative_path")
			if relPath == "" {
				continue
			}
			// Resolve partial_hash: new column name, or fall back to old file_hash.
			partialHash := srcCol(row, "partial_hash")
			if partialHash == "" {
				partialHash = srcCol(row, "file_hash")
			}
			// Rebuild row in current schema order.
			newRow := make([]string, len(headers))
			newRow[headerIdx["filename"]] = srcCol(row, "filename")
			newRow[headerIdx["relative_path"]] = relPath
			newRow[headerIdx["file_size_bytes"]] = srcCol(row, "file_size_bytes")
			newRow[headerIdx["file_size_mb"]] = srcCol(row, "file_size_mb")
			newRow[headerIdx["file_modified"]] = srcCol(row, "file_modified")
			newRow[headerIdx["capture_date"]] = srcCol(row, "capture_date")
			newRow[headerIdx["camera_make"]] = srcCol(row, "camera_make")
			newRow[headerIdx["camera_model"]] = srcCol(row, "camera_model")
			newRow[headerIdx["partial_hash"]] = partialHash
			newRow[headerIdx["full_hash"]] = srcCol(row, "full_hash")
			newRow[headerIdx["extension"]] = srcCol(row, "extension")
			newRow[headerIdx["scan_date"]] = srcCol(row, "scan_date")
			newRow[headerIdx["scan_path"]] = srcCol(row, "scan_path")
			newRow[headerIdx["machine_name"]] = srcCol(row, "machine_name")
			existing[relPath] = newRow
		}
	}
doneLoading:

	newCount, updatedCount := 0, 0
	for i, fi := range files {
		if len(files) > 1000 && (i+1)%10000 == 0 {
			fmt.Fprintf(os.Stderr, "  Processing: %d / %d files\n", i+1, len(files))
		}
		relPath, _ := filepath.Rel(scanDir, fi.Path)
		if row, exists := existing[relPath]; exists {
			storedSize, err := strconv.ParseInt(row[headerIdx["file_size_bytes"]], 10, 64)
			if err != nil {
				continue
			}
			sizeChanged := storedSize != fi.Size
			hashChanged := row[headerIdx["partial_hash"]] != fi.PartialHash ||
				row[headerIdx["full_hash"]] != fi.FullHash

			if sizeChanged {
				row[headerIdx["file_size_bytes"]] = fmt.Sprintf("%d", fi.Size)
				row[headerIdx["file_size_mb"]] = fmt.Sprintf("%.2f", float64(fi.Size)/(1024*1024))
				row[headerIdx["file_modified"]] = fi.ModTime.Format("2006-01-02 15:04:05")
				row[headerIdx["capture_date"]] = fi.CaptureDate.Format("2006:01:02 15:04:05")
				row[headerIdx["partial_hash"]] = fi.PartialHash
				row[headerIdx["full_hash"]] = fi.FullHash
				row[headerIdx["scan_date"]] = time.Now().Format("2006-01-02 15:04:05")
				existing[relPath] = row
				updatedCount++
			} else if hashChanged {
				row[headerIdx["partial_hash"]] = fi.PartialHash
				row[headerIdx["full_hash"]] = fi.FullHash
				existing[relPath] = row
				updatedCount++
			}
			continue
		}
		existing[relPath] = []string{
			filepath.Base(fi.Path),
			relPath,
			fmt.Sprintf("%d", fi.Size),
			fmt.Sprintf("%.2f", float64(fi.Size)/(1024*1024)),
			fi.ModTime.Format("2006-01-02 15:04:05"),
			fi.CaptureDate.Format("2006:01:02 15:04:05"),
			"", "", // camera make/model (not yet extracted)
			fi.PartialHash,
			fi.FullHash,
			strings.ToLower(filepath.Ext(fi.Path)),
			time.Now().Format("2006-01-02 15:04:05"),
			scanDir,
			machineName,
		}
		newCount++
	}

	// For LOCAL manifests: scan is authoritative — remove entries for files not found.
	// For REMOTE manifests: keep them (remote might be offline but files still exist there).
	// We determine "local" by checking if scanDir is readable on this machine.
	isLocal := true
	if _, err := os.Stat(scanDir); err != nil {
		// Can't read scanDir on this machine, assume it's a remote backup location
		isLocal = false
	}

	// Apply pruning before write (so we write the final state).
	// Only prune LOCAL manifests (scan is authoritative); keep REMOTE manifests as-is.
	if prune && isLocal {
		scanned := make(map[string]bool, len(files))
		for _, fi := range files {
			relPath, _ := filepath.Rel(scanDir, fi.Path)
			scanned[relPath] = true
		}
		wouldPrune := 0
		for relPath := range existing {
			if !scanned[relPath] {
				wouldPrune++
			}
		}
		// Safety: if pruning would remove more than 50% of existing entries,
		// something is wrong (folder moved, volume unmounted, etc) — skip and warn.
		if len(existing) > 0 && wouldPrune*100/len(existing) > 50 {
			fmt.Fprintf(os.Stderr, "⚠  Skipping prune: would remove %s of %s entries (>50%%).\n",
				formatCount(wouldPrune), formatCount(len(existing)))
			fmt.Fprintf(os.Stderr, "   Is the folder still at %s? Re-run with --prune after verifying.\n", scanDir)
		} else {
			for relPath := range existing {
				if !scanned[relPath] {
					delete(existing, relPath)
					mstats.Pruned++
				}
			}
		}
	} else if prune && !isLocal {
		fmt.Fprintf(os.Stderr, "⚠  Not pruning remote manifest (remote location %q not accessible)\n", scanDir)
		fmt.Fprintf(os.Stderr, "   Remote backups are kept as-is for safety.\n")
	}

	mstats.New = newCount
	mstats.Updated = updatedCount

	// Back up the existing manifest before overwriting.
	if err := backupManifest(manifestFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not back up manifest: %v\n", err)
	}

	// Write manifest atomically: write to temp file first, then rename.
	// This ensures the old manifest is never truncated before the new one is complete.
	tmpFile, err := os.CreateTemp(filepath.Dir(manifestFile), ".manifest-tmp-*")
	if err != nil {
		return mstats, err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // clean up temp file if we error

	w := csv.NewWriter(tmpFile)
	w.Write(headers)

	var paths []string
	for p := range existing {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Show progress for all manifests
	fmt.Fprintf(os.Stderr, "  Writing %d entries to manifest...\n", len(paths))

	for i, p := range paths {
		w.Write(existing[p])
		// Show progress periodically
		if len(paths) > 100 && (i+1)%10000 == 0 {
			fmt.Fprintf(os.Stderr, "    %d / %d written\n", i+1, len(paths))
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmpFile.Close()
		return mstats, fmt.Errorf("write manifest %s: %w", manifestFile, err)
	}

	if err := tmpFile.Close(); err != nil {
		return mstats, fmt.Errorf("close manifest %s: %w", manifestFile, err)
	}

	// Atomic rename: either old file stays intact or new one is in place, never both truncated.
	if err := os.Rename(tmpPath, manifestFile); err != nil {
		return mstats, fmt.Errorf("finalize manifest %s: %w", manifestFile, err)
	}

	if len(existing) > 100000 {
		fmt.Fprintf(os.Stderr, "⚠  Manifest has %s entries — consider scanning subfolders separately with --root to split it up.\n",
			formatCount(len(existing)))
	}

	return mstats, nil
}

// =============================================================================
// Main / Subcommand Dispatch
// suggestCommand finds similar command names for typo suggestions
func suggestCommand(typo string) string {
	commands := []string{
		"archive", "backup", "backup-missing", "check-backup", "collect",
		"dups", "dup-folders", "fix", "help", "list", "lookup",
		"machines", "manifests", "restore", "scan", "search", "sign",
		"storage-plan", "storage-status", "verify-archive",
	}

	// Simple similarity: count matching prefixes and characters
	bestMatch := ""
	bestScore := 0

	for _, cmd := range commands {
		score := 0

		// Bonus for common prefix
		for i := 0; i < len(typo) && i < len(cmd); i++ {
			if typo[i] == cmd[i] {
				score += 2
			}
		}

		// Bonus if typo appears as substring
		if strings.Contains(cmd, typo) || strings.Contains(typo, cmd) {
			score += 3
		}

		// Penalize based on length difference
		lenDiff := len(cmd) - len(typo)
		if lenDiff < 0 {
			lenDiff = -lenDiff
		}
		score -= lenDiff

		if score > bestScore {
			bestScore = score
			bestMatch = cmd
		}
	}

	// Only suggest if there's a reasonable match
	if bestScore > 2 {
		return bestMatch
	}
	return ""
}

// =============================================================================

func main() {
	// Check for interrupted operations at startup
	DetectInterruptedOperations()

	if len(os.Args) >= 2 {
		cmd := os.Args[1]
		switch cmd {
		// Core workflow commands (14 total)
		case "scan":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "dups":
			runAnalyze(os.Args[2:])
			return
		case "dup-folders":
			runFindDuplicateFolders(os.Args[2:])
			return
		case "storage-status":
			runStorageStatus(os.Args[2:])
			return
		case "storage-plan":
			runStoragePlan(os.Args[2:])
			return
		case "backup":
			runBackup(os.Args[2:])
			return
		case "restore":
			runRestore(os.Args[2:])
			return
		case "list":
			runListArchives(os.Args[2:])
			return
		case "verify-archive":
			runVerifyBackup(os.Args[2:])
			return
		case "check-backup":
			runCheckBackupStatus(os.Args[2:])
			return
		case "sign":
			runSignManifest(os.Args[2:])
			return
		case "fix":
			runRepairManifest(os.Args[2:])
			return
		case "archive":
			runArchive(os.Args[2:])
			return
		case "manifests":
			runManifests(os.Args[2:])
			return
		case "machines":
			runMachines(os.Args[2:])
			return
		case "lookup":
			runLookup(os.Args[2:])
			return
		case "search":
			runSearch(os.Args[2:])
			return
		case "collect":
			runCollect(os.Args[2:])
			return
		case "backup-missing":
			runBackupMissing(os.Args[2:])
			return

		case "help", "--help", "-h":
			printUsage()
			return
		default:
			// Check if it looks like a command (not a directory path)
			if !strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, ".") {
				// Might be a typo'd command, suggest similar ones
				suggested := suggestCommand(cmd)
				fmt.Fprintf(os.Stderr, "Error: unknown command %q\n", cmd)
				if suggested != "" {
					fmt.Fprintf(os.Stderr, "Did you mean: %s?\n\n", suggested)
				}
				fmt.Fprintf(os.Stderr, "Run 'photo-organizer help' for available commands.\n")
				os.Exit(1)
			}
			// Looks like a directory path, try to scan it
		}
		runScan(os.Args[1:])
		return
	}
	printUsage()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "photo-organizer — scan, deduplicate, and verify photo backups\n\n")
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan <folder> [--media-id ID] [--prune]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dups [manifest.csv ...]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders [--top N] [-s]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup <folder> <archive-root> [--new-only]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing <folder> --dest <target>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer check-backup <folder>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer verify-archive <manifest.csv>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer search [filters]\n\n")
	fmt.Fprintf(os.Stderr, "Other commands:\n")
	fmt.Fprintf(os.Stderr, "  archive, collect, fix, list, lookup, machines, manifests,\n")
	fmt.Fprintf(os.Stderr, "  restore, sign, storage-plan, storage-status\n\n")
	fmt.Fprintf(os.Stderr, "Examples:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan ~/Photos\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos /mnt/archive\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders --top 10 -s\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer check-backup ~/Photos\n")
}

// =============================================================================
// Archive and Delete Commands
// =============================================================================

// pruneManifestEntries removes entries for a folder from a manifest file
func pruneManifestEntries(manifestPath string, folderPath string) (int, error) {
	// Read CSV directly to preserve all columns
	f, err := os.Open(manifestPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	f.Close()
	if err != nil {
		return 0, err
	}

	if len(records) < 1 {
		return 0, nil // Empty manifest
	}

	// Get scan_path and relative_path column indices
	headers := records[0]
	scanPathIdx, relPathIdx := -1, -1
	for i, h := range headers {
		if h == "scan_path" {
			scanPathIdx = i
		} else if h == "relative_path" {
			relPathIdx = i
		}
	}

	if scanPathIdx < 0 || relPathIdx < 0 {
		return 0, fmt.Errorf("missing required columns")
	}

	// Filter rows: keep only those NOT under the archived folder
	prunedRecords := [][]string{headers} // Start with header
	prunedCount := 0

	for _, row := range records[1:] {
		if len(row) > relPathIdx && len(row) > scanPathIdx {
			filePath := filepath.Join(row[scanPathIdx], row[relPathIdx])
			// Keep only entries that are NOT under the folder being archived
			if strings.HasPrefix(filePath, folderPath+"/") || strings.HasPrefix(filePath, folderPath+string(filepath.Separator)) {
				prunedCount++
				continue // Skip this row
			}
		}
		prunedRecords = append(prunedRecords, row)
	}

	if prunedCount == 0 {
		return 0, nil // Nothing to prune
	}

	// Back up first
	if err := backupManifest(manifestPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not back up manifest: %v\n", err)
	}

	// Write atomically using temp file
	tmpFile, err := os.CreateTemp(filepath.Dir(manifestPath), ".manifest-tmp-*")
	if err != nil {
		return prunedCount, err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	w := csv.NewWriter(tmpFile)
	w.WriteAll(prunedRecords)
	w.Flush()
	if err := w.Error(); err != nil {
		tmpFile.Close()
		return prunedCount, err
	}
	tmpFile.Close()

	// Atomically rename
	if err := os.Rename(tmpPath, manifestPath); err != nil {
		return prunedCount, err
	}

	return prunedCount, nil
}

func runArchive(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer archive <folder-path> --dest <archive-dir>\n\n")
		fmt.Fprintf(os.Stderr, "Move folder to local archive directory and update manifests.\n")
		os.Exit(1)
	}

	sourceFolder := args[0]
	archiveDir := parseRequiredDestFlag(args)

	if archiveDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required\n")
		os.Exit(1)
	}

	// Resolve paths to absolute
	absSourceFolder, err := resolveExistingFolder(sourceFolder)
	if err != nil {
		if _, statErr := os.Stat(sourceFolder); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		}
		os.Exit(1)
	}
	absArchiveDir, err := filepath.Abs(archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid archive directory %q\n", archiveDir)
		os.Exit(1)
	}

	// Check archive directory exists or create it
	if err := os.MkdirAll(absArchiveDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create archive directory: %v\n", err)
		os.Exit(1)
	}

	// Create timestamped archive folder name (include time to avoid conflicts)
	folderName := filepath.Base(absSourceFolder)
	archiveFolderName := generateArchiveFolderName(folderName, time.Now())
	archiveFolder := filepath.Join(absArchiveDir, archiveFolderName)

	// === PREVIEW: Show what will happen ===
	fmt.Fprintf(os.Stderr, "\n📋 ARCHIVE PREVIEW\n")
	fmt.Fprintf(os.Stderr, "Source: %s\n", absSourceFolder)

	// Scan folder to show stats
	folderFiles, _, err := scanDirectory(absSourceFolder, make(map[string]CacheEntry), nil)
	if err == nil && len(folderFiles) > 0 {
		totalSize := int64(0)
		for _, f := range folderFiles {
			totalSize += f.Size
		}
		fmt.Fprintf(os.Stderr, "Files: %d, Size: %s\n", len(folderFiles), formatBytes(totalSize))
	}

	fmt.Fprintf(os.Stderr, "Archive to: %s\n", archiveFolder)
	fmt.Fprintf(os.Stderr, "\n⚠️  SAFETY CHECK - Before archiving, verify:\n")
	fmt.Fprintf(os.Stderr, "  1. You have 2+ valid backup copies (remote machines)\n")
	fmt.Fprintf(os.Stderr, "  2. Run: photo-organizer verify %s\n", absSourceFolder)
	fmt.Fprintf(os.Stderr, "  3. Confirm status shows '✓ SAFE TO ARCHIVE'\n")

	if !confirmPrompt("\nProceed?") {
		fmt.Fprintf(os.Stderr, "Cancelled.\n")
		os.Exit(0)
	}

	// Move the folder
	fmt.Fprintf(os.Stderr, "\n▶️  Moving %s → %s\n", absSourceFolder, archiveFolder)
	if err := os.Rename(absSourceFolder, archiveFolder); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to move folder: %v\n", err)
		os.Exit(1)
	}

	// Get machine name
	machineName := resolveMachineID("")

	// Rescan the parent of source folder to remove old entries (with --prune)
	sourceParent := filepath.Dir(absSourceFolder)
	fmt.Fprintf(os.Stderr, "\nUpdating manifest for %s (pruning removed files)...\n", sourceParent)
	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, sourceParent))

	files, _, err := scanDirectory(sourceParent, make(map[string]CacheEntry), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Could not refresh parent folder scan: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Stale entries may remain. Run 'photo-organizer cleanup-manifests' to clean them up.\n")
	} else {
		stats, err := updateManifest(sourceParent, files, manifestFile, machineName, true) // prune=true
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update manifest: %v\n", err)
		} else if stats.Pruned > 0 {
			fmt.Fprintf(os.Stderr, "✓ Removed %d stale entries from parent manifest\n", stats.Pruned)
		}
	}

	// Remove or prune old manifests for the archived folder
	fmt.Fprintf(os.Stderr, "Cleaning up old manifest entries...\n")
	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	if matches, err := filepath.Glob(filepath.Join(manifestDir, "*.csv")); err == nil {
		for _, manifestPath := range matches {
			// Read manifest to check if it references the old source folder
			if src, err := readManifest(manifestPath); err == nil {
				// Case 1: Manifest scanned the old folder or its subdirectories - remove it entirely
				if src.ScanPath == absSourceFolder || strings.HasPrefix(src.ScanPath, absSourceFolder+string(filepath.Separator)) {
					if err := os.Remove(manifestPath); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not remove old manifest %s: %v\n", manifestPath, err)
					}
					continue
				}

				// Case 2: Manifest scanned a parent directory - prune entries for the old folder
				if strings.HasPrefix(absSourceFolder, src.ScanPath+string(filepath.Separator)) {
					if prunedCount, err := pruneManifestEntries(manifestPath, absSourceFolder); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not prune manifest %s: %v\n", manifestPath, err)
					} else if prunedCount > 0 {
						fmt.Fprintf(os.Stderr, "✓ Pruned %d entries from %s\n", prunedCount, filepath.Base(manifestPath))
					}
				}
			}
		}
	}

	// Scan archive folder and update manifest (partial hashes only, no full hash computation)
	fmt.Fprintf(os.Stderr, "Scanning archive folder (partial hashes only)...\n")
	archiveParent := filepath.Dir(archiveFolder)
	manifestFile = filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, archiveParent))

	files, _, err = scanDirectory(archiveParent, make(map[string]CacheEntry), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Could not scan archive folder: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Updating manifest for archive location...\n")
		fmt.Fprintf(os.Stderr, "  Processing %d files...\n", len(files))
		startTime := time.Now()
		stats, err := updateManifest(archiveParent, files, manifestFile, machineName, false)
		elapsed := time.Since(startTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update archive manifest: %v\n", err)
		} else if stats.New > 0 || stats.Updated > 0 {
			fmt.Fprintf(os.Stderr, "✓ Updated archive manifest (%d new, %d updated, %d pruned) in %v\n", stats.New, stats.Updated, stats.Pruned, elapsed)
		} else {
			fmt.Fprintf(os.Stderr, "✓ Archive manifest up to date in %v\n", elapsed)
		}
	}

	fmt.Fprintf(os.Stderr, "\n✓ Folder archived to %s\n", archiveFolder)
	fmt.Fprintf(os.Stderr, "  Files are still tracked in manifest at new location\n")
}

// =============================================================================
// Archive Status (Show archived folders and deletion safety)
// =============================================================================

// =============================================================================
// Prune (Clean manifests for archived folder)
// =============================================================================

// =============================================================================
// Lookup (Find manifests containing a folder)
// =============================================================================

func runLookup(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP FILE/FOLDER - Show complete details\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer lookup <name-or-path>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"vacation.jpg\"\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"Vacation/\"\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer lookup \"2026/06/\"\n\n")
		fmt.Fprintf(os.Stderr, "Shows: location, scan path, machine, date, size, hash, backup status\n")
		os.Exit(1)
	}

	lookupPath := filepath.Clean(args[0])

	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return
	}

	type FileMatch struct {
		relativePath string
		scanPath     string
		machine      string
		fileSize     string
		hash         string
		fileModified string
		manifestPath string
	}
	var results []FileMatch

	// Search manifests for matching file/folder
	filename := filepath.Base(lookupPath)
	isFolder := strings.HasSuffix(lookupPath, "/")

	for _, manifestPath := range matches {
		src, err := readManifest(manifestPath)
		if err != nil || len(src.Rows) == 0 {
			continue
		}

		// Search for matching entries
		for _, row := range src.Rows {
			var matches bool
			if isFolder {
				// Folder search: check if entry starts with folder path
				searchPath := strings.TrimSuffix(lookupPath, "/")
				matches = strings.HasPrefix(row.RelativePath, searchPath+"/") || row.RelativePath == searchPath
			} else {
				// File search: match filename or path contains
				matches = strings.Contains(row.RelativePath, filename) || filepath.Base(row.RelativePath) == filename
			}

			if matches {
				hash := row.FullHash
				if hash == "" {
					hash = row.PartialHash
				}
				if len(hash) > 16 {
					hash = hash[:16] + "..."
				}
				results = append(results, FileMatch{
					relativePath: row.RelativePath,
					scanPath:     row.ScanPath,
					machine:      row.MachineName,
					fileSize:     formatSize(row.SizeBytes),
					hash:         hash,
					fileModified: row.FileModified,
					manifestPath: manifestPath,
				})
			}
		}
	}

	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n\n", lookupPath)
		fmt.Fprintf(os.Stderr, "❌ Not found in any manifests\n")
		return
	}

	// Display results
	fmt.Fprintf(os.Stderr, "\n🔍 LOOKUP: %s\n\n", lookupPath)
	fmt.Fprintf(os.Stderr, "Found %d match(es):\n\n", len(results))

	for i, match := range results {
		fmt.Fprintf(os.Stderr, "%d. %s\n", i+1, match.relativePath)
		fmt.Fprintf(os.Stderr, "   Scan path:   %s\n", match.scanPath)
		fmt.Fprintf(os.Stderr, "   Machine:     %s\n", match.machine)
		fmt.Fprintf(os.Stderr, "   Size:        %s\n", match.fileSize)
		fmt.Fprintf(os.Stderr, "   Hash:        %s\n", match.hash)
		fmt.Fprintf(os.Stderr, "   Modified:    %s\n\n", match.fileModified)
	}
}

// =============================================================================
// Remove Manifest (Delete manifest file for a scanned folder)
// =============================================================================

// =============================================================================
// Backup Missing Command
// =============================================================================

func runBackupMissing(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup-missing <folder> --dest <target>\n\n")
		fmt.Fprintf(os.Stderr, "Ensures all files in folder are backed up to target destination.\n")
		fmt.Fprintf(os.Stderr, "Only copies missing files via rsync.\n\n")
		fmt.Fprintf(os.Stderr, "Target formats: machine-id:/path or user@host:/path\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		os.Exit(1)
	}

	sourceFolder := args[0]
	destLocation := parseRequiredDestFlag(args)

	if destLocation == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required (e.g., user@host:/backups)\n")
		os.Exit(1)
	}

	// Resolve source folder to absolute path
	absSourceFolder, err := resolveExistingFolder(sourceFolder)
	if err != nil {
		if _, statErr := os.Stat(sourceFolder); statErr != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		}
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Checking which files need backup...\n")

	// Get local machine ID to distinguish local from remote backups
	localMachineID := resolveMachineID("")

	// Load all manifests
	sources := loadManifestSources(localMachineID)

	// Build hash index
	idx := buildHashIndex(sources)

	// Find missing files (files without non-removable backups)
	var missingFiles []string
	var missingCount int
	var missingSize int64

	filepath.WalkDir(absSourceFolder, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == absSourceFolder {
			return nil
		}

		info, _ := os.Stat(path)
		if info == nil {
			return nil
		}

		// Skip unwanted system/sync files
		if shouldSkipFile(path) {
			return nil
		}

		// Compute partial hash
		partialHash, _ := processFile(path)
		if partialHash == "" {
			return nil
		}

		if !hasIndependentBackup(sources, idx, localMachineID, partialHash, info.Size()) {
			// This file needs backing up
			relPath, _ := filepath.Rel(absSourceFolder, path)
			missingFiles = append(missingFiles, relPath)
			missingCount++
			missingSize += info.Size()
		}

		return nil
	})

	if missingCount == 0 {
		fmt.Fprintf(os.Stderr, "✓ All files already backed up\n")
		return
	}

	fmt.Fprintf(os.Stderr, "Backing up %d files (%.1f GB)...\n", missingCount, float64(missingSize)/(1024*1024*1024))

	// Look up remote machine config
	machines := loadMachinesConfig()
	remoteUserHost, remoteMachineID, remotePath, err := resolveBackupDestination(destLocation, machines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Available options:\n")
		for machID, sshHost := range machines {
			if !strings.HasPrefix(sshHost, "[removable]") {
				fmt.Fprintf(os.Stderr, "  - %s (machine-id: %s)\n", sshHost, machID)
			}
		}
		fmt.Fprintf(os.Stderr, "\nUsage examples:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max-acb605:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu-max:/backups\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing ~/Photos --dest ubuntu@192.168.1.100:/backups\n")
		os.Exit(1)
	}

	// Step 2: Create temporary file list for rsync
	tmpFile, err := os.CreateTemp("", "backup-missing-*.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create temp file: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmpFile.Name())

	for _, f := range missingFiles {
		fmt.Fprintln(tmpFile, f)
	}
	tmpFile.Close()

	// Copy files via rsync
	rsyncCmd := exec.Command("rsync", "-az", "--files-from="+tmpFile.Name(), absSourceFolder+"/", remoteUserHost+":"+remotePath+"/")
	rsyncCmd.Stdout = os.Stderr
	rsyncCmd.Stderr = os.Stderr
	if err := rsyncCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
		os.Exit(1)
	}

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "⚠ Backup copied, but manifest refresh was skipped.\n")
		fmt.Fprintf(os.Stderr, "  Add this host first: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		return
	}

	// Scan remote location
	scanCmd := fmt.Sprintf("cd %s && for path in photo-organizer ~/bin/photo-organizer /usr/local/bin/photo-organizer; do if command -v $path &>/dev/null || [ -f $path ]; then $path scan . --machine %s >/dev/null 2>&1; exit $?; fi; done; exit 1", shellQuote(remotePath), shellQuote(remoteMachineID))
	sshCmd := exec.Command("ssh", remoteUserHost, scanCmd)
	if err := sshCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: remote scan failed after copying files. photo-organizer must be installed on remote machine.\n")
		os.Exit(1)
	}

	// Collect updated manifests
	collectCmd := exec.Command("photo-organizer", "collect", "--from", remoteMachineID)
	collectCmd.Stdout = os.Stderr
	collectCmd.Stderr = os.Stderr
	if err := collectCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: files were copied, but manifest collection failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "✓ Backup complete\n")
}

// =============================================================================
// Cleanup Plan Command
// =============================================================================

func runManifests(args []string) {
	// Parse flags
	showStalled := false
	doCleanup := false
	removeFolder := ""

	for i := 0; i < len(args); i++ {
		if args[i] == "--stalled" {
			showStalled = true
		} else if args[i] == "--cleanup" {
			doCleanup = true
		} else if args[i] == "--remove" && i+1 < len(args) {
			removeFolder = args[i+1]
			i++
		}
	}

	// Handle --remove flag
	if removeFolder != "" {
		absPath, _ := filepath.Abs(removeFolder)
		manifestRoot := defaultManifestRoot()
		manifestDir := filepath.Join(manifestRoot, "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

		removed := 0
		for _, path := range matches {
			src, err := readManifest(path)
			if err == nil && src.ScanPath == absPath {
				if err := os.Remove(path); err == nil {
					fmt.Printf("✓ Removed manifest: %s\n", filepath.Base(path))
					removed++
				}
			}
		}

		if removed == 0 {
			fmt.Fprintf(os.Stderr, "No manifest found for: %s\n", absPath)
		} else {
			fmt.Printf("\n✓ Removed %d manifest(s)\n", removed)
		}
		return
	}

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		return
	}

	var sources []ManifestSource
	currentMachineID := machineID()

	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		// Mark as local or remote
		markManifestOrigin(&src, currentMachineID)
		// Check if stalled (source path no longer exists)
		if _, err := os.Stat(src.ScanPath); err != nil {
			src.IsStale = true
		}
		sources = append(sources, src)
	}

	// Handle --stalled flag
	if showStalled {
		var stalledSources []ManifestSource
		for _, src := range sources {
			if src.IsStale {
				stalledSources = append(stalledSources, src)
			}
		}

		if len(stalledSources) == 0 {
			fmt.Fprintf(os.Stderr, "✅ No stalled manifests found\n")
			return
		}

		fmt.Fprintf(os.Stderr, "📋 STALLED MANIFESTS (source paths no longer exist)\n\n")
		for _, src := range stalledSources {
			fmt.Printf("  %s\n", filepath.Base(src.FilePath))
			fmt.Printf("    Scan path: %s (missing)\n", src.ScanPath)
			fmt.Printf("    Machine:   %s\n", src.MachineName)
			fmt.Printf("    Files:     %d\n\n", len(src.Rows))
		}
		return
	}

	// Handle --cleanup flag
	if doCleanup {
		var stalledSources []ManifestSource
		var stalledPaths []string
		for i, src := range sources {
			if src.IsStale {
				stalledSources = append(stalledSources, src)
				stalledPaths = append(stalledPaths, matches[i])
			}
		}

		if len(stalledSources) == 0 {
			fmt.Fprintf(os.Stderr, "✅ No stalled manifests to clean up\n")
			return
		}

		fmt.Fprintf(os.Stderr, "Found %d stalled manifest(s):\n\n", len(stalledSources))
		for _, src := range stalledSources {
			fmt.Fprintf(os.Stderr, "  %s (%s - missing)\n", filepath.Base(src.FilePath), src.ScanPath)
		}
		fmt.Fprintf(os.Stderr, "\n")

		if !confirmPrompt("Remove stalled manifests?") {
			fmt.Fprintf(os.Stderr, "Cancelled.\n")
			return
		}

		removed := 0
		for _, path := range stalledPaths {
			if err := os.Remove(path); err == nil {
				fmt.Printf("✓ Removed: %s\n", filepath.Base(path))
				removed++
			}
		}
		fmt.Printf("\n✓ Removed %d stalled manifest(s)\n", removed)
		return
	}

	fmt.Fprintf(os.Stderr, "Listing all manifests and their origin...\n\n")

	// Sort by origin (local first), then by machine name, then scan path
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].IsLocal != sources[j].IsLocal {
			return sources[i].IsLocal // local first
		}
		if sources[i].MachineName != sources[j].MachineName {
			return sources[i].MachineName < sources[j].MachineName
		}
		return sources[i].ScanPath < sources[j].ScanPath
	})

	// Separate into local, removable, and remote (excluding empty)
	var localSources, removableSources, remoteSources, emptySources []ManifestSource
	for _, src := range sources {
		if len(src.Rows) == 0 {
			emptySources = append(emptySources, src)
		} else if isRemovablePath(src.ScanPath) {
			// Check removable path first (USB, SD cards, external drives)
			removableSources = append(removableSources, src)
		} else if src.IsLocal {
			// Local permanent storage
			localSources = append(localSources, src)
		} else {
			// Remote SSH machines
			remoteSources = append(remoteSources, src)
		}
	}

	// Display local manifests
	if len(localSources) > 0 {
		fmt.Fprintf(os.Stderr, "LOCAL MANIFESTS\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(localSources, "💻 lcl")
	}

	// Display removable manifests
	if len(removableSources) > 0 {
		if len(localSources) > 0 {
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "REMOVABLE MEDIA (USB, SD cards, etc.)\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(removableSources, "💾 rem")
	}

	// Display remote manifests
	if len(remoteSources) > 0 {
		if len(localSources) > 0 || len(removableSources) > 0 {
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "REMOTE MACHINES\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(remoteSources, "📦 rmt")
	}

	// Display empty manifests separately
	if len(emptySources) > 0 {
		fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
		fmt.Fprintf(os.Stderr, "EMPTY MANIFESTS (0 files - need investigation)\n")
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

		for i, src := range emptySources {
			// Extract path from filename
			scanPath := src.ScanPath
			if scanPath == "" {
				filename := filepath.Base(src.FilePath)
				pathEncoded := strings.TrimSuffix(filename, ".csv")
				pathEncoded = strings.TrimPrefix(pathEncoded, "photo_manifest_")
				if idx := strings.Index(pathEncoded, src.MachineName); idx == 0 {
					pathEncoded = pathEncoded[len(src.MachineName)+1:]
					scanPath = "/" + strings.ReplaceAll(pathEncoded, "_", "/")
				}
			}

			originMark := "📦 rmt"
			if src.IsLocal {
				originMark = "💻 lcl"
			}
			if isRemovablePath(src.ScanPath) {
				originMark = "💾 rem"
			}

			fmt.Fprintf(os.Stderr, "%d. %s %s @ %s\n", i+1, originMark, src.MachineName, scanPath)
			fmt.Fprintf(os.Stderr, "   File: %s\n\n", src.FilePath)
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	localCount := 0
	removableCount := 0
	remoteCount := 0
	emptyCount := 0
	for _, src := range sources {
		if len(src.Rows) == 0 {
			emptyCount++
		} else if isRemovablePath(src.ScanPath) {
			removableCount++
		} else if src.IsLocal {
			localCount++
		} else {
			remoteCount++
		}
	}
	fmt.Fprintf(os.Stderr, "Summary: %d local, %d removable, %d remote, %d empty\n", localCount, removableCount, remoteCount, emptyCount)
	fmt.Fprintf(os.Stderr, "💻 = Local machine       💾 = Removable media       📦 = Remote machines\n")
}

// Helper function to display manifest table
func displayManifestTable(sources []ManifestSource, originMark string) {
	for _, src := range sources {

		// Truncate long paths
		scanPath := src.ScanPath
		if len(scanPath) > 28 {
			scanPath = "..." + scanPath[len(scanPath)-25:]
		}

		// Calculate freshness status
		lastScanDate, _ := time.Parse("2006-01-02 15:04:05", src.LastScanned)
		daysSince := time.Since(lastScanDate).Hours() / 24

		var freshStatus string
		if daysSince < 1 {
			freshStatus = "✓ Fresh"
		} else if daysSince < 7 {
			freshStatus = fmt.Sprintf("⚡ %d day%s", int(daysSince), map[bool]string{true: "s", false: ""}[daysSince != 1])
		} else if daysSince < 30 {
			freshStatus = fmt.Sprintf("⚠  %d wks", int(daysSince/7))
		} else {
			freshStatus = fmt.Sprintf("🔴 %d mo", int(daysSince/30))
		}

		fmt.Fprintf(os.Stderr, "%s  %-20s %-28s %6d  %-17s %s\n",
			originMark,
			src.MachineName,
			scanPath,
			len(src.Rows),
			freshStatus,
			src.LastScanned,
		)
	}
}

// =============================================================================
// FileMeta represents metadata about a file for duplicate detection
type FileMeta struct {
	Path    string
	Size    int64
	ModTime time.Time
}

func runSearch(args []string) {
	runSearchAnalyze(args)
}
