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
	".hif":  true,
	".dng":  true,
	".arw":  true,
	".cr2":  true,
	".nef":  true,
	".raf":  true,
}

var videoExts = map[string]bool{
	".mp4": true,
	".mov": true,
	".avi": true,
	".mkv": true,
}

var audioExts = map[string]bool{
	".wav": true,
	".mp3": true,
}

var sidecarExts = map[string]bool{
	".lrf":  true,
	".xmp":  true,
	".json": true,
}

// System/camera directories to skip during scanning.
var skipFolders = map[string]bool{
	".stfolder":       true,
	".fseventsd":      true,
	".Trashes":        true,
	".Spotlight-V100": true,
	"PRIVATE":         true,
	"AVF_INFO":        true,
	"THMBNL":          true,
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

func isMediaFile(ext string) bool {
	ext = strings.ToLower(ext)
	return photoExts[ext] || videoExts[ext] || audioExts[ext] || sidecarExts[ext]
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
			if path != dir && (strings.HasPrefix(info.Name(), ".") || skipFolders[info.Name()]) {
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
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "analyze":
			runAnalyze(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "  collect --from <machine>      Pull manifests from remote machines via SSH\n")
	fmt.Fprintf(os.Stderr, "  machines                      List all machines in manifests with metadata\n")
	fmt.Fprintf(os.Stderr, "  analyze                       Compare manifests, find cross-machine duplicates\n")
	fmt.Fprintf(os.Stderr, "  risk-report                   Identify files at risk (only on one machine)\n")
	fmt.Fprintf(os.Stderr, "  plan --keep <machine>         Generate safe-delete script for duplicates\n")
	fmt.Fprintf(os.Stderr, "  migrate --from <machine> --dest <path>  Copy unique files preserving folder structure\n\n")
	fmt.Fprintf(os.Stderr, "machines config: ~/manifests/machines.conf  (machine_id = user@host)\n\n")
	fmt.Fprintf(os.Stderr, "scan flags:\n")
	fmt.Fprintf(os.Stderr, "  --root dir       write manifest to dir/_Manifest/ (default: ~/manifests)\n")
	fmt.Fprintf(os.Stderr, "  --machine name   machine label embedded in manifest (default: stable machine ID)\n")
	fmt.Fprintf(os.Stderr, "  --full-hash      hash all files fully, not just colliding ones (rarely needed)\n")
	fmt.Fprintf(os.Stderr, "  --no-cache       recompute all hashes, ignoring cached values\n")
	fmt.Fprintf(os.Stderr, "  --prune          remove manifest entries for files no longer on disk\n\n")
	fmt.Fprintf(os.Stderr, "plan flags:\n")
	fmt.Fprintf(os.Stderr, "  --keep <machine>     keep copies on this machine, move others to quarantine\n")
	fmt.Fprintf(os.Stderr, "  --intra <machine>    find duplicates within a single machine\n")
	fmt.Fprintf(os.Stderr, "  --keep-under <path>  with --intra: keep copies under this path\n")
	fmt.Fprintf(os.Stderr, "  --ssh <user@host>    verify backup files exist on remote machine\n")
	fmt.Fprintf(os.Stderr, "  --delete             generate rm commands instead of mv to quarantine\n")
	fmt.Fprintf(os.Stderr, "  --out <file>         write script to file instead of stdout\n\n")
	fmt.Fprintf(os.Stderr, "analyze flags:\n")
	fmt.Fprintf(os.Stderr, "  --csv prefix     also write CSV output files with this filename prefix\n")
	fmt.Fprintf(os.Stderr, "  --threshold n    folder coverage %% to flag as nearly-redundant (default: 0.9)\n\n")
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
				if name == "root" || name == "machine" { // value-taking flags only
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
	fullHashFlag := fs.Bool("full-hash", false, "hash all files fully, not just colliding ones (rarely needed)")
	noCacheFlag := fs.Bool("no-cache", false, "recompute all hashes, ignoring cached values (use after hash algorithm change)")
	pruneFlag := fs.Bool("prune", false, "remove manifest entries for files no longer on disk")
	fs.Usage = printUsage
	fs.Parse(flagArgs)

	// Resolve machine name
	machineName := *machineFlag
	if machineName == "" {
		machineName = machineID()
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

	machine := *machineFlag
	if machine == "" {
		machine = machineID()
	}

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

func runMachines(_ []string) {
	// Load all manifests from the default manifest directory.
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")

	allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if len(allCSVs) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		os.Exit(1)
	}

	// Collect all machines and their metadata.
	type machineInfo struct {
		name      string
		scanPaths map[string]bool
		lastScan  string
		fileCount int
		totalSize int64
	}
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

		fmt.Printf("  %s\n", name)
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
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect --from <machine> [--from <machine> ...]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls manifests from remote machines into ~/manifests/_Manifest/.\n")
		fmt.Fprintf(os.Stderr, "SSH targets are looked up from ~/manifests/machines.conf.\n\n")
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

	if len(fromMachines) == 0 {
		fmt.Fprintln(os.Stderr, "collect: --from <machine> is required")
		fs.Usage()
		os.Exit(1)
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
		remoteDir := target + ":~/manifests/_Manifest/"
		fmt.Printf("Collecting from %s (%s)...\n", machine, target)

		cmd := exec.Command("rsync", "-av", "--update", remoteDir, localDir)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "collect: rsync from %s failed: %v\n", target, err)
		} else {
			fmt.Printf("Done: %s\n\n", machine)
		}
	}
}
