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

// processFile opens a file once, reads the first 64KB, and derives both the
// partial MD5 hash and the capture date.
func processFile(path string) (hash string, captureDate time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", getDateFallback(path)
	}
	defer f.Close()

	buf := make([]byte, 65536)
	n, _ := f.Read(buf)
	buf = buf[:n]

	h := md5.New()
	h.Write(buf)
	hash = fmt.Sprintf("%x", h.Sum(nil))

	// Try EXIF from the first 64KB buffer.
	if photoExts[strings.ToLower(filepath.Ext(path))] {
		if x, err := exif.Decode(bytes.NewReader(buf)); err == nil {
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
			fmt.Fprintf(os.Stderr, "  skipping symlink: %s\n", path)
			stats.Symlinks++
			return nil
		}
		if info.IsDir() {
			if path != dir && (strings.HasPrefix(info.Name(), ".") || skipFolders[info.Name()]) {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(dir, path)
			fmt.Fprintf(os.Stderr, "\r  %-78s", "walking: "+rel)
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

	// Phase 3: always upgrade files whose partial hash collides with at least
	// one other file in this scan to a full-file hash. This eliminates false
	// positives from cameras whose videos share identical first-64KB headers.
	// --full-hash additionally upgrades all remaining partial-hash files.
	// Phase 3: compute full hash for files whose partial hash collides with
	// at least one other file, and for all files when --full-hash is set.
	byPartial := make(map[string][]int)
	for i, fi := range files {
		if fi.FullHash == "" { // skip files already full-hashed from cache
			byPartial[fi.PartialHash] = append(byPartial[fi.PartialHash], i)
		}
	}
	var upgradeIdx []int
	for _, indices := range byPartial {
		if len(indices) >= 2 || fullHash {
			upgradeIdx = append(upgradeIdx, indices...)
		}
	}
	if len(upgradeIdx) > 0 {
		stats.FullHashed = len(upgradeIdx)
		total := len(upgradeIdx)
		fmt.Fprintf(os.Stderr, "\r  %-78s\n", fmt.Sprintf("%s files need full hash — computing...", formatCount(total)))
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
					fmt.Fprintf(os.Stderr, "\r  %-78s",
						fmt.Sprintf("full hash: %s / %s", formatCount(int(n)), formatCount(total)))
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
// "Ls-MBP-a3f7c2". It is computed once and cached in ~/.photo-organizer-id
// so it never changes even if the hostname is later renamed.
func machineID() string {
	idFile := filepath.Join(os.Getenv("HOME"), ".photo-organizer-id")

	// Return cached ID if it exists.
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}

	// Build a new ID: short hostname + 6-char hardware UUID suffix.
	id := buildMachineID()

	// Persist it so it never changes.
	os.WriteFile(idFile, []byte(id+"\n"), 0644)
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
}

func updateManifest(scanDir string, files []FileInfo, manifestFile string, machineName string) (ManifestStats, error) {
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
		records, _ := r.ReadAll()
		f.Close()
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

	f, err := os.Create(manifestFile)
	if err != nil {
		return mstats, err
	}
	defer f.Close()

	w := csv.NewWriter(f)
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

	mstats.New = newCount
	mstats.Updated = updatedCount

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
		case "rescan":
			runRescan(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "  analyze                       Compare manifests, find cross-machine duplicates\n")
	fmt.Fprintf(os.Stderr, "  plan --keep <machine>         Generate safe-delete script for duplicates\n\n")
	fmt.Fprintf(os.Stderr, "scan flags:\n")
	fmt.Fprintf(os.Stderr, "  --root dir       write manifest to dir/_Manifest/ (default: ~/manifests)\n")
	fmt.Fprintf(os.Stderr, "  --machine name   machine label embedded in manifest (default: stable machine ID)\n")
	fmt.Fprintf(os.Stderr, "  --full-hash      hash all files fully, not just colliding ones (rarely needed)\n\n")
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
	fullHashFlag := fs.Bool("full-hash", false, "hash entire file instead of first 64KB (slower, more thorough)")
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
		manifestRoot = filepath.Join(os.Getenv("HOME"), "manifests")
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
	files, scanStats, err := scanDirectory(absScanDir, cache, *fullHashFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	manifestStats, err := updateManifest(absScanDir, files, manifestFile, machineName)
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
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer rescan [--machine id] [--root dir] [--full-hash]\n\n")
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
		manifestRoot = filepath.Join(os.Getenv("HOME"), "manifests")
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
		files, scanStats, err := scanDirectory(scanDir, cache, *fullHashFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning %s: %v\n", scanDir, err)
			continue
		}

		manifestStats, err := updateManifest(scanDir, files, manifestFile, machine)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
			continue
		}

		printScanSummary(scanStats, manifestStats)
	}
}
