// photo-organizer: scan a folder and generate a photo manifest CSV.
//
// Usage:
//
//	photo-organizer [directory]           # scan directory, write manifest inside it
//	photo-organizer                       # scan current directory
//	photo-organizer /photos --root /other # scan /photos, write manifest to /other/_Manifest/
package main

import (
	"crypto/md5"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

func getExifDate(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return time.Time{}, err
	}
	return x.DateTime()
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

func getFileDate(path string) time.Time {
	if photoExts[strings.ToLower(filepath.Ext(path))] {
		if t, err := getExifDate(path); err == nil {
			return t
		}
	}
	if t, ok := getDateFromFilename(filepath.Base(path)); ok {
		return t
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Now()
}

// =============================================================================
// Hashing
// =============================================================================

func getFileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := md5.New()
	buf := make([]byte, 65536)
	n, _ := f.Read(buf)
	h.Write(buf[:n])
	return fmt.Sprintf("%x", h.Sum(nil))
}

// =============================================================================
// Scanning
// =============================================================================

type FileInfo struct {
	Path        string
	Size        int64
	ModTime     time.Time
	CaptureDate time.Time
	Hash        string
}

func isMediaFile(ext string) bool {
	ext = strings.ToLower(ext)
	return photoExts[ext] || videoExts[ext] || audioExts[ext] || sidecarExts[ext]
}

func scanDirectory(dir string) ([]FileInfo, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, fmt.Errorf("directory not found: %s", dir)
	}

	var files []FileInfo
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") || skipFolders[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || !isMediaFile(filepath.Ext(path)) {
			return nil
		}
		files = append(files, FileInfo{
			Path:        path,
			Size:        info.Size(),
			ModTime:     info.ModTime(),
			CaptureDate: getFileDate(path),
			Hash:        getFileHash(path),
		})
		return nil
	})
	return files, err
}

// =============================================================================
// Manifest
// =============================================================================

func updateManifest(scanDir string, files []FileInfo, manifestFile string) error {
	manifestDir := filepath.Dir(manifestFile)
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
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
		"file_hash",
		"extension",
		"scan_date",
		"scan_path",
	}

	// Load existing entries keyed by relative path, padding rows to current width.
	existing := make(map[string][]string)
	if f, err := os.Open(manifestFile); err == nil {
		r := csv.NewReader(f)
		records, _ := r.ReadAll()
		f.Close()
		for _, row := range records[1:] { // skip header row
			if len(row) < 2 {
				continue
			}
			// Pad short rows (from older manifest versions) to current column count
			for len(row) < len(headers) {
				row = append(row, "")
			}
			existing[row[1]] = row
		}
	}

	newCount := 0
	for _, fi := range files {
		relPath, _ := filepath.Rel(scanDir, fi.Path)
		if _, exists := existing[relPath]; exists {
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
			fi.Hash,
			strings.ToLower(filepath.Ext(fi.Path)),
			time.Now().Format("2006-01-02 15:04:05"),
			scanDir,
		}
		newCount++
	}

	f, err := os.Create(manifestFile)
	if err != nil {
		return err
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

	fmt.Printf("Scanned %d files, added %d new entries to manifest\n", len(files), newCount)
	return nil
}

// =============================================================================
// Main
// =============================================================================

func main() {
	rootFlag := flag.String("root", "", "where to write the manifest (default: same as scan directory)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "photo-organizer — scan a folder and generate a photo manifest\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer [directory]            scan directory, manifest written inside it\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer                        scan current directory\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer /photos --root /other  scan /photos, manifest in /other/_Manifest/\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Determine directory to scan
	scanDir := flag.Arg(0)
	if scanDir == "" {
		var err error
		scanDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}

	// Determine where manifest is written
	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = scanDir
	}
	manifestFile := filepath.Join(manifestRoot, "_Manifest", "photo_manifest.csv")

	fmt.Printf("Scanning: %s\n", scanDir)
	fmt.Printf("Manifest: %s\n\n", manifestFile)

	files, err := scanDirectory(scanDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	if err := updateManifest(scanDir, files, manifestFile); err != nil {
		fmt.Fprintln(os.Stderr, "Error writing manifest:", err)
		os.Exit(1)
	}
}
