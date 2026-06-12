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
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".heic": true,
	".heif": true,
	".hif":  true,
	".dng":  true,
	".arw":  true,
	".cr2":  true,
	".cr3":  true,
	".nef":  true,
	".raf":  true,
	".orf":  true,
	".rw2":  true,
	".tif":  true,
	".tiff": true,
	".webp": true,
	".raw":  true,
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

// FileTypeStat holds aggregated stats for one file extension.
type FileTypeStat struct {
	Ext       string
	Count     int
	TotalSize int64
	IsScanned bool
}

// analyzeDirectoryTypes walks directory and shows what file types exist and which we'll scan.
func analyzeDirectoryTypes(dir string) {
	stats := make(map[string]*FileTypeStat)

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		if ext == "" {
			ext = "(no extension)"
		}

		if _, exists := stats[ext]; !exists {
			stats[ext] = &FileTypeStat{Ext: ext, IsScanned: isMediaFile(ext)}
		}
		stats[ext].Count++
		stats[ext].TotalSize += info.Size()
		return nil
	})

	// Separate into scanned and ignored types
	var scanned, ignored []FileTypeStat
	var scannedBytes, ignoredBytes int64

	for _, stat := range stats {
		if stat.IsScanned {
			scanned = append(scanned, *stat)
			scannedBytes += stat.TotalSize
		} else {
			ignored = append(ignored, *stat)
			ignoredBytes += stat.TotalSize
		}
	}

	// Sort by size descending
	sort.Slice(scanned, func(i, j int) bool {
		return scanned[i].TotalSize > scanned[j].TotalSize
	})
	sort.Slice(ignored, func(i, j int) bool {
		return ignored[i].TotalSize > ignored[j].TotalSize
	})

	// Print report
	fmt.Printf("\n═════════════════════════════════════════════════════════\n")
	fmt.Printf("File Type Coverage Report: %s\n", dir)
	fmt.Printf("═════════════════════════════════════════════════════════\n\n")

	fmt.Printf("Files to be SCANNED:\n")
	fmt.Printf("  %-10s %10s %12s\n", "Type", "Count", "Size")
	fmt.Printf("  %-10s %10s %12s\n", "────", "─────", "────")
	for _, s := range scanned {
		fmt.Printf("  %-10s %10d %12s\n", s.Ext, s.Count, formatBytes(s.TotalSize))
	}
	fmt.Printf("  %-10s %10d %12s\n", "TOTAL", sumCount(scanned), formatBytes(scannedBytes))

	if len(ignored) > 0 {
		fmt.Printf("\nFiles to be IGNORED (not media):\n")
		fmt.Printf("  %-10s %10s %12s\n", "Type", "Count", "Size")
		fmt.Printf("  %-10s %10s %12s\n", "────", "─────", "────")
		for _, s := range ignored {
			fmt.Printf("  %-10s %10d %12s\n", s.Ext, s.Count, formatBytes(s.TotalSize))
		}
		fmt.Printf("  %-10s %10d %12s\n", "TOTAL", sumCount(ignored), formatBytes(ignoredBytes))

		fmt.Printf("\n⚠️  If any ignored types contain important media, add to:\n")
		fmt.Printf("   /Users/lei/workspace/agents/photo-organizer/main.go (photoExts, videoExts, etc.)\n")
	}

	fmt.Printf("\n✓ Ready to scan %d files (%s)\n\n", sumCount(scanned), formatBytes(scannedBytes))
}

func sumCount(stats []FileTypeStat) int {
	total := 0
	for _, s := range stats {
		total += s.Count
	}
	return total
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

func scanDirectory(dir string, cache map[string]CacheEntry, fullHash bool) ([]FileInfo, ScanStats, error) {
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
		if strings.HasPrefix(info.Name(), ".") || !isMediaFile(filepath.Ext(path)) {
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

	// Apply pruning before write (so we write the final state).
	if prune {
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
		// the volume is likely unmounted or empty — skip pruning and warn.
		if len(existing) > 0 && wouldPrune*100/len(existing) > 50 {
			fmt.Fprintf(os.Stderr, "⚠  Skipping prune: would remove %s of %s entries (>50%%).\n",
				formatCount(wouldPrune), formatCount(len(existing)))
			fmt.Fprintf(os.Stderr, "   Is the volume mounted? Re-run with --prune after verifying.\n")
		} else {
			for relPath := range existing {
				if !scanned[relPath] {
					delete(existing, relPath)
					mstats.Pruned++
				}
			}
		}
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
// =============================================================================

func main() {
	// Check for interrupted operations at startup
	DetectInterruptedOperations()

	if len(os.Args) >= 2 {
		switch os.Args[1] {
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
		case "rescan":
			runRescan(os.Args[2:])
			return
		case "machines":
			runMachines(os.Args[2:])
			return
		case "risk-report":
			runRiskReport(os.Args[2:])
			return
		case "analyze-backup-compliance":
			runAnalyzeBackupCompliance(os.Args[2:])
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
		}
		runScan(os.Args[1:])
		return
	}
	printUsage()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "photo-organizer — scan folders and analyze photo manifests across machines\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  scan [directory]              Scan a directory and write a manifest CSV\n")
	fmt.Fprintf(os.Stderr, "  rescan                        Re-scan all folders previously scanned on this machine\n")
	fmt.Fprintf(os.Stderr, "  collect [--from <machine>]    Pull manifests from remote machines via SSH (all if --from omitted)\n")
	fmt.Fprintf(os.Stderr, "  machines [--write-conf]       List all machines; --write-conf generates machines.conf from manifests\n")
	fmt.Fprintf(os.Stderr, "  analyze                       Compare manifests, find cross-machine duplicates\n")
	fmt.Fprintf(os.Stderr, "  backup-status --from <name>   Show copy count and coverage after migration\n")
	fmt.Fprintf(os.Stderr, "  risk-report                   Identify files at risk (only on one machine)\n")
	fmt.Fprintf(os.Stderr, "  analyze-backup-compliance     Analyze 3-2-1 backup rule compliance (space impact)\n")
	fmt.Fprintf(os.Stderr, "  search [manifests...]         Search files by name, path, hash, size, date, machine\n")
	fmt.Fprintf(os.Stderr, "  plan --keep <machine>         Generate safe-delete script for duplicates\n")
	fmt.Fprintf(os.Stderr, "  migrate --from <machine> --dest <path>  Copy unique files preserving folder structure\n\n")
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

	// If media-id is provided, use it as the machine name (for tracking removable media across machines)
	if *mediaIDFlag != "" {
		machineName = *mediaIDFlag
	}

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

	// Determine where manifest is written
	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}

	// Warn if scanning removable media without --media-id (before pre-flight)
	if *mediaIDFlag == "" && isRemovableMedia(absScanDir) {
		fmt.Fprintf(os.Stderr, "⚠  Removable media detected: %s\n", absScanDir)
		fmt.Fprintf(os.Stderr, "   Without --media-id, this scan is recorded under machine %q.\n", machineName)
		fmt.Fprintf(os.Stderr, "   If this card is scanned on another machine, it will appear as a\n")
		fmt.Fprintf(os.Stderr, "   separate machine and backup-status will show false copy counts.\n\n")
		fmt.Fprintf(os.Stderr, "   Recommended:\n")
		fmt.Fprintf(os.Stderr, "     photo-organizer scan %s --media-id \"<label-on-card>\"\n\n", absScanDir)
		fmt.Fprintf(os.Stderr, "Proceed without --media-id? [y/N] ")
		var answer string
		fmt.Fscan(os.Stdin, &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintln(os.Stderr, "Aborted. Re-run with --media-id.")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr)
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
			files, scanStats, err := scanDirectory(qualifyingAbsDir, cache, *fullHashFlag)
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
		files, scanStats, err := scanDirectory(absScanDir, cache, *fullHashFlag)
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
		files, scanStats, err := scanDirectory(scanDir, cache, *fullHashFlag)
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
// Collect (pull manifests from remote machines)
// =============================================================================

func runCollect(args []string) {
	// Collect all --from values manually (flag package doesn't support
	// repeated string flags natively).
	var fromMachines []string
	var remaining []string
	for i := 0; i < len(args); i++ {
		if (args[i] == "--from" || args[i] == "-from") && i+1 < len(args) {
			fromMachines = append(fromMachines, args[i+1])
			i++
		} else if strings.HasPrefix(args[i], "--from=") {
			fromMachines = append(fromMachines, strings.TrimPrefix(args[i], "--from="))
		} else {
			remaining = append(remaining, args[i])
		}
	}

	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	rootFlag := fs.String("root", "", "local manifest directory (default: ~/manifests)")
	addFlag := fs.String("add", "", "register a new machine: --add machine_id=user@host")
	listFlag := fs.Bool("list", false, "list configured machines")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect [--from <machine> [--from <machine> ...]]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls manifests from remote machines into ~/manifests/_Manifest/.\n")
		fmt.Fprintf(os.Stderr, "If --from is omitted, collects from all configured machines.\n")
		fmt.Fprintf(os.Stderr, "SSH targets are looked up from ~/manifests/machines.conf.\n\n")
		fmt.Fprintf(os.Stderr, "Collect from all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect\n\n")
		fmt.Fprintf(os.Stderr, "Collect from specific machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect --from ubuntu-max --from nas\n\n")
		fmt.Fprintf(os.Stderr, "Register a machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect --add ubuntu-max-acb605=ubuntu@192.168.1.100\n\n")
		fmt.Fprintf(os.Stderr, "List configured machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect --list\n\n")
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

func runSearch(args []string) {
	runSearchAnalyze(args)
}
