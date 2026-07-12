package main

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

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
	return strings.Contains(normPath, "/.stfolder/")
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
