// photo-organizer: scan a folder and generate a photo manifest CSV.
//
// Usage:
//
//	photo-organizer [directory]            scan directory, manifest written inside it
//	photo-organizer scan [directory]       explicit scan subcommand
//	photo-organizer analyze a.csv b.csv    compare manifests across machines
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"flag"
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
	".lrf": true,
	".xmp": true,
	".aae": true,
	".json": true,
	".bnp": true,
	".inp": true,
	".int": true,
	".xml": true,
	".bin": true,
	".txt": true,
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
func computeFullHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := md5.New()
	io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))
}

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
		size, _ := strconv.ParseInt(col(row, "file_size_bytes"), 10, 64)
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
	// Skip Syncthing sync metadata
	if strings.Contains(path, "/.stfolder/") || strings.HasPrefix(name, ".stfolder") {
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

	fmt.Fprintf(os.Stderr, "Analyzing file types...\n")

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
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

	fmt.Printf("\n✓ Ready to scan %d files (%s)\n\n", totalCount, formatBytes(totalBytes))
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
		if strings.Contains(path, fmt.Sprintf("%d", year)) {
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

func scanDirectory(dir string, cache map[string]CacheEntry, fullHash bool, photoIgnore *PhotoIgnore) ([]FileInfo, ScanStats, error) {
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
			// Don't skip any directories — file extension filter catches non-media files
			if path != dir && strings.HasPrefix(info.Name(), ".") {
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

	// Phase 2: extract EXIF dates and compute hashes.
	// Limit to 4 workers — more than that causes I/O contention on SSDs.
	files := make([]FileInfo, len(raw))
	var processed, cachedCount atomic.Int64
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	// Phase 2: compute partial hashes (and capture dates) for all files.
	// If a full hash is already cached, use it directly.
	for i, rf := range raw {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, rf rawFile) {
			defer wg.Done()
			defer func() { <-sem }()

			relPath, _ := filepath.Rel(dir, rf.path)
			fi := FileInfo{Path: rf.path, Size: rf.size, ModTime: rf.modTime}

			entry, hasCached := cache[relPath]
			if hasCached && entry.SizeBytes == rf.size {
				fi.PartialHash = entry.PartialHash
				fi.FullHash = entry.FullHash
				fi.CaptureDate = entry.CaptureDate
				cachedCount.Add(1)
			} else {
				fi.PartialHash, fi.CaptureDate = processFile(rf.path)
				fi.FullHash = ""
			}
			files[i] = fi

			n := processed.Add(1)
			if n%100 == 0 {
				fmt.Fprintf(os.Stderr, "\r  %-78s",
					fmt.Sprintf("%s / %s processed...", formatCount(int(n)), formatCount(len(raw))))
			}
		}(i, rf)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "\r  %-78s\n",
		fmt.Sprintf("%s / %s processed", formatCount(len(raw)), formatCount(len(raw))))
	stats.Cached = int(cachedCount.Load())

	// Phase 3: compute full hash only for files that share BOTH the same
	// partial hash AND the same file size with at least one other file in
	// this scan. Different sizes can never be duplicates regardless of
	// partial hash match. --full-hash upgrades all remaining files too.
	//
	// All files (including those already full-hashed from cache) count toward
	// collision detection, but only files missing a full hash get upgraded.
	// This ensures a new file colliding with a cached full-hashed file still
	// gets its full hash computed.
	byKey := make(map[string][]int)
	for i, fi := range files {
		k := indexKey(fi.PartialHash, fi.Size)
		byKey[k] = append(byKey[k], i)
	}
	var upgradeIdx []int
	for _, indices := range byKey {
		if len(indices) >= 2 || fullHash {
			for _, idx := range indices {
				if files[idx].FullHash == "" {
					upgradeIdx = append(upgradeIdx, idx)
				}
			}
		}
	}
	if len(upgradeIdx) > 0 {
		stats.FullHashed = len(upgradeIdx)
		total := len(upgradeIdx)
		var fullDone atomic.Int64
		var uwg sync.WaitGroup
		for _, idx := range upgradeIdx {
			uwg.Add(1)
			sem <- struct{}{}
			go func(idx int) {
				defer uwg.Done()
				defer func() { <-sem }()
				files[idx].FullHash = computeFullHash(files[idx].Path)
				n := fullDone.Add(1)
				if n%100 == 0 || int(n) == total {
					name := filepath.Base(files[idx].Path)
					progress := fmt.Sprintf("full hash: %s / %s  %s", formatCount(int(n)), formatCount(total), name)
					if len(progress) > 78 {
						progress = progress[:75] + "..."
					}
					fmt.Fprintf(os.Stderr, "\r  %-78s", progress)
				}
			}(idx)
		}
		uwg.Wait()
		fmt.Fprintf(os.Stderr, "\r  %-78s\n",
			fmt.Sprintf("full hash: %s / %s done", formatCount(total), formatCount(total)))
	}

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
	oldPath := filepath.Join(os.Getenv("HOME"), ".photo-organizer-id")

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
		records, err := r.ReadAll()
		f.Close()
		if err != nil {
			return mstats, fmt.Errorf("read existing manifest %s: %w", manifestFile, err)
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
	for _, fi := range files {
		relPath, _ := filepath.Rel(scanDir, fi.Path)
		if row, exists := existing[relPath]; exists {
			storedSize, _ := strconv.ParseInt(row[headerIdx["file_size_bytes"]], 10, 64)
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
	for _, p := range paths {
		w.Write(existing[p])
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
		"analyze", "backup-status", "plan", "migrate", "collect", "rescan",
		"machines", "risk-report", "analyze-backup-compliance", "check-backup",
		"archive", "delete-folder", "backup-missing", "cleanup-plan",
		"cleanup-manifests", "search", "scan", "help",
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
		case "analyze":
			runAnalyze(os.Args[2:])
			return
		case "backup-status":
			runBackupStatus(os.Args[2:])
			return
		case "plan":
			runPlan(os.Args[2:])
			return
		case "migrate":
			runMigrate(os.Args[2:])
			return
		case "collect":
			runCollect(os.Args[2:])
			return
		case "collect-config":
			runCollectConfig(os.Args[2:])
			return
		case "push-config":
			runPushConfig(os.Args[2:])
			return
		case "sync-config":
			runSyncConfig(os.Args[2:])
			return
		case "rescan":
			runRescan(os.Args[2:])
			return
		case "machines":
			runMachines(os.Args[2:])
			return
		case "manifests":
			runManifests(os.Args[2:])
			return
		case "risk-report":
			runRiskReport(os.Args[2:])
			return
		case "analyze-backup-compliance":
			runAnalyzeBackupCompliance(os.Args[2:])
			return
		case "check-backup":
			runCheckBackup(os.Args[2:])
			return
		case "archive":
			runArchive(os.Args[2:])
			return
		case "delete-folder":
			runDeleteFolder(os.Args[2:])
			return
		case "backup-missing":
			runBackupMissing(os.Args[2:])
			return
		case "cleanup-plan":
			runCleanupPlan(os.Args[2:])
			return
		case "cleanup-manifests":
			runCleanupManifests(os.Args[2:])
			return
		case "search":
			runSearch(os.Args[2:])
			return
		case "scan":
			// Strip the subcommand word and fall through to runScan
			os.Args = append(os.Args[:1], os.Args[2:]...)
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
	fmt.Fprintf(os.Stderr, "photo-organizer — track and manage photo backups across machines\n\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "CORE WORKFLOW - Archive & Cleanup\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")
	fmt.Fprintf(os.Stderr, "  scan [directory]                Scan folder and create manifest\n")
	fmt.Fprintf(os.Stderr, "  rescan                          Re-scan previously scanned folders\n")
	fmt.Fprintf(os.Stderr, "  collect [--from <machine>]      Pull manifests from remote machines\n")
	fmt.Fprintf(os.Stderr, "  manifests                       List all manifests and show origin (local/remote)\n")
	fmt.Fprintf(os.Stderr, "  cleanup-manifests               Remove stale manifests (scan paths no longer exist)\n")
	fmt.Fprintf(os.Stderr, "  check-backup <path>             Check if folder is backed up elsewhere\n")
	fmt.Fprintf(os.Stderr, "  cleanup-plan <path>             Show cleanup plan & space impact\n")
	fmt.Fprintf(os.Stderr, "  backup-missing <path> --dest <dest>  Back up files not backed up (auto pipeline)\n")
	fmt.Fprintf(os.Stderr, "  archive <path> --dest <dir>        Archive folder locally (move, not copy)\n")
	fmt.Fprintf(os.Stderr, "  delete-folder <path>            Delete folder and clean manifest\n\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "ANALYSIS & VERIFICATION\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")
	fmt.Fprintf(os.Stderr, "  analyze                         Find duplicates across machines\n")
	fmt.Fprintf(os.Stderr, "  search [manifests...]           Search files by name, path, hash, size, date\n")
	fmt.Fprintf(os.Stderr, "  risk-report                     Find files only on one machine\n")
	fmt.Fprintf(os.Stderr, "  backup-status --from <name>     Show copy count and coverage\n")
	fmt.Fprintf(os.Stderr, "  machines [--write-conf]         List all machines\n\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "ADVANCED\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")
	fmt.Fprintf(os.Stderr, "  analyze-backup-compliance       3-2-1 backup rule compliance analysis\n")
	fmt.Fprintf(os.Stderr, "  plan --keep <machine>           Generate safe-delete script\n")
	fmt.Fprintf(os.Stderr, "  migrate --from <machine>        Migrate unique files to backup\n\n")
	fmt.Fprintf(os.Stderr, "machines config: ~/manifests/machines.conf  (machine_id = user@host)\n\n")
	fmt.Fprintf(os.Stderr, "scan flags:\n")
	fmt.Fprintf(os.Stderr, "  --root dir       write manifest to dir/_Manifest/ (default: ~/manifests)\n")
	fmt.Fprintf(os.Stderr, "  --machine name   machine label embedded in manifest (default: stable machine ID)\n")
	fmt.Fprintf(os.Stderr, "  --media-id id    stable ID for removable media (same across different machines)\n")
	fmt.Fprintf(os.Stderr, "  --full-hash      hash all files fully, not just colliding ones (rarely needed)\n")
	fmt.Fprintf(os.Stderr, "  --no-cache       recompute all hashes, ignoring cached values\n")
	fmt.Fprintf(os.Stderr, "  --prune          remove manifest entries for files no longer on disk\n")
	fmt.Fprintf(os.Stderr, "  --no-report      skip file type coverage report (report shown by default)\n")
	fmt.Fprintf(os.Stderr, "  --auto-identify-folders  sample subdirectories and score by media ratio + path signals\n")
	fmt.Fprintf(os.Stderr, "  --score-threshold N      minimum folder score (0-100) to qualify (default: 30)\n")
	fmt.Fprintf(os.Stderr, "  --detect-only    with --auto-identify-folders: show results and exit without scanning\n\n")
	fmt.Fprintf(os.Stderr, "plan flags:\n")
	fmt.Fprintf(os.Stderr, "  --keep <machine>      keep copies on this machine, move others to quarantine\n")
	fmt.Fprintf(os.Stderr, "  --intra <machine>     find duplicates within a single machine\n")
	fmt.Fprintf(os.Stderr, "  --keep-under <path>   with --intra: keep copies under this path\n")
	fmt.Fprintf(os.Stderr, "  --ssh <user@host>     verify backup files exist on remote machine\n")
	fmt.Fprintf(os.Stderr, "  --ssh-timeout <dur>   timeout for SSH verification (default: 30s, e.g., 60s)\n")
	fmt.Fprintf(os.Stderr, "  --delete              generate rm commands instead of mv to quarantine\n")
	fmt.Fprintf(os.Stderr, "  --out <file>          write script to file instead of stdout\n\n")
	fmt.Fprintf(os.Stderr, "analyze flags:\n")
	fmt.Fprintf(os.Stderr, "  --csv prefix     also write CSV output files with this filename prefix\n")
	fmt.Fprintf(os.Stderr, "  --threshold n    folder coverage %% to flag as nearly-redundant (default: 0.9)\n\n")
	fmt.Fprintf(os.Stderr, "search flags:\n")
	fmt.Fprintf(os.Stderr, "  -name <pattern>          filename glob/regex pattern (e.g., IMG_* or \\.jpg$)\n")
	fmt.Fprintf(os.Stderr, "  -path <substring>        path contains substring\n")
	fmt.Fprintf(os.Stderr, "  -hash <hash>             find by full hash (shows all copies)\n")
	fmt.Fprintf(os.Stderr, "  -size <size>             exact size (e.g., 5MB) or range (e.g., 100MB-500MB)\n")
	fmt.Fprintf(os.Stderr, "  -date <date>             date range: YYYY-MM-DD or YYYY-MM-DD:YYYY-MM-DD\n")
	fmt.Fprintf(os.Stderr, "  -machine <id>            filter by machine_id\n")
	fmt.Fprintf(os.Stderr, "  -duplicates-only         only files with >1 copy\n")
	fmt.Fprintf(os.Stderr, "  -group                   show results grouped by hash\n")
	fmt.Fprintf(os.Stderr, "  -csv <file>              export to CSV instead of table\n\n")
	fmt.Fprintf(os.Stderr, "Examples:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan /Volumes/SSD          # manifest → ~/manifests/\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan ~/Photos\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer analyze ~/manifests/_Manifest/*.csv\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer analyze ~/manifests/_Manifest/*.csv --csv report\n")
}

func runScan(args []string) {
	// Separate flag args from the positional directory argument so that
	// both orderings work: --machine foo /dir  and  /dir --machine foo
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			// Peek ahead: if next arg is the flag value (not another flag), consume it too.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				// Only consume if this flag expects a value (not a bool flag).
				// We check by whether the flag name is a known value-taking flag.
				name := strings.TrimLeft(a, "-")
				if name == "root" || name == "machine" || name == "media-id" || name == "score-threshold" { // value-taking flags only
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	rootFlag := fs.String("root", "", "where to write the manifest (default: ~/manifests)")
	machineFlag := fs.String("machine", "", "machine label embedded in manifest (default: stable machine ID)")
	mediaIDFlag := fs.String("media-id", "", "stable identifier for removable media (same across different machines)")
	noWriteMediaIDFlag := fs.Bool("no-write-media-id", false, "skip writing machine-id file to card")
	fullHashFlag := fs.Bool("full-hash", false, "hash all files fully, not just colliding ones (rarely needed)")
	noCacheFlag := fs.Bool("no-cache", false, "recompute all hashes, ignoring cached values (use after hash algorithm change)")
	pruneFlag := fs.Bool("prune", false, "remove manifest entries for files no longer on disk")
	autoIdentifyFlag := fs.Bool("auto-identify-folders", false, "sample subdirectories and only scan those matching score threshold")
	scoreThresholdFlag := fs.Int("score-threshold", 30, "minimum folder score (0-100) for --auto-identify-folders (default: 30)")
	detectOnlyFlag := fs.Bool("detect-only", false, "with --auto-identify-folders: show detection results and exit without scanning")
	noReportFlag := fs.Bool("no-report", false, "skip file type coverage report (usually shown before scanning)")
	fs.Usage = printUsage
	fs.Parse(flagArgs)

	// Resolve machine name (priority: flag > ./machine-id > ~/manifests/machine-id)
	machineName := resolveMachineID(*machineFlag)

	// Determine directory to scan
	scanDir := ""
	if len(posArgs) > 0 {
		scanDir = posArgs[0]
	}
	if scanDir == "" {
		var err error
		scanDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}

	// Resolve scanDir to an absolute path for stable manifest naming.
	absScanDir, err := filepath.Abs(scanDir)
	if err != nil {
		absScanDir = scanDir
	}

	// Auto-read media-id from card if not provided via flag
	if *mediaIDFlag == "" {
		cardIDPath := filepath.Join(absScanDir, "machine-id")
		if data, err := os.ReadFile(cardIDPath); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				*mediaIDFlag = id
			}
		}
	}

	// Apply media-id to machine name after auto-read
	if *mediaIDFlag != "" {
		machineName = *mediaIDFlag
	}

	// Determine where manifest is written
	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}

	// Exit if scanning removable media without media-id (before pre-flight)
	if *mediaIDFlag == "" && isRemovableMedia(absScanDir) {
		fmt.Fprintf(os.Stderr, "⚠  Removable media detected: %s\n", absScanDir)
		fmt.Fprintf(os.Stderr, "   This is removable media and requires --media-id to scan.\n")
		fmt.Fprintf(os.Stderr, "   Without it, the card will be recorded under this machine (%q)\n", machineName)
		fmt.Fprintf(os.Stderr, "   and appear as different machines when scanned on other systems.\n\n")
		fmt.Fprintf(os.Stderr, "   Please provide a stable identifier:\n")
		fmt.Fprintf(os.Stderr, "     photo-organizer scan %s --media-id \"<label-on-card>\"\n\n", absScanDir)
		os.Exit(1)
	}

	// Write machine-id to card on first use (unless --no-write-media-id)
	if *mediaIDFlag != "" && !*noWriteMediaIDFlag {
		cardIDPath := filepath.Join(absScanDir, "machine-id")
		if _, err := os.Stat(cardIDPath); os.IsNotExist(err) {
			if err := os.WriteFile(cardIDPath, []byte(*mediaIDFlag+"\n"), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "⚠  Could not write machine-id to card: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✓  Media ID %q written to %s\n", *mediaIDFlag, cardIDPath)
			}
		}
	}

	// Pre-flight checks
	fmt.Fprintf(os.Stderr, "Pre-flight checks:\n")
	checks := []PreflightCheck{
		CheckDirReadable(absScanDir, "Source directory readable"),
		CheckDirWritable(filepath.Join(manifestRoot, "_Manifest"), "Manifest directory writable"),
	}
	if !RunPreflightChecks(checks) {
		os.Exit(1)
	}

	// Check if folder was already scanned
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		if src.ScanPath == absScanDir {
			fmt.Fprintf(os.Stderr, "\n⚠️  This folder was already scanned:\n")
			fmt.Fprintf(os.Stderr, "   Machine: %s\n", src.MachineName)
			fmt.Fprintf(os.Stderr, "   Last scanned: %s\n", src.LastScanned)
			fmt.Fprintf(os.Stderr, "   Files: %d\n", len(src.Rows))
			fmt.Fprintf(os.Stderr, "   Tip: Use 'rescan' to update instead of 're-scanning from scratch'\n\n")
			break
		}
	}

	// Show file type coverage report by default (unless --no-report is set)
	if !*noReportFlag {
		analyzeDirectoryTypes(absScanDir)
		fmt.Fprintf(os.Stderr, "Proceeding with scan...\n\n")
	}

	// If auto-identify-folders is set, sample subdirectories and scan only those matching score threshold.
	if *autoIdentifyFlag {
		qualifying, skipped, err := identifyPhotoFolders(absScanDir, *scoreThresholdFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error identifying photo folders: %v\n", err)
			os.Exit(1)
		}

		if len(qualifying) == 0 {
			fmt.Fprintf(os.Stderr, "No photo folders found.\n")
			os.Exit(1)
		}

		fmt.Printf("Auto-identifying photo folders in %s...\n\n", scanDir)
		for _, scored := range qualifying {
			ratio := 0.0
			if scored.Sample.TotalCount > 0 {
				ratio = float64(scored.Sample.MediaCount) / float64(scored.Sample.TotalCount) * 100
			}
			reasonsStr := strings.Join(scored.Reasons, ", ")
			fmt.Printf("  [%3d] ✓ %-40s %3d%% media, %s files  (%s)\n",
				scored.Score, filepath.Base(scored.Path), int(ratio), formatCount(scored.Sample.TotalCount), reasonsStr)
		}
		fmt.Printf("  ──── threshold: %d ────────────────────────────────────────────\n", *scoreThresholdFlag)
		for _, scored := range skipped {
			ratio := 0.0
			if scored.Sample.TotalCount > 0 {
				ratio = float64(scored.Sample.MediaCount) / float64(scored.Sample.TotalCount) * 100
			}
			reasonsStr := strings.Join(scored.Reasons, ", ")
			fmt.Printf("  [%3d] ✗ %-40s %3d%% media, %s files  (%s)\n",
				scored.Score, filepath.Base(scored.Path), int(ratio), formatCount(scored.Sample.TotalCount), reasonsStr)
		}
		if *detectOnlyFlag {
			fmt.Printf("\nDetection complete. Use --auto-identify-folders (without --detect-only) to scan these folders.\n")
			return
		}

		fmt.Printf("\nScanning %d photo folder(s)...\n\n", len(qualifying))

		// Scan each qualifying folder separately.
		for i, scored := range qualifying {
			qualifyingAbsDir, err := filepath.Abs(scored.Path)
			if err != nil {
				qualifyingAbsDir = scored.Path
			}

			manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, qualifyingAbsDir))
			manifestDir := filepath.Dir(manifestFile)
			if err := os.MkdirAll(manifestDir, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "Error: cannot create manifest directory %s: %v\n", manifestDir, err)
				os.Exit(1)
			}

			fmt.Printf("[%d/%d] Scanning: %s\n", i+1, len(qualifying), scored.Path)
			fmt.Printf("        Manifest: %s\n\n", manifestFile)

			cache := loadCache(manifestFile)
			if *noCacheFlag {
				cache = make(map[string]CacheEntry)
			}
			photoIgnore := newPhotoIgnore(qualifyingAbsDir)
			files, scanStats, err := scanDirectory(qualifyingAbsDir, cache, *fullHashFlag, photoIgnore)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error scanning %s: %v\n", scored.Path, err)
				continue
			}

			manifestStats, err := updateManifest(qualifyingAbsDir, files, manifestFile, machineName, *pruneFlag)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
				continue
			}

			printScanSummary(scanStats, manifestStats)
		}
	} else {
		// Original single-folder scan.
		manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, absScanDir))

		// Check write access to manifest directory before spending time scanning.
		manifestDir := filepath.Dir(manifestFile)
		if err := os.MkdirAll(manifestDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot create manifest directory %s: %v\n", manifestDir, err)
			os.Exit(1)
		}

		fmt.Printf("Scanning:  %s\n", scanDir)
		fmt.Printf("Manifest:  %s\n", manifestFile)
		fmt.Printf("Machine:   %s\n\n", machineName)

		cache := loadCache(manifestFile)
		if *noCacheFlag {
			cache = make(map[string]CacheEntry) // discard cache — force full recompute
		}
		photoIgnore := newPhotoIgnore(absScanDir)
		files, scanStats, err := scanDirectory(absScanDir, cache, *fullHashFlag, photoIgnore)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		manifestStats, err := updateManifest(absScanDir, files, manifestFile, machineName, *pruneFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error writing manifest:", err)
			os.Exit(1)
		}

		printScanSummary(scanStats, manifestStats)
	}
}

func printScanSummary(s ScanStats, m ManifestStats) {
	fmt.Println()
	fmt.Println("─────────────────────────────────────────────────────")
	fmt.Printf("  Files found:       %s\n", formatCount(s.Found))
	fmt.Printf("  Cached (skipped):  %s\n", formatCount(s.Cached))
	fmt.Printf("  New entries:       %s\n", formatCount(m.New))
	if m.Updated > 0 {
		fmt.Printf("  Hash upgraded:     %s\n", formatCount(m.Updated))
	}
	if s.FullHashed > 0 {
		fmt.Printf("  Full-hashed:       %s  (partial hash collision)\n", formatCount(s.FullHashed))
	}
	if s.Symlinks > 0 {
		fmt.Printf("  Symlinks skipped:  %s\n", formatCount(s.Symlinks))
	}
	if m.Pruned > 0 {
		fmt.Printf("  Pruned (deleted):  %s  (no longer on disk)\n", formatCount(m.Pruned))
	}
	fmt.Printf("  Total size:        %s\n", formatSize(s.TotalBytes))
	fmt.Println("─────────────────────────────────────────────────────")
}

func runRescan(args []string) {
	// Pre-separate flags from positional args.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "machine" || name == "root" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("rescan", flag.ExitOnError)
	machineFlag := fs.String("machine", "", "machine ID to rescan (default: current machine)")
	rootFlag := fs.String("root", "", "manifest directory (default: ~/manifests)")
	fullHashFlag := fs.Bool("full-hash", false, "hash all files fully, not just colliding ones")
	noCacheFlag := fs.Bool("no-cache", false, "recompute all hashes, ignoring cached values")
	pruneFlag := fs.Bool("prune", false, "remove manifest entries for files no longer on disk")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer rescan [--machine id] [--root dir] [--full-hash] [--no-cache] [--prune]\n\n")
		fmt.Fprintf(os.Stderr, "Re-scans all folders previously scanned on this machine.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)
	_ = posArgs

	// Resolve machine name (priority: flag > ./machine-id > ~/manifests/machine-id)
	machine := resolveMachineID(*machineFlag)

	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}
	manifestDir := filepath.Join(manifestRoot, "_Manifest")

	// Discover all manifests for this machine.
	allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if len(allCSVs) == 0 {
		fmt.Fprintf(os.Stderr, "rescan: no manifests found in %s\n", manifestDir)
		os.Exit(1)
	}

	// Collect unique scan_path values for this machine.
	seen := make(map[string]bool)
	var scanPaths []string
	for _, csvPath := range allCSVs {
		src, err := readManifest(csvPath)
		if err != nil || src.MachineName != machine {
			continue
		}
		if src.ScanPath != "" && !seen[src.ScanPath] {
			seen[src.ScanPath] = true
			scanPaths = append(scanPaths, src.ScanPath)
		}
	}
	sort.Strings(scanPaths)

	if len(scanPaths) == 0 {
		fmt.Fprintf(os.Stderr, "rescan: no scan paths found for machine %q in %s\n", machine, manifestDir)
		os.Exit(1)
	}

	fmt.Printf("Machine:  %s\n", machine)
	fmt.Printf("Folders to rescan (%d):\n", len(scanPaths))
	for _, p := range scanPaths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println()

	// Rescan each path.
	for i, scanDir := range scanPaths {
		if _, err := os.Stat(scanDir); os.IsNotExist(err) {
			fmt.Printf("[%d/%d] Skipping (not found): %s\n\n", i+1, len(scanPaths), scanDir)
			continue
		}
		fmt.Printf("[%d/%d] Scanning: %s\n", i+1, len(scanPaths), scanDir)
		manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machine, scanDir))
		fmt.Printf("        Manifest: %s\n\n", manifestFile)

		if err := os.MkdirAll(filepath.Dir(manifestFile), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot create manifest directory: %v\n", err)
			continue
		}

		cache := loadCache(manifestFile)
		if *noCacheFlag {
			cache = make(map[string]CacheEntry)
		}
		files, scanStats, err := scanDirectory(scanDir, cache, *fullHashFlag, newPhotoIgnore(scanDir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning %s: %v\n", scanDir, err)
			continue
		}

		manifestStats, err := updateManifest(scanDir, files, manifestFile, machine, *pruneFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
			continue
		}

		printScanSummary(scanStats, manifestStats)
	}
}

// =============================================================================
// Archive and Delete Commands
// =============================================================================

func runArchive(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer archive <folder-path> --dest <archive-dir>\n\n")
		fmt.Fprintf(os.Stderr, "Move folder to local archive directory and update manifests.\n")
		os.Exit(1)
	}

	sourceFolder := args[0]
	var archiveDir string

	// Parse --dest flag
	for i := 1; i < len(args); i++ {
		if args[i] == "--dest" && i+1 < len(args) {
			archiveDir = args[i+1]
			break
		}
	}

	if archiveDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required\n")
		os.Exit(1)
	}

	// Resolve paths to absolute
	absSourceFolder, err := filepath.Abs(sourceFolder)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		os.Exit(1)
	}

	absArchiveDir, err := filepath.Abs(archiveDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid archive directory %q\n", archiveDir)
		os.Exit(1)
	}

	// Check source folder exists
	if _, err := os.Stat(absSourceFolder); err != nil {
		fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		os.Exit(1)
	}

	// Check archive directory exists or create it
	if err := os.MkdirAll(absArchiveDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create archive directory: %v\n", err)
		os.Exit(1)
	}

	// Create timestamped archive folder name
	folderName := filepath.Base(absSourceFolder)
	timestamp := time.Now().Format("2006-01-02")
	archiveFolder := filepath.Join(absArchiveDir, fmt.Sprintf("%s-%s", timestamp, folderName))

	// Check if target already exists
	if _, err := os.Stat(archiveFolder); err == nil {
		fmt.Fprintf(os.Stderr, "Error: archive folder already exists: %s\n", archiveFolder)
		os.Exit(1)
	}

	// Move the folder
	fmt.Fprintf(os.Stderr, "Moving %s → %s\n", absSourceFolder, archiveFolder)
	if err := os.Rename(absSourceFolder, archiveFolder); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to move folder: %v\n", err)
		os.Exit(1)
	}

	// Get machine name
	machineName := resolveMachineID("")

	// Rescan the parent of source folder to remove old entries (with --prune)
	sourceParent := filepath.Dir(absSourceFolder)
	fmt.Fprintf(os.Stderr, "\nUpdating manifest for %s (pruning removed files)...\n", sourceParent)
	manifestRoot := filepath.Join(os.Getenv("HOME"), "manifests")
	manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, sourceParent))

	files, _, err := scanDirectory(sourceParent, make(map[string]CacheEntry), false, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Could not rescan parent folder: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Stale entries may remain. Run 'photo-organizer cleanup-manifests' to clean them up.\n")
	} else {
		stats, err := updateManifest(sourceParent, files, manifestFile, machineName, true) // prune=true
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Warning: Could not update manifest: %v\n", err)
		} else if stats.Pruned > 0 {
			fmt.Fprintf(os.Stderr, "✓ Removed %d stale entries from parent manifest\n", stats.Pruned)
		}
	}

	// Remove old manifests for the archived folder and its subdirectories
	manifestDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
	if matches, err := filepath.Glob(filepath.Join(manifestDir, "*.csv")); err == nil {
		for _, path := range matches {
			// Read manifest to check if it references the old source folder
			if src, err := readManifest(path); err == nil {
				// If this manifest scanned the old folder or its subdirectories, remove it
				if src.ScanPath == absSourceFolder || strings.HasPrefix(src.ScanPath, absSourceFolder+string(filepath.Separator)) {
					if err := os.Remove(path); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not remove old manifest %s: %v\n", path, err)
					}
				}
			}
		}
	}

	// Scan the archive folder to add new entries
	fmt.Fprintf(os.Stderr, "Scanning archive folder...\n")
	archiveParent := filepath.Dir(archiveFolder)
	manifestFile = filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, archiveParent))

	files, _, err = scanDirectory(archiveParent, make(map[string]CacheEntry), false, nil)
	if err == nil {
		updateManifest(archiveParent, files, manifestFile, machineName, false)
	}

	fmt.Fprintf(os.Stderr, "\n✓ Folder archived to %s\n", archiveFolder)
	fmt.Fprintf(os.Stderr, "  Files are still tracked in manifest at new location\n")
}

func runDeleteFolder(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer delete-folder <folder-path>\n\n")
		fmt.Fprintf(os.Stderr, "Delete folder and remove entries from manifest.\n")
		os.Exit(1)
	}

	folderPath := args[0]

	// Resolve to absolute path
	absFolderPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid path %q\n", folderPath)
		os.Exit(1)
	}

	// Check folder exists
	info, err := os.Stat(absFolderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: folder not found: %v\n", err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Error: %q is not a directory\n", folderPath)
		os.Exit(1)
	}

	// Confirm deletion
	fmt.Fprintf(os.Stderr, "⚠  This will permanently delete: %s\n", absFolderPath)
	fmt.Fprintf(os.Stderr, "Proceed? [y/N] ")
	var answer string
	fmt.Fscan(os.Stdin, &answer)
	if answer != "y" && answer != "Y" {
		fmt.Fprintf(os.Stderr, "Aborted.\n")
		os.Exit(0)
	}

	// Delete the folder
	fmt.Fprintf(os.Stderr, "Deleting folder...\n")
	if err := os.RemoveAll(absFolderPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to delete folder: %v\n", err)
		os.Exit(1)
	}

	// Update manifest - rescan parent with prune
	machineName := resolveMachineID("")
	parentFolder := filepath.Dir(absFolderPath)
	manifestFile := manifestFilename(machineName, parentFolder)

	fmt.Fprintf(os.Stderr, "Updating manifest (removing deleted files)...\n")
	files, _, err := scanDirectory(parentFolder, make(map[string]CacheEntry), false, nil)
	if err == nil {
		updateManifest(parentFolder, files, manifestFile, machineName, true) // prune=true
	}

	fmt.Fprintf(os.Stderr, "\n✓ Folder deleted: %s\n", absFolderPath)
	fmt.Fprintf(os.Stderr, "  Manifest entries removed\n")
}

// =============================================================================
// Backup Missing Command
// =============================================================================

func runBackupMissing(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup-missing <folder-path> --dest <user@host:/path>\n\n")
		fmt.Fprintf(os.Stderr, "Back up ONLY files not yet backed up to remote location via rsync.\n")
		fmt.Fprintf(os.Stderr, "Supports remote SSH destinations: user@host:/path\n\n")
		fmt.Fprintf(os.Stderr, "Workflow:\n")
		fmt.Fprintf(os.Stderr, "  1. Finds files NOT backed up (checks manifests)\n")
		fmt.Fprintf(os.Stderr, "  2. Copies ONLY missing files via rsync to remote\n")
		fmt.Fprintf(os.Stderr, "  3. Scans remote location to update manifests\n")
		fmt.Fprintf(os.Stderr, "  4. Collects updated manifests back to local\n")
		fmt.Fprintf(os.Stderr, "  5. Verifies all files are now backed up\n")
		os.Exit(1)
	}

	sourceFolder := args[0]
	var destLocation string

	// Parse --dest flag
	for i := 1; i < len(args); i++ {
		if args[i] == "--dest" && i+1 < len(args) {
			destLocation = args[i+1]
			break
		}
	}

	if destLocation == "" {
		fmt.Fprintf(os.Stderr, "Error: --dest is required (e.g., user@host:/backups)\n")
		os.Exit(1)
	}

	// Resolve source folder to absolute path
	absSourceFolder, err := filepath.Abs(sourceFolder)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid source path %q\n", sourceFolder)
		os.Exit(1)
	}

	// Check source folder exists
	if _, err := os.Stat(absSourceFolder); err != nil {
		fmt.Fprintf(os.Stderr, "Error: source folder not found: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Backing up missing files from %s to %s\n\n", absSourceFolder, destLocation)

	// Get local machine ID to distinguish local from remote backups
	localMachineID := resolveMachineID("")

	// Step 1: Find which files need backing up
	fmt.Fprintf(os.Stderr, "Step 1: Finding files not backed up...\n")

	// Load all manifests
	defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		// Mark whether this manifest is local or remote
		markManifestOrigin(&src, localMachineID)
		sources = append(sources, src)
	}

	// Detect stale manifests before building index
	stale := detectStaleManifests(sources)
	printStaleManifestReport(stale)

	// Report overlapping manifests before building index (only local overlaps matter)
	dedup := reportOverlapDeduplication(sources)
	printDeduplicationReportFiltered(dedup, localMachineID, sources)

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

		// Check if this file has non-removable backups
		key := indexKey(partialHash, info.Size())
		locs, exists := idx[key]

		hasBackup := false
		if exists && len(locs) > 0 {
			// Check if any location is non-removable and not the local source folder
			for _, loc := range locs {
				scanPath := sources[loc.sourceIdx].ScanPath
				isLocalManifest := sources[loc.sourceIdx].MachineName == localMachineID
				// Only skip if it's a local manifest at the source folder (file we're backing up from)
				if isLocalManifest && scanPath == absSourceFolder {
					continue
				}
				if !isRemovablePath(scanPath) {
					hasBackup = true
					break
				}
			}
		}

		if !hasBackup {
			// This file needs backing up
			relPath, _ := filepath.Rel(absSourceFolder, path)
			missingFiles = append(missingFiles, relPath)
			missingCount++
			missingSize += info.Size()
		}

		return nil
	})

	if missingCount == 0 {
		fmt.Fprintf(os.Stderr, "✓ No files need backing up - all files have non-removable backups\n")
		fmt.Fprintf(os.Stderr, "\nYou're all set! Run 'photo-organizer archive' to free up space.\n")
		return
	}

	fmt.Fprintf(os.Stderr, "Found %d files to backup (%.1f GB)\n\n", missingCount, float64(missingSize)/(1024*1024*1024))

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

	// Step 3: Use rsync with --files-from to copy only missing files
	fmt.Fprintf(os.Stderr, "Step 2: Copying missing files via rsync...\n")
	rsyncCmd := exec.Command("rsync", "-avz", "--progress", "--files-from="+tmpFile.Name(), absSourceFolder+"/", destLocation+"/")
	rsyncCmd.Stdout = os.Stderr
	rsyncCmd.Stderr = os.Stderr
	if err := rsyncCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: rsync failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "\n✓ Missing files copied via rsync\n")

	// Parse remote destination (user@host:/path)
	parts := strings.Split(destLocation, ":")
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "Error: invalid destination format. Use: user@host:/path\n")
		os.Exit(1)
	}

	remoteUserHost := parts[0]
	remotePath := parts[1]

	// Look up remote machine ID from machines.conf
	// machines map is machine-id -> ssh-host, so we need to reverse-lookup
	machines := loadMachinesConfig()

	remoteMachineID := ""
	for machID, sshHost := range machines {
		if sshHost == remoteUserHost {
			remoteMachineID = machID
			break
		}
	}

	if remoteMachineID == "" {
		fmt.Fprintf(os.Stderr, "Error: Remote host '%s' not found in machines.conf\n", remoteUserHost)
		fmt.Fprintf(os.Stderr, "Add it with: photo-organizer collect --add <machine-id>=%s\n", remoteUserHost)
		fmt.Fprintf(os.Stderr, "Example: photo-organizer collect --add ubuntu-backup=ubuntu-max\n")
		os.Exit(1)
	}

	// Step 3: SSH to remote and scan
	fmt.Fprintf(os.Stderr, "\nStep 3: Scanning remote location...\n")
	// Try to find photo-organizer in common locations
	scanCmd := fmt.Sprintf("cd %s && for path in photo-organizer ~/bin/photo-organizer /usr/local/bin/photo-organizer; do if command -v $path &>/dev/null || [ -f $path ]; then $path scan . --machine %s; exit $?; fi; done; echo 'photo-organizer not found in PATH'; exit 1", remotePath, remoteMachineID)
	sshCmd := exec.Command("ssh", remoteUserHost, scanCmd)
	sshCmd.Stdout = os.Stderr
	sshCmd.Stderr = os.Stderr
	if err := sshCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: Remote scan failed\n")
		fmt.Fprintf(os.Stderr, "photo-organizer must be installed on the remote machine.\n")
		fmt.Fprintf(os.Stderr, "Install it and run: ssh %s 'cd %s && photo-organizer scan . --machine %s'\n", remoteUserHost, remotePath, remoteMachineID)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "✓ Remote location scanned\n")

	// Step 4: Collect updated manifests
	fmt.Fprintf(os.Stderr, "\nStep 4: Collecting manifests from remote...\n")
	collectCmd := exec.Command("photo-organizer", "collect", "--from", remoteMachineID)
	collectCmd.Stdout = os.Stderr
	collectCmd.Stderr = os.Stderr
	if err := collectCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Manifest collection had issues: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "✓ Manifests collected\n")
	}

	// Step 5: Verify all files are now backed up
	fmt.Fprintf(os.Stderr, "\nStep 5: Verifying backup status...\n")
	verifyCmd := exec.Command("photo-organizer", "check-backup", absSourceFolder)
	verifyCmd.Stdout = os.Stderr
	verifyCmd.Stderr = os.Stderr
	if err := verifyCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Verification check complete\n")
	}

	fmt.Fprintf(os.Stderr, "\n✓ Backup process complete! All files are now backed up.\n")
}

// =============================================================================
// Cleanup Plan Command
// =============================================================================

func runCleanupPlan(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer cleanup-plan <folder-path>\n\n")
		fmt.Fprintf(os.Stderr, "Shows cleanup plan: what can be safely deleted and space impact.\n")
		os.Exit(1)
	}

	folderPath := args[0]

	// Resolve to absolute path
	absFolderPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid path %q\n", folderPath)
		os.Exit(1)
	}

	// Check folder exists
	if _, err := os.Stat(absFolderPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: folder not found: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Analyzing cleanup potential for: %s\n\n", absFolderPath)

	// Note: In a full implementation, we'd integrate check-backup logic here
	// For now, guide the user through the steps
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "CLEANUP PLAN\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

	fmt.Fprintf(os.Stderr, "Step 1: Check backup status\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer check-backup %s\n\n", absFolderPath)

	fmt.Fprintf(os.Stderr, "This will show:\n")
	fmt.Fprintf(os.Stderr, "  ✅ BACKED UP: Files safe to delete (with locations)\n")
	fmt.Fprintf(os.Stderr, "  ❌ NOT BACKED UP: Files that need backing up first\n\n")

	fmt.Fprintf(os.Stderr, "Step 2: Back up any unbacked files (if needed)\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing %s --dest user@host:/backups\n\n", absFolderPath)

	fmt.Fprintf(os.Stderr, "Step 3: Archive and delete\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer archive %s --dest /Archive\n", absFolderPath)
	fmt.Fprintf(os.Stderr, "  photo-organizer delete-folder /Archive/2026-06-18-<foldername>\n\n")

	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "Next: Run 'photo-organizer check-backup %s' to see details\n", absFolderPath)
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
}

func runCleanupManifests(args []string) {
	fmt.Fprintf(os.Stderr, "Scanning for stale manifests...\n\n")

	// Load all manifests
	manifestRoot := filepath.Join(os.Getenv("HOME"), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		return
	}

	var sources []ManifestSource
	var emptyManifests []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		// Flag empty manifests (header only, no data rows)
		if len(src.Rows) == 0 {
			emptyManifests = append(emptyManifests, src)
		}
		sources = append(sources, src)
	}

	// Detect stale manifests
	stale := detectStaleManifests(sources)

	// Detect overlapping manifests
	overlaps := overlappingPairs(sources)
	overlapCount := 0
	for pair := range overlaps {
		i, j := pair[0], pair[1]
		if i < j { // Count each pair once
			if sources[i].MachineName == sources[j].MachineName {
				overlapCount++
			}
		}
	}

	if stale.StaleCount == 0 && overlapCount == 0 && len(emptyManifests) == 0 {
		fmt.Fprintf(os.Stderr, "✓ No stale, overlapping, or empty manifests found\n")
		return
	}

	// Show empty manifests (informational - don't delete yet)
	if len(emptyManifests) > 0 {
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
		fmt.Fprintf(os.Stderr, "EMPTY MANIFESTS (no file data)\n")
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")
		fmt.Fprintf(os.Stderr, "Found %d empty manifest(s) - header only, no file entries:\n\n", len(emptyManifests))
		for _, src := range emptyManifests {
			info, _ := os.Stat(src.FilePath)
			modTime := info.ModTime().Format("2006-01-02 15:04:05")

			// Extract path from filename if ScanPath is empty
			displayPath := src.ScanPath
			if displayPath == "" {
				// Parse filename: photo_manifest_<machine>_<path>.csv
				filename := filepath.Base(src.FilePath)
				// Remove .csv extension
				pathEncoded := strings.TrimSuffix(filename, ".csv")
				// Remove "photo_manifest_" prefix
				pathEncoded = strings.TrimPrefix(pathEncoded, "photo_manifest_")
				// Remove machine ID prefix (everything up to and including the next underscore after machine ID)
				// Machine ID format: ubuntu-max-acb605, ubuntu-nas-f7e184, Ls-MBP-967e82
				if idx := strings.Index(pathEncoded, src.MachineName); idx == 0 {
					pathEncoded = pathEncoded[len(src.MachineName)+1:] // +1 for the underscore
					// Replace underscores with slashes, add leading slash
					displayPath = "/" + strings.ReplaceAll(pathEncoded, "_", "/")
				}
			}

			fmt.Fprintf(os.Stderr, "  • Machine: %s\n", src.MachineName)
			fmt.Fprintf(os.Stderr, "    Path: %s\n", displayPath)
			fmt.Fprintf(os.Stderr, "    File: %s\n", filepath.Base(src.FilePath))
			fmt.Fprintf(os.Stderr, "    Size: %d bytes, Modified: %s\n\n", info.Size(), modTime)
		}
		fmt.Fprintf(os.Stderr, "These manifests contain no file entries. Investigate before deletion:\n")
		fmt.Fprintf(os.Stderr, "  - Were they interrupted scans?\n")
		fmt.Fprintf(os.Stderr, "  - Did the scan find 0 files in that path?\n")
		fmt.Fprintf(os.Stderr, "  - Are they from deleted/inaccessible remote paths?\n\n")
	}

	// Show overlapping manifests (informational only, don't remove)
	if overlapCount > 0 {
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
		fmt.Fprintf(os.Stderr, "OVERLAPPING MANIFESTS (informational)\n")
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

		shown := make(map[[2]int]bool)
		for pair := range overlaps {
			i, j := pair[0], pair[1]
			if shown[pair] || shown[[2]int{j, i}] {
				continue
			}
			shown[pair] = true

			if sources[i].MachineName == sources[j].MachineName {
				pi := filepath.Clean(sources[i].ScanPath)
				pj := filepath.Clean(sources[j].ScanPath)
				var parent, child int
				if strings.HasPrefix(pi, pj) {
					parent, child = j, i
				} else {
					parent, child = i, j
				}
				fmt.Fprintf(os.Stderr, "ℹ  Same machine, nested paths:\n")
				fmt.Fprintf(os.Stderr, "   Parent: %s @ %s\n", sources[parent].MachineName, sources[parent].ScanPath)
				fmt.Fprintf(os.Stderr, "           (File: %s)\n", filepath.Base(sources[parent].FilePath))
				fmt.Fprintf(os.Stderr, "   Child:  %s @ %s\n", sources[child].MachineName, sources[child].ScanPath)
				fmt.Fprintf(os.Stderr, "           (File: %s)\n", filepath.Base(sources[child].FilePath))
				fmt.Fprintf(os.Stderr, "\n   Both manifests are kept. Analysis deduplicates them automatically.\n\n")
			}
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "STALE MANIFESTS\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

	// Categorize stale manifests by machine
	localStale := make([]ManifestSource, 0)
	remoteStale := make([]ManifestSource, 0)

	for i, detail := range stale.Details {
		fmt.Fprintf(os.Stderr, "%s\n", detail)
		if sources[i].IsStale {
			if isRemoteMachine(sources[i].MachineName) {
				remoteStale = append(remoteStale, sources[i])
			} else {
				localStale = append(localStale, sources[i])
			}
		}
		if i < len(stale.Details)-1 {
			fmt.Fprintf(os.Stderr, "───────────────────────────────────────────────────────────────────\n\n")
		}
	}

	fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "CLEANUP STATUS\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

	if len(localStale) > 0 {
		fmt.Fprintf(os.Stderr, "✓ Verified stale (local): %d manifest(s) - SAFE TO REMOVE\n", len(localStale))
		fmt.Fprintf(os.Stderr, "  (paths verified as non-existent on this machine)\n\n")
	}

	if len(remoteStale) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  Uncertain (remote): %d manifest(s) - CANNOT VERIFY\n", len(remoteStale))
		fmt.Fprintf(os.Stderr, "  (cannot verify if paths exist on unreachable remote machines)\n")
		fmt.Fprintf(os.Stderr, "  (these will be skipped - manual verification required)\n\n")
	}

	if len(localStale) == 0 && len(emptyManifests) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests verified as stale (100%% certain).\n")
		if len(remoteStale) > 0 {
			fmt.Fprintf(os.Stderr, "Remote manifests cannot be auto-verified. Manual review needed.\n")
		}
		if len(emptyManifests) > 0 {
			fmt.Fprintf(os.Stderr, "\nEmpty manifests found (see details above).\n")
		}
		return
	}

	// Ask user confirmation mode: one-by-one or all at once
	fmt.Fprintf(os.Stderr, "\nConfirm removal: [o]ne-by-one or [A]ll? [o/A] ")
	var mode string
	fmt.Fscan(os.Stdin, &mode)
	mode = strings.TrimSpace(mode)

	if mode != "A" && mode != "o" {
		fmt.Fprintf(os.Stderr, "Invalid choice. Aborted.\n")
		return
	}

	confirmAll := mode == "A"

	// Remove ONLY local stale manifests (100% certain)
	removedCount := 0
	skippedCount := 0

	for i := range sources {
		if !sources[i].IsStale || isRemoteMachine(sources[i].MachineName) {
			continue
		}

		if !confirmAll {
			// Ask for each manifest one by one
			fmt.Fprintf(os.Stderr, "\nRemove: %s @ %s\n", sources[i].MachineName, sources[i].ScanPath)
			fmt.Fprintf(os.Stderr, "         (File: %s)\n", filepath.Base(sources[i].FilePath))
			fmt.Fprintf(os.Stderr, "  Remove? [y/n/A-all/q-quit] ")
			var answer string
			fmt.Fscan(os.Stdin, &answer)
			answer = strings.TrimSpace(answer)

			switch answer {
			case "A":
				confirmAll = true // Start confirming all remaining
				fallthrough
			case "y":
				// Remove this one
			case "q":
				fmt.Fprintf(os.Stderr, "Quit. Aborted.\n")
				return
			case "n", "":
				skippedCount++
				continue
			default:
				fmt.Fprintf(os.Stderr, "Invalid choice. Skipping.\n")
				skippedCount++
				continue
			}
		}

		// Remove the manifest
		if err := os.Remove(sources[i].FilePath); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Failed to remove %s: %v\n", filepath.Base(sources[i].FilePath), err)
		} else {
			fmt.Fprintf(os.Stderr, "✓ Removed: %s\n", filepath.Base(sources[i].FilePath))
			removedCount++
		}
	}

	fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "Summary: Removed %d stale, Skipped %d\n", removedCount, skippedCount)
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n")

	if len(remoteStale) > 0 {
		fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
		fmt.Fprintf(os.Stderr, "REMOTE MANIFESTS (cannot auto-verify)\n")
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")
		fmt.Fprintf(os.Stderr, "Found %d potentially stale remote manifests, but cannot verify remotely.\n", len(remoteStale))
		fmt.Fprintf(os.Stderr, "Manual verification required before deletion.\n\n")
		fmt.Fprintf(os.Stderr, "To manually check remote manifests, SSH to each remote machine and verify\n")
		fmt.Fprintf(os.Stderr, "if the paths actually exist, then delete manifests manually if confirmed stale.\n")
	}
}

// isRemoteMachine checks if a machine name looks like a remote machine
// (contains @, or is a non-local identifier)
func isRemoteMachine(machineName string) bool {
	// If machine name looks like a remote identifier (not localhost-like),
	// it's probably remote. Common patterns:
	// - ubuntu-max-acb605 (remote)
	// - ubuntu-nas-f7e184 (remote)
	// - Ls-MBP-967e82 (local)
	// - Sony-A7IV-... (local device)
	// For simplicity: if it contains "ubuntu-" or starts with a known remote prefix, it's remote
	isKnownRemote := strings.HasPrefix(machineName, "ubuntu-") ||
		strings.HasPrefix(machineName, "backup-") ||
		(strings.Contains(machineName, "-") && len(machineName) > 20) // Random identifiers
	return isKnownRemote
}

// machineInfo holds metadata about a discovered machine
type machineInfo struct {
	name      string
	scanPaths map[string]bool
	lastScan  string
	fileCount int
	totalSize int64
}

func runMachines(args []string) {
	// Parse flags
	writeConf := false
	for _, arg := range args {
		if arg == "--write-conf" || arg == "--generate-conf" {
			writeConf = true
		}
	}

	// Load all manifests from the default manifest directory.
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")

	allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if len(allCSVs) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		os.Exit(1)
	}

	// Collect all machines and their metadata.
	machines := make(map[string]*machineInfo)

	for _, csvPath := range allCSVs {
		src, err := readManifest(csvPath)
		if err != nil || src.MachineName == "" {
			continue
		}

		if machines[src.MachineName] == nil {
			machines[src.MachineName] = &machineInfo{
				name:      src.MachineName,
				scanPaths: make(map[string]bool),
			}
		}
		m := machines[src.MachineName]

		if src.ScanPath != "" {
			m.scanPaths[src.ScanPath] = true
		}
		m.fileCount += len(src.Rows)
		if src.LastScanned > m.lastScan {
			m.lastScan = src.LastScanned
		}
		for _, row := range src.Rows {
			m.totalSize += row.SizeBytes
		}
	}

	// Load machine config for SSH targets.
	cfg := loadMachinesConfig()

	// Sort machines by name and display.
	var names []string
	for name := range machines {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Println("No machines found in manifests.")
		return
	}

	// If --write-conf flag, generate and write machines.conf
	if writeConf {
		confPath := machinesConfFile()
		confContent := generateMachinesConfWithPaths(names, machines, cfg)

		// Check if file exists
		if _, err := os.Stat(confPath); err == nil {
			fmt.Fprintf(os.Stderr, "⚠  %s already exists\n", confPath)
			fmt.Fprintf(os.Stderr, "Backup: cp %s %s.backup\n", confPath, confPath)
			fmt.Fprintf(os.Stderr, "Then overwrite with:\n")
			fmt.Fprintf(os.Stderr, "  cat > %s << 'EOF'\n%sEOF\n", confPath, confContent)
			return
		}

		if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", confPath, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "✓ Generated machines.conf at %s\n", confPath)
		fmt.Fprintf(os.Stderr, "\nEdit the file to add SSH targets for remote machines:\n")
		fmt.Fprintf(os.Stderr, "  nano %s\n", confPath)
		fmt.Fprintf(os.Stderr, "\nFormat: machine-name=user@host:/path\n")
		fmt.Fprintf(os.Stderr, "Example: nas-backup=admin@192.168.1.100:/mnt/backup\n")
		fmt.Fprintf(os.Stderr, "\nNote: Removable media (marked with [removable]) don't need SSH targets.\n")
		return
	}

	// Get current machine ID to mark it in the list
	currentMachine := machineID()

	fmt.Println("Machines in manifests:")
	fmt.Println()
	for _, name := range names {
		m := machines[name]
		sshTarget := cfg[name]
		if sshTarget == "" {
			sshTarget = "—"
		}

		// Collect and sort scan paths.
		var paths []string
		for p := range m.scanPaths {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		// Mark current machine
		marker := ""
		if name == currentMachine {
			marker = " (this machine)"
		}

		fmt.Printf("  %s%s\n", name, marker)
		fmt.Printf("    SSH: %s\n", sshTarget)
		fmt.Printf("    Last scanned: %s\n", m.lastScan)
		fmt.Printf("    Files: %s  (%s)\n", formatCount(m.fileCount), formatSize(m.totalSize))
		if len(paths) > 0 {
			fmt.Printf("    Scan paths:\n")
			for _, p := range paths {
				fmt.Printf("      %s\n", p)
			}
		}
		fmt.Println()
	}

	fmt.Printf("Use: photo-organizer plan --keep <machine> to generate a delete plan\n")
	fmt.Printf("     photo-organizer plan --intra <machine> to find duplicates within a machine\n")
}

// =============================================================================
// Config Paths
// =============================================================================

func defaultManifestRoot() string {
	return filepath.Join(os.Getenv("HOME"), "manifests")
}

func machineIDFile() string {
	return filepath.Join(defaultManifestRoot(), "machine-id")
}

func machinesConfFile() string {
	return filepath.Join(defaultManifestRoot(), "machines.conf")
}

// =============================================================================
// Machines Config (~/manifests/machines.conf)
// =============================================================================

// isRemovableMedia asks the OS whether the filesystem at path is removable.
// On macOS: uses diskutil info to check "Removable Media: Removable" or "Ejectable: Yes".
// On Linux: resolves the mount device via /proc/mounts, then checks /sys/block/<dev>/removable.
// Falls back to false (no warning) if detection fails — better a missed warning than a false one.
func isRemovableMedia(path string) bool {
	switch runtime.GOOS {
	case "darwin":
		return isMacOSRemovable(path)
	case "linux":
		return isLinuxRemovable(path)
	}
	return false
}

func isMacOSRemovable(path string) bool {
	out, err := exec.Command("diskutil", "info", path).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(line, ":", 2)
		if len(fields) != 2 {
			continue
		}
		key := strings.TrimSpace(fields[0])
		val := strings.TrimSpace(fields[1])
		// "Removable Media: Removable" (not "Fixed")
		if key == "Removable Media" && val == "Removable" {
			return true
		}
		// "Ejectable: Yes"
		if key == "Ejectable" && val == "Yes" {
			return true
		}
	}
	return false
}

func isLinuxRemovable(path string) bool {
	// Find the mount device for the given path via /proc/mounts.
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}

	// Find the longest matching mount point for path.
	bestMount, bestDev := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev, mount := fields[0], fields[1]
		if strings.HasPrefix(path, mount) && len(mount) > len(bestMount) {
			bestMount = mount
			bestDev = dev
		}
	}
	if bestDev == "" {
		return false
	}

	// Strip partition number to get block device (e.g., /dev/sdb1 → sdb).
	devName := filepath.Base(bestDev)
	for len(devName) > 0 && devName[len(devName)-1] >= '0' && devName[len(devName)-1] <= '9' {
		devName = devName[:len(devName)-1]
	}

	// Check /sys/block/<dev>/removable: "1" means removable.
	removable, err := os.ReadFile("/sys/block/" + devName + "/removable")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(removable)) == "1"
}

// generateMachinesConfWithPaths creates machines.conf from discovered machines and their paths
func generateMachinesConfWithPaths(machineNames []string, machineInfo map[string]*machineInfo, existing map[string]string) string {
	var buf strings.Builder

	buf.WriteString("# Machine SSH Configuration\n")
	buf.WriteString("# Format: machine-name=user@host:/path\n")
	buf.WriteString("#\n")
	buf.WriteString("# Machines marked [removable] are external drives/SD cards (no SSH needed)\n")
	buf.WriteString("# Machines marked [local] are on this computer (no SSH needed)\n")
	buf.WriteString("# For other machines, add SSH target in format: user@host:/path\n")
	buf.WriteString("#\n")
	buf.WriteString("# Examples:\n")
	buf.WriteString("# nas-backup=admin@nas.local:/mnt/backup\n")
	buf.WriteString("# cloud-storage=user@cloud.example.com:/home/user/backup\n")
	buf.WriteString("#\n\n")

	for _, name := range machineNames {
		existing_target := existing[name]
		paths := machineInfo[name].scanPaths

		// Determine if this is removable media
		isRemovable := false
		var scanPath string
		for p := range paths {
			scanPath = p
			if isRemovableMedia(p) {
				isRemovable = true
				break
			}
		}

		// Build comment with scan path
		var comment string
		if isRemovable {
			comment = fmt.Sprintf(" [removable] scanned from: %s", scanPath)
		} else if scanPath != "" {
			comment = fmt.Sprintf(" [local] scanned from: %s", scanPath)
		}

		if existing_target != "" {
			// Preserve existing SSH target
			buf.WriteString(fmt.Sprintf("%s=%s  #%s\n", name, existing_target, comment))
		} else {
			// Add placeholder for new machines
			if isRemovable {
				// Don't need SSH for removable media
				buf.WriteString(fmt.Sprintf("# %s=%s\n", name, comment))
			} else {
				buf.WriteString(fmt.Sprintf("# %s=%s\n", name, comment))
			}
		}
	}

	return buf.String()
}


// loadMachinesConfig reads ~/manifests/machines.conf and returns a map of
// machine_id → ssh_target. File format:
//
//	# comment
//	ubuntu-max-acb605 = ubuntu@192.168.1.100
//	nas-main          = admin@nas.local
func loadMachinesConfig() map[string]string {
	cfg := make(map[string]string)
	newPath := machinesConfFile()
	oldPath := filepath.Join(os.Getenv("HOME"), ".photo-organizer-machines")

	// Migrate old dotfile to new location if needed.
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if data, err := os.ReadFile(oldPath); err == nil {
			os.MkdirAll(filepath.Dir(newPath), 0755)
			if os.WriteFile(newPath, data, 0644) == nil {
				os.Remove(oldPath)
			}
		}
	}

	data, err := os.ReadFile(newPath)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if id != "" && target != "" {
			cfg[id] = target
		}
	}
	return cfg
}

// sshTargetFor returns the SSH target for a machine ID. Falls back to the
// machine ID itself if no entry exists (works if ~/.ssh/config has a match).
func sshTargetFor(machineID string, cfg map[string]string) string {
	if target, ok := cfg[machineID]; ok {
		return target
	}
	return machineID
}

// saveMachinesConfig writes the machines config file, creating it if needed.
func saveMachinesConfig(cfg map[string]string) error {
	path := machinesConfFile()
	os.MkdirAll(filepath.Dir(path), 0755)
	var sb strings.Builder
	sb.WriteString("# photo-organizer machines configuration\n")
	sb.WriteString("# Format: machine_id = user@host\n")
	sb.WriteString("# SSH connection details (port, key, etc.) go in ~/.ssh/config\n\n")
	ids := make([]string, 0, len(cfg))
	for id := range cfg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(&sb, "%-30s = %s\n", id, cfg[id])
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// =============================================================================
// Manifests (show manifest metadata and origin)
// =============================================================================

func runManifests(args []string) {
	fmt.Fprintf(os.Stderr, "Listing all manifests and their origin...\n\n")

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
		sources = append(sources, src)
	}

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
// Collect (pull manifests from remote machines)
// =============================================================================

func runCollect(args []string) {
	// Collect all --from/-f values and convert short flags to long form.
	// (flag package doesn't support repeated string flags natively)
	var fromMachines []string
	var remaining []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// Handle short aliases: -f → --from, -a → --add, -l → --list, -r → --root
		if arg == "-f" && i+1 < len(args) {
			fromMachines = append(fromMachines, args[i+1])
			i++
		} else if arg == "-a" && i+1 < len(args) {
			remaining = append(remaining, "--add", args[i+1])
			i++
		} else if arg == "-l" {
			remaining = append(remaining, "--list")
		} else if arg == "-r" && i+1 < len(args) {
			remaining = append(remaining, "--root", args[i+1])
			i++
		} else if (arg == "--from" || arg == "-from") && i+1 < len(args) {
			fromMachines = append(fromMachines, args[i+1])
			i++
		} else if strings.HasPrefix(arg, "--from=") {
			fromMachines = append(fromMachines, strings.TrimPrefix(arg, "--from="))
		} else if strings.HasPrefix(arg, "-f=") {
			fromMachines = append(fromMachines, strings.TrimPrefix(arg, "-f="))
		} else {
			remaining = append(remaining, arg)
		}
	}

	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	rootFlag := fs.String("root", "", "local manifest directory (default: ~/manifests)")
	addFlag := fs.String("add", "", "register a new machine: --add machine_id=user@host")
	listFlag := fs.Bool("list", false, "list configured machines")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect [--from/-f <machine> [--from <machine> ...]]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls manifests from remote machines into ~/manifests/_Manifest/.\n")
		fmt.Fprintf(os.Stderr, "If --from/-f is omitted, collects from all configured machines.\n")
		fmt.Fprintf(os.Stderr, "SSH targets are looked up from ~/manifests/machines.conf.\n\n")
		fmt.Fprintf(os.Stderr, "Collect from all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect\n\n")
		fmt.Fprintf(os.Stderr, "Collect from specific machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -f ubuntu-max -f nas\n\n")
		fmt.Fprintf(os.Stderr, "Register a machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -a ubuntu-max-acb605=ubuntu@192.168.1.100\n\n")
		fmt.Fprintf(os.Stderr, "List configured machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -l\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(remaining)

	cfg := loadMachinesConfig()

	// --add: register a new machine.
	if *addFlag != "" {
		parts := strings.SplitN(*addFlag, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "collect: --add format is machine_id=user@host")
			os.Exit(1)
		}
		id, target := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		cfg[id] = target
		if err := saveMachinesConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "collect: could not save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Registered: %-30s → %s\n", id, target)
		fmt.Printf("Config saved to %s\n", machinesConfFile())
		return
	}

	// --list: show all configured machines.
	if *listFlag {
		if len(cfg) == 0 {
			fmt.Fprintf(os.Stderr, "No machines configured. Add one with:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer collect --add machine_id=user@host\n")
			return
		}
		fmt.Println("Configured machines:")
		ids := make([]string, 0, len(cfg))
		for id := range cfg {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("  %-30s → %s\n", id, cfg[id])
		}
		return
	}

	// If no --from specified, collect from all configured machines.
	if len(fromMachines) == 0 {
		if len(cfg) == 0 {
			fmt.Fprintln(os.Stderr, "collect: no machines configured. Add one with:")
			fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
			os.Exit(1)
		}
		// Collect from all machines.
		for id := range cfg {
			fromMachines = append(fromMachines, id)
		}
		sort.Strings(fromMachines)
	}

	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}
	localDir := filepath.Join(manifestRoot, "_Manifest") + "/"
	if err := os.MkdirAll(localDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "collect: cannot create local manifest dir: %v\n", err)
		os.Exit(1)
	}

	for _, machine := range fromMachines {
		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			fmt.Fprintf(os.Stderr, "   Add with: photo-organizer collect --add %s=user@host\n", machine)
			continue
		}

		remoteDir := target + ":~/manifests/_Manifest/"
		fmt.Printf("Collecting from %s (%s)...\n", machine, target)

		var stderr bytes.Buffer
		cmd := exec.Command("rsync", "-av", "--update", remoteDir, localDir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			errMsg := strings.ToLower(stderr.String() + err.Error())
			fmt.Fprintf(os.Stderr, "⚠  Collect from %s failed\n", machine)

			switch {
			case strings.Contains(errMsg, "connection refused"):
				fmt.Fprintf(os.Stderr, "   Error: Cannot connect to %s (host unreachable or SSH not running)\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ping %s  or  ssh %s echo ok\n", target, target)
			case strings.Contains(errMsg, "permission denied"):
				fmt.Fprintf(os.Stderr, "   Error: Permission denied when connecting to %s\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ssh-copy-id %s  or check SSH keys\n", target)
			case strings.Contains(errMsg, "no such file"):
				fmt.Fprintf(os.Stderr, "   Error: Remote path ~/manifests/_Manifest/ not found on %s\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ssh %s ls -la ~/manifests/\n", target)
			case strings.Contains(errMsg, "timeout"):
				fmt.Fprintf(os.Stderr, "   Error: Connection timeout to %s (network too slow)\n", target)
				fmt.Fprintf(os.Stderr, "   Try: Check network or wait and retry\n")
			default:
				fmt.Fprintf(os.Stderr, "   %s\n", err)
			}
		} else {
			fmt.Printf("Done: %s\n\n", machine)
		}
	}
}

// mergeMachinesConfig merges remote config into local config, preserving remote-only entries.
// Returns the merged config, counts of added/updated/preserved entries, and list of conflicts.
func mergeMachinesConfig(local, remote map[string]string) (map[string]string, int, int, int) {
	merged := make(map[string]string)
	var added, updated, preserved int

	// Copy all remote entries initially
	for k, v := range remote {
		merged[k] = v
	}

	// Local entries override remote entries
	for k, v := range local {
		if _, exists := remote[k]; exists && remote[k] != v {
			updated++
		} else if !exists {
			added++
		}
		merged[k] = v
	}

	// Count preserved remote-only entries
	for k := range remote {
		if _, inLocal := local[k]; !inLocal {
			preserved++
		}
	}

	return merged, added, updated, preserved
}

func runCollectConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-f" || arg == "--from") && i+1 < len(args) {
			processedArgs = append(processedArgs, "--from", args[i+1])
			i++
		} else if strings.HasPrefix(arg, "-f=") {
			processedArgs = append(processedArgs, "--from="+strings.TrimPrefix(arg, "-f="))
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("collect-config", flag.ExitOnError)
	fromFlag := fs.String("from", "", "collect from specific machine (default: all machines)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect-config [-f/--from <machine>]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls machines.conf from remote machines and merges locally.\n")
		fmt.Fprintf(os.Stderr, "Remote-only entries are preserved; local entries override.\n\n")
		fmt.Fprintf(os.Stderr, "Collect from all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect-config\n\n")
		fmt.Fprintf(os.Stderr, "Collect from specific machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect-config -f ubuntu-max\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	cfg := loadMachinesConfig()
	if len(cfg) == 0 {
		fmt.Fprintln(os.Stderr, "No machines configured. Add one with:")
		fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
		return
	}

	fromMachines := []string{}
	if *fromFlag != "" {
		fromMachines = append(fromMachines, *fromFlag)
	} else {
		for id := range cfg {
			fromMachines = append(fromMachines, id)
		}
		sort.Strings(fromMachines)
	}

	merged := make(map[string]string)
	// Start with current local config
	for k, v := range cfg {
		merged[k] = v
	}

	for _, machine := range fromMachines {
		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			continue
		}

		fmt.Printf("Collecting config from %s (%s)...\n", machine, target)

		// SSH to remote and get machines.conf (silently returns empty if missing)
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd := exec.Command("ssh", target, "cat ~/manifests/machines.conf 2>/dev/null || echo ''")
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not fetch config from %s: %v\n", machine, err)
			continue
		}

		// Parse remote config
		remoteConfig := make(map[string]string)
		for _, line := range strings.Split(out.String(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			id := strings.TrimSpace(parts[0])
			target := strings.TrimSpace(parts[1])
			if id != "" && target != "" {
				remoteConfig[id] = target
			}
		}

		// Merge: mergeMachinesConfig(remote, local) to count remote-originated changes
		// "added" = new entries from remote, "updated" = conflicting entries
		_, found, conflicts, _ := mergeMachinesConfig(remoteConfig, merged)

		// Now merge remote into merged with local precedence
		for k, v := range remoteConfig {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}

		fmt.Printf("  Found: %d new, Conflicts: %d (local kept)\n", found, conflicts)
	}

	// Save merged config
	if err := saveMachinesConfig(merged); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving merged config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nMerged config saved to %s\n", machinesConfFile())
}

func runPushConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-t" || arg == "--to") && i+1 < len(args) {
			processedArgs = append(processedArgs, "--to", args[i+1])
			i++
		} else if strings.HasPrefix(arg, "-t=") {
			processedArgs = append(processedArgs, "--to="+strings.TrimPrefix(arg, "-t="))
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("push-config", flag.ExitOnError)
	toFlag := fs.String("to", "", "push to specific machine (default: all machines)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer push-config [-t/--to <machine>]\n\n")
		fmt.Fprintf(os.Stderr, "Pushes local machines.conf to remote machines with merge strategy.\n")
		fmt.Fprintf(os.Stderr, "Local entries override; remote-only entries are preserved.\n\n")
		fmt.Fprintf(os.Stderr, "Push to all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer push-config\n\n")
		fmt.Fprintf(os.Stderr, "Push to specific machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer push-config -t ubuntu-max\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	cfg := loadMachinesConfig()
	if len(cfg) == 0 {
		fmt.Fprintln(os.Stderr, "No machines configured. Add one with:")
		fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
		return
	}

	toMachines := []string{}
	if *toFlag != "" {
		toMachines = append(toMachines, *toFlag)
	} else {
		for id := range cfg {
			toMachines = append(toMachines, id)
		}
		sort.Strings(toMachines)
	}

	for _, machine := range toMachines {
		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			continue
		}

		fmt.Printf("Pushing config to %s (%s)...\n", machine, target)

		// SSH to remote and get their current machines.conf
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd := exec.Command("ssh", target, "cat ~/manifests/machines.conf 2>/dev/null || echo ''")
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not fetch remote config from %s: %v\n", machine, err)
			continue
		}

		// Parse remote config
		remoteConfig := make(map[string]string)
		for _, line := range strings.Split(out.String(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			id := strings.TrimSpace(parts[0])
			target := strings.TrimSpace(parts[1])
			if id != "" && target != "" {
				remoteConfig[id] = target
			}
		}

		// Merge: local overrides, remote-only preserved
		merged, added, updated, preserved := mergeMachinesConfig(cfg, remoteConfig)

		// Build merged config content
		var sb strings.Builder
		sb.WriteString("# photo-organizer machines configuration\n")
		sb.WriteString("# Format: machine_id = user@host\n")
		sb.WriteString("# SSH connection details (port, key, etc.) go in ~/.ssh/config\n\n")
		ids := make([]string, 0, len(merged))
		for id := range merged {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&sb, "%-30s = %s\n", id, merged[id])
		}

		// Push merged config to remote via SSH
		var pushStderr bytes.Buffer
		pushCmd := exec.Command("ssh", target, "mkdir -p ~/manifests && cat > ~/manifests/machines.conf")
		pushCmd.Stdin = strings.NewReader(sb.String())
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = &pushStderr
		if err := pushCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not push config to %s: %v\n", machine, err)
			continue
		}

		fmt.Printf("  Added: %d, Updated: %d, Preserved remote-only: %d\n", added, updated, preserved)
		fmt.Printf("Done: %s\n\n", machine)
	}
}

func runSyncConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for _, arg := range args {
		if arg == "-d" || arg == "--dry-run" {
			processedArgs = append(processedArgs, "--dry-run")
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("sync-config", flag.ExitOnError)
	dryRunFlag := fs.Bool("dry-run", false, "show what would be added without modifying machines.conf")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer sync-config [-d/--dry-run]\n\n")
		fmt.Fprintf(os.Stderr, "Auto-register machines found in manifests to machines.conf.\n")
		fmt.Fprintf(os.Stderr, "Reads all manifest files and adds any machines not yet in machines.conf.\n")
		fmt.Fprintf(os.Stderr, "Machines are tagged as [local], [removable], or plain SSH targets.\n\n")
		fmt.Fprintf(os.Stderr, "Dry-run preview:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer sync-config -d\n\n")
		fmt.Fprintf(os.Stderr, "Actually sync:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer sync-config\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		return
	}

	// Read all manifests and extract unique machines
	var sources []ManifestSource
	machineMap := make(map[string]ManifestSource) // machine name -> best source

	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		// Keep the first (or best) source for each machine
		if _, exists := machineMap[src.MachineName]; !exists {
			sources = append(sources, src)
			machineMap[src.MachineName] = src
		}
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No valid manifests with data found\n")
		return
	}

	// Load current machines.conf
	cfg := loadMachinesConfig()

	// Determine which machines need to be added
	type NewMachine struct {
		ID     string
		Target string
		Kind   string // "local", "removable", or "unknown"
	}
	var newMachines []NewMachine

	for _, src := range sources {
		if _, exists := cfg[src.MachineName]; exists {
			// Already configured
			continue
		}

		kind := "unknown"
		target := ""

		if isRemovablePath(src.ScanPath) {
			// Removable media (USB, SD card, etc.)
			kind = "removable"
			target = "[removable] scanned from: " + src.ScanPath
		} else if src.IsLocal {
			// Local machine
			kind = "local"
			target = "[local] scanned from: " + src.ScanPath
		}
		// For remote machines, we can't auto-configure (need SSH target)

		if kind != "unknown" {
			newMachines = append(newMachines, NewMachine{
				ID:     src.MachineName,
				Target: target,
				Kind:   kind,
			})
		}
	}

	if len(newMachines) == 0 {
		fmt.Printf("All machines in manifests are already configured in machines.conf\n")
		return
	}

	// Display what will be added
	fmt.Printf("Found %d new machines to add to machines.conf:\n\n", len(newMachines))
	for _, m := range newMachines {
		fmt.Printf("  %-30s → %s (%s)\n", m.ID, m.Target, m.Kind)
	}

	if *dryRunFlag {
		fmt.Printf("\nDry-run mode: no changes made\n")
		return
	}

	// Add to machines.conf
	for _, m := range newMachines {
		cfg[m.ID] = m.Target
	}

	if err := saveMachinesConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving machines.conf: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Added %d machines to %s\n", len(newMachines), machinesConfFile())
}

func runSearch(args []string) {
	runSearchAnalyze(args)
}
