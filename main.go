package main

import (
	"crypto/md5"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// Configuration
var (
	photoRoot   string
	incomingDir string
	originalsDir string
	manifestDir string
	manifestFile string
)

// Supported extensions
var (
	photoExts = map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
		".heic": true, ".dng": true, ".arw": true, ".cr2": true,
		".nef": true, ".raf": true,
	}
	videoExts = map[string]bool{
		".mp4": true, ".mov": true, ".avi": true, ".mkv": true,
	}
	audioExts = map[string]bool{
		".wav": true, ".mp3": true,
	}
	sidecarExts = map[string]bool{
		".lrf": true, ".xmp": true, ".json": true,
	}
	skipFolders = map[string]bool{
		".stfolder": true, ".fseventsd": true, ".Trashes": true,
		".Spotlight-V100": true, "PRIVATE": true, "AVF_INFO": true,
		"THMBNL": true,
	}
)

// Date extraction patterns
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

type FileInfo struct {
	SrcPath     string
	DestPath    string
	Size        int64
	ModTime     time.Time
	CaptureDate time.Time
	Hash        string
}

func isMediaFile(ext string) bool {
	ext = strings.ToLower(ext)
	return photoExts[ext] || videoExts[ext] || audioExts[ext] || sidecarExts[ext]
}

func isPhotoFile(ext string) bool {
	return photoExts[strings.ToLower(ext)]
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
		matches := p.regex.FindStringSubmatch(filename)
		if len(matches) >= 2 {
			t, err := time.Parse(p.layout, matches[1])
			if err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func getFileDate(path string) time.Time {
	ext := filepath.Ext(path)
	filename := filepath.Base(path)

	// Try EXIF for photos
	if isPhotoFile(ext) {
		if t, err := getExifDate(path); err == nil {
			return t
		}
	}

	// Try filename patterns
	if t, ok := getDateFromFilename(filename); ok {
		return t
	}

	// Fall back to modification time
	info, err := os.Stat(path)
	if err == nil {
		return info.ModTime()
	}

	return time.Now()
}

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

func findFilesToOrganize() ([]string, error) {
	var files []string

	err := filepath.Walk(incomingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Skip directories we don't want
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || skipFolders[name] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden files
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}

		// Check extension
		ext := filepath.Ext(path)
		if isMediaFile(ext) {
			files = append(files, path)
		}

		return nil
	})

	return files, err
}

func getDestination(srcPath string) string {
	fileDate := getFileDate(srcPath)
	year := fileDate.Format("2006")
	dateFolder := fileDate.Format("2006-01-02")
	filename := filepath.Base(srcPath)

	return filepath.Join(originalsDir, year, dateFolder, filename)
}

func organizeFiles(dryRun bool) ([]FileInfo, error) {
	files, err := findFilesToOrganize()
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		fmt.Println("No new files found in Incoming/")
		return nil, nil
	}

	fmt.Printf("Found %d files to organize\n\n", len(files))

	var organized []FileInfo
	skipped := 0

	for _, srcPath := range files {
		destPath := getDestination(srcPath)

		// Check for existing file
		if destInfo, err := os.Stat(destPath); err == nil {
			srcInfo, _ := os.Stat(srcPath)
			if srcInfo.Size() == destInfo.Size() {
				skipped++
				continue
			}
			// Different file, add suffix
			ext := filepath.Ext(destPath)
			base := strings.TrimSuffix(destPath, ext)
			counter := 1
			for {
				destPath = fmt.Sprintf("%s_%d%s", base, counter, ext)
				if _, err := os.Stat(destPath); os.IsNotExist(err) {
					break
				}
				counter++
			}
		}

		relSrc, _ := filepath.Rel(photoRoot, srcPath)
		relDest, _ := filepath.Rel(photoRoot, destPath)

		if dryRun {
			fmt.Printf("  %s\n", relSrc)
			fmt.Printf("    → %s\n", relDest)
		} else {
			// Create destination directory
			destDir := filepath.Dir(destPath)
			if err := os.MkdirAll(destDir, 0755); err != nil {
				fmt.Printf("Error creating directory %s: %v\n", destDir, err)
				continue
			}

			// Move file
			if err := os.Rename(srcPath, destPath); err != nil {
				// Try copy + delete if rename fails (cross-device)
				if err := copyFile(srcPath, destPath); err != nil {
					fmt.Printf("Error moving %s: %v\n", srcPath, err)
					continue
				}
				os.Remove(srcPath)
			}

			srcInfo, _ := os.Stat(destPath)
			organized = append(organized, FileInfo{
				SrcPath:     srcPath,
				DestPath:    destPath,
				Size:        srcInfo.Size(),
				ModTime:     srcInfo.ModTime(),
				CaptureDate: getFileDate(destPath),
				Hash:        getFileHash(destPath),
			})
		}
	}

	if dryRun {
		fmt.Printf("\n[DRY RUN] Would organize %d files\n", len(files)-skipped)
		if skipped > 0 {
			fmt.Printf("[DRY RUN] Would skip %d duplicates\n", skipped)
		}
	} else {
		fmt.Printf("\nOrganized %d files\n", len(organized))
		if skipped > 0 {
			fmt.Printf("Skipped %d duplicates\n", skipped)
		}
	}

	return organized, nil
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func updateManifest(organized []FileInfo) error {
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
	}

	// Read existing entries
	existing := make(map[string][]string)
	var headers []string

	if f, err := os.Open(manifestFile); err == nil {
		reader := csv.NewReader(f)
		records, _ := reader.ReadAll()
		f.Close()

		if len(records) > 0 {
			headers = records[0]
			for _, row := range records[1:] {
				if len(row) > 1 {
					existing[row[1]] = row // Key by relative_path
				}
			}
		}
	}

	if len(headers) == 0 {
		headers = []string{"filename", "relative_path", "source_folder", "file_size_bytes",
			"file_size_mb", "file_modified", "capture_date", "camera_make",
			"camera_model", "file_hash", "extension", "organized_date"}
	}

	// Add new entries
	newCount := 0
	for _, fi := range organized {
		relPath, _ := filepath.Rel(photoRoot, fi.DestPath)
		if _, exists := existing[relPath]; exists {
			continue
		}

		srcRel, _ := filepath.Rel(incomingDir, fi.SrcPath)
		sourceFolder := strings.Split(srcRel, string(os.PathSeparator))[0]

		row := []string{
			filepath.Base(fi.DestPath),
			relPath,
			sourceFolder,
			fmt.Sprintf("%d", fi.Size),
			fmt.Sprintf("%.2f", float64(fi.Size)/(1024*1024)),
			fi.ModTime.Format("2006-01-02 15:04:05"),
			fi.CaptureDate.Format("2006:01:02 15:04:05"),
			"", "", // camera make/model
			fi.Hash,
			strings.ToLower(filepath.Ext(fi.DestPath)),
			time.Now().Format("2006-01-02 15:04:05"),
		}
		existing[relPath] = row
		newCount++
	}

	// Write updated manifest
	f, err := os.Create(manifestFile)
	if err != nil {
		return err
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	writer.Write(headers)

	// Sort by relative path
	var paths []string
	for p := range existing {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		writer.Write(existing[p])
	}
	writer.Flush()

	if newCount > 0 {
		fmt.Printf("Added %d entries to manifest\n", newCount)
	}

	return nil
}

func cleanupEmptyFolders() {
	removed := 0

	filepath.Walk(incomingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == incomingDir {
			return nil
		}

		entries, _ := os.ReadDir(path)
		// Filter hidden files
		visible := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				visible++
			}
		}

		if visible == 0 {
			os.RemoveAll(path)
			removed++
		}
		return nil
	})

	if removed > 0 {
		fmt.Printf("Cleaned up %d empty folders\n", removed)
	}
}

func main() {
	// Flags
	execute := flag.Bool("execute", false, "Actually move files (default is dry-run)")
	executeShort := flag.Bool("x", false, "Actually move files (short for --execute)")
	updateManifestFlag := flag.Bool("update-manifest", false, "Update the manifest CSV after organizing")
	updateManifestShort := flag.Bool("m", false, "Update manifest (short for --update-manifest)")
	rootDir := flag.String("root", "", "Photo library root directory (default: current directory)")
	flag.Parse()

	// Combine short and long flags
	doExecute := *execute || *executeShort
	doUpdateManifest := *updateManifestFlag || *updateManifestShort
	dryRun := !doExecute

	// Set paths
	if *rootDir != "" {
		photoRoot = *rootDir
	} else {
		var err error
		photoRoot, err = os.Getwd()
		if err != nil {
			fmt.Println("Error getting current directory:", err)
			os.Exit(1)
		}
	}

	incomingDir = filepath.Join(photoRoot, "Incoming")
	originalsDir = filepath.Join(photoRoot, "Originals")
	manifestDir = filepath.Join(photoRoot, "_Manifest")
	manifestFile = filepath.Join(manifestDir, "photo_manifest.csv")

	// Check that Incoming exists
	if _, err := os.Stat(incomingDir); os.IsNotExist(err) {
		fmt.Printf("Error: Incoming directory not found at %s\n", incomingDir)
		os.Exit(1)
	}

	fmt.Println(strings.Repeat("=", 50))
	fmt.Println("Photo Organizer")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Incoming:  %s\n", incomingDir)
	fmt.Printf("Originals: %s\n", originalsDir)
	fmt.Println()

	if dryRun {
		fmt.Println("[DRY RUN MODE - use --execute or -x to actually move files]\n")
	}

	organized, err := organizeFiles(dryRun)
	if err != nil {
		fmt.Println("Error organizing files:", err)
		os.Exit(1)
	}

	if !dryRun {
		if len(organized) > 0 && doUpdateManifest {
			if err := updateManifest(organized); err != nil {
				fmt.Println("Error updating manifest:", err)
			}
		}
		cleanupEmptyFolders()
	}

	fmt.Println("\nDone!")
}
