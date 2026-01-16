// Photo Organizer - A tool to organize photos by capture date
//
// This tool scans an Incoming directory for photos and videos, extracts their
// capture dates from EXIF metadata or filenames, and organizes them into a
// structured directory hierarchy (Originals/YYYY/YYYY-MM-DD/).
//
// Features:
//   - EXIF date extraction from photos
//   - Filename pattern recognition (DJI, Sony, etc.)
//   - Duplicate detection via file size comparison
//   - Manifest CSV tracking for all organized files
//   - Cross-device file moving support
//   - Empty folder cleanup
//
// Usage:
//
//	photo-organizer              # Preview changes (dry-run, default)
//	photo-organizer -x           # Execute file moves
//	photo-organizer -x -m        # Execute and update manifest
//	photo-organizer --root /path # Use custom root directory
//
// Expected directory structure:
//
//	Photos/
//	├── Incoming/      <- Drop new photos here
//	├── Originals/     <- Organized photos (YYYY/YYYY-MM-DD/)
//	├── Exports/       <- Curated/edited photos
//	├── _Manifest/     <- Tracking CSV
//	└── photo-organizer
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

// =============================================================================
// Configuration
// =============================================================================

// Global path variables, set at runtime based on --root flag or current directory.
var (
	photoRoot    string // Root directory of the photo library
	incomingDir  string // Directory for new/unorganized photos
	originalsDir string // Directory for organized original photos
	manifestDir  string // Directory for manifest CSV
	manifestFile string // Path to the manifest CSV file
)

// =============================================================================
// Supported File Types
// =============================================================================

// photoExts contains supported photo file extensions.
// These files will have EXIF data extracted for date detection.
var photoExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".heic": true,
	".hif":  true, // Apple HEIF (alternate extension)
	".dng":  true, // Adobe Digital Negative
	".arw":  true, // Sony RAW
	".cr2":  true, // Canon RAW
	".nef":  true, // Nikon RAW
	".raf":  true, // Fujifilm RAW
}

// videoExts contains supported video file extensions.
var videoExts = map[string]bool{
	".mp4": true,
	".mov": true,
	".avi": true,
	".mkv": true,
}

// audioExts contains supported audio file extensions.
// Primarily for DJI drone audio files that accompany videos.
var audioExts = map[string]bool{
	".wav": true,
	".mp3": true,
}

// sidecarExts contains sidecar/metadata file extensions.
// These files typically accompany photos with additional metadata.
var sidecarExts = map[string]bool{
	".lrf":  true, // Low Resolution File (DJI)
	".xmp":  true, // Adobe XMP sidecar
	".json": true, // JSON metadata
}

// skipFolders contains directory names to skip during scanning.
// These are typically system folders or camera-specific directories
// that don't contain user photos.
var skipFolders = map[string]bool{
	".stfolder":      true, // Syncthing
	".fseventsd":     true, // macOS filesystem events
	".Trashes":       true, // macOS trash
	".Spotlight-V100": true, // macOS Spotlight index
	"PRIVATE":        true, // Camera system folder
	"AVF_INFO":       true, // Sony AVCHD info
	"THMBNL":         true, // Sony thumbnails
}

// =============================================================================
// Date Extraction Patterns
// =============================================================================

// datePatterns contains regex patterns for extracting dates from filenames.
// Patterns are tried in order; first match wins.
// The layout string uses Go's reference time: Mon Jan 2 15:04:05 MST 2006
var datePatterns = []struct {
	regex  *regexp.Regexp
	layout string
	desc   string // Description for documentation
}{
	// DJI drone: DJI_20250619224111_0001_D.MP4
	{regexp.MustCompile(`DJI_(\d{8})`), "20060102", "DJI drone files"},

	// Sony video: 20250616_C0416.MP4
	{regexp.MustCompile(`^(\d{8})_C\d+`), "20060102", "Sony video clips"},

	// Generic timestamp: IMG_20250619_123456.jpg
	{regexp.MustCompile(`(\d{8})_\d{6}`), "20060102", "Generic timestamp format"},

	// ISO date: 2025-06-19_photo.jpg
	{regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`), "2006-01-02", "ISO date format"},

	// Compact date: 20250619_photo.jpg (last resort, less specific)
	{regexp.MustCompile(`(\d{8})`), "20060102", "Compact date format"},
}

// =============================================================================
// Data Types
// =============================================================================

// FileInfo holds metadata about an organized file.
// Used for manifest tracking and reporting.
type FileInfo struct {
	SrcPath     string    // Original path in Incoming/
	DestPath    string    // New path in Originals/
	Size        int64     // File size in bytes
	ModTime     time.Time // File modification time
	CaptureDate time.Time // Extracted capture date
	Hash        string    // MD5 hash of first 64KB (for duplicate detection)
}

// =============================================================================
// File Type Detection
// =============================================================================

// isMediaFile returns true if the file extension indicates a supported media file.
// Checks against all supported types: photos, videos, audio, and sidecars.
func isMediaFile(ext string) bool {
	ext = strings.ToLower(ext)
	return photoExts[ext] || videoExts[ext] || audioExts[ext] || sidecarExts[ext]
}

// isPhotoFile returns true if the file extension indicates a photo file.
// Photo files are candidates for EXIF date extraction.
func isPhotoFile(ext string) bool {
	return photoExts[strings.ToLower(ext)]
}

// =============================================================================
// Date Extraction
// =============================================================================

// getExifDate extracts the capture date from a photo's EXIF metadata.
// Returns the DateTimeOriginal field if available.
// Returns an error if the file cannot be read or has no EXIF data.
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

// getDateFromFilename attempts to extract a date from the filename.
// Tries each pattern in datePatterns in order.
// Returns the parsed date and true if successful, or zero time and false if no match.
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

// getFileDate determines the best available date for a file.
// Priority:
//  1. EXIF DateTimeOriginal (for photos)
//  2. Date parsed from filename
//  3. File modification time
//  4. Current time (fallback)
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

// =============================================================================
// File Hashing
// =============================================================================

// getFileHash computes an MD5 hash of the first 64KB of a file.
// This provides fast duplicate detection without reading entire files.
// Returns an empty string if the file cannot be read.
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
// File Discovery
// =============================================================================

// findFilesToOrganize walks the Incoming directory and returns paths to all
// media files that should be organized.
// Skips hidden files/folders and system directories defined in skipFolders.
func findFilesToOrganize() ([]string, error) {
	var files []string

	err := filepath.Walk(incomingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}

		// Skip directories we don't want to process
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

		// Include only media files
		ext := filepath.Ext(path)
		if isMediaFile(ext) {
			files = append(files, path)
		}

		return nil
	})

	return files, err
}

// =============================================================================
// Path Generation
// =============================================================================

// getDestination calculates the destination path for a source file.
// Organizes into: Originals/YYYY/YYYY-MM-DD/filename
func getDestination(srcPath string) string {
	fileDate := getFileDate(srcPath)
	year := fileDate.Format("2006")
	dateFolder := fileDate.Format("2006-01-02")
	filename := filepath.Base(srcPath)

	return filepath.Join(originalsDir, year, dateFolder, filename)
}

// =============================================================================
// Core Organization Logic
// =============================================================================

// organizeFiles processes all files in Incoming and moves them to Originals.
// If dryRun is true, only prints what would happen without moving files.
// Returns a slice of FileInfo for successfully organized files.
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

		// Check for existing file at destination
		if destInfo, err := os.Stat(destPath); err == nil {
			srcInfo, _ := os.Stat(srcPath)
			// Skip if same size (likely duplicate)
			if srcInfo.Size() == destInfo.Size() {
				skipped++
				continue
			}
			// Different file with same name - add numeric suffix
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

		// Display relative paths for cleaner output
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

			// Move file (try rename first, fall back to copy+delete for cross-device)
			if err := os.Rename(srcPath, destPath); err != nil {
				if err := copyFile(srcPath, destPath); err != nil {
					fmt.Printf("Error moving %s: %v\n", srcPath, err)
					continue
				}
				os.Remove(srcPath)
			}

			// Record organized file info
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

	// Print summary
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

// =============================================================================
// File Operations
// =============================================================================

// copyFile copies a file from src to dst.
// Used as fallback when os.Rename fails (cross-device moves).
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

// =============================================================================
// Manifest Management
// =============================================================================

// updateManifest adds newly organized files to the manifest CSV.
// Creates the manifest file if it doesn't exist.
// Preserves existing entries and appends new ones.
func updateManifest(organized []FileInfo) error {
	// Ensure manifest directory exists
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return err
	}

	// Read existing entries from manifest
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

	// Define headers if manifest is new
	if len(headers) == 0 {
		headers = []string{
			"filename",        // Base filename
			"relative_path",   // Path relative to photo root
			"source_folder",   // Original folder in Incoming/
			"file_size_bytes", // Size in bytes
			"file_size_mb",    // Size in megabytes
			"file_modified",   // File modification timestamp
			"capture_date",    // EXIF/parsed capture date
			"camera_make",     // Camera manufacturer (if available)
			"camera_model",    // Camera model (if available)
			"file_hash",       // MD5 hash of first 64KB
			"extension",       // File extension
			"organized_date",  // When file was organized
		}
	}

	// Add new entries
	newCount := 0
	for _, fi := range organized {
		relPath, _ := filepath.Rel(photoRoot, fi.DestPath)
		if _, exists := existing[relPath]; exists {
			continue // Skip if already in manifest
		}

		// Determine source folder
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
			"", "", // camera make/model (not extracted in Go version)
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

	// Sort entries by relative path for consistent output
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

// =============================================================================
// Cleanup
// =============================================================================

// cleanupEmptyFolders removes empty directories from Incoming.
// Only removes directories that contain no visible (non-hidden) files.
func cleanupEmptyFolders() {
	removed := 0

	filepath.Walk(incomingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == incomingDir {
			return nil
		}

		entries, _ := os.ReadDir(path)

		// Count visible (non-hidden) files
		visible := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				visible++
			}
		}

		// Remove if empty
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

// =============================================================================
// Skill Installation
// =============================================================================

// skillContent contains the Claude Code skill definition.
const skillContent = `---
skill: organize-photos
description: Organize photos by capture date using the photo-organizer tool
---

# Photo Organization Skill

This skill helps you organize photos using the photo-organizer tool in this repository. The tool scans the Incoming directory, extracts dates from EXIF metadata or filenames, and organizes photos into a structured date-based hierarchy.

## How to Use This Skill

When the user invokes this skill (e.g., ` + "`/organize-photos`" + `), help them organize their photos by:

1. **Understanding their intent**: Ask what they want to do:
   - Preview what will be organized (dry-run)
   - Actually organize photos
   - Update the manifest after organizing
   - Check the status of their photo library

2. **Running the appropriate command**:
   - Preview: ` + "`./photo-organizer`" + `
   - Execute: ` + "`./photo-organizer -x`" + `
   - Execute + manifest: ` + "`./photo-organizer -x -m`" + `
   - Custom root: ` + "`./photo-organizer --root /path/to/photos -x`" + `

3. **Explain the output**: Help them understand what happened, including:
   - How many files were found
   - How many duplicates were skipped
   - Where files were organized to
   - Any errors or issues

## Directory Structure

The tool expects this structure:
` + "```" + `
Photos/
├── Incoming/          ← New photos go here
├── Originals/         ← Organized photos (YYYY/YYYY-MM-DD/)
├── Exports/           ← Curated/edited photos
├── _Manifest/         ← Tracking CSV
└── photo-organizer    ← The binary
` + "```" + `

## Supported Formats

- **Photos**: JPG, JPEG, PNG, GIF, HEIC, DNG, ARW, CR2, NEF, RAF
- **Videos**: MP4, MOV, AVI, MKV
- **Audio**: WAV, MP3 (DJI audio files)
- **Sidecars**: LRF, XMP, JSON

## Date Detection

The tool tries multiple methods to determine capture dates:
1. EXIF DateTimeOriginal (for photos)
2. Filename patterns (DJI, Sony, generic timestamps)
3. File modification time
4. Current time (fallback)

## Common Workflows

### Quick Check
Ask: "What would you like to do?"
- Preview mode (default, safe)
- Execute mode (actually move files)
- Execute with manifest update

### Preview Mode (Safe)
` + "```bash" + `
cd ~/Photos
./photo-organizer
` + "```" + `
Shows what would happen without moving any files.

### Organize Photos
` + "```bash" + `
cd ~/Photos
./photo-organizer -x
` + "```" + `
Actually moves files from Incoming/ to Originals/YYYY/YYYY-MM-DD/

### Organize + Update Manifest
` + "```bash" + `
cd ~/Photos
./photo-organizer -x -m
` + "```" + `
Moves files AND updates the tracking CSV.

### Custom Location
` + "```bash" + `
./photo-organizer --root /path/to/photos -x -m
` + "```" + `
Use a different root directory.

## Tips for Users

- **Always preview first**: Run without ` + "`-x`" + ` to see what will happen
- **Duplicates are safe**: Files with the same size are automatically skipped
- **Name conflicts**: Files with same name but different size get a numeric suffix
- **Empty folders**: Automatically cleaned up after organizing
- **Build first**: If the binary doesn't exist, run ` + "`./build.sh`" + ` to compile it

## Error Handling

If the tool fails:
- Check that ` + "`Incoming/`" + ` directory exists
- Verify the binary is executable: ` + "`chmod +x photo-organizer`" + `
- If binary is missing, build it: ` + "`./build.sh`" + `
- For permission errors, check file system permissions

## Proactive Assistance

When this skill is invoked:
1. First check if the binary exists, if not suggest building it
2. Check if Incoming directory exists and has files
3. Offer to run in preview mode first (safest option)
4. After organizing, offer to check the results or update manifest
5. Suggest cleanup actions if needed
`

// installSkill creates the .claude/skills directory and installs the skill file.
// Returns nil on success, error on failure.
func installSkill(targetDir string) error {
	skillDir := filepath.Join(targetDir, ".claude", "skills", "photo-organizer")
	skillFile := filepath.Join(skillDir, "SKILL.md")

	// Check if skill already exists
	if _, err := os.Stat(skillFile); err == nil {
		return fmt.Errorf("skill already exists at %s", skillFile)
	}

	// Create .claude/skills/photo-organizer directory
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", skillDir, err)
	}

	// Write skill file
	if err := os.WriteFile(skillFile, []byte(skillContent), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %v", err)
	}

	fmt.Printf("✓ Installed Claude Code skill to %s\n", skillFile)
	fmt.Println("\nYou can now use the skill in Claude Code by running:")
	fmt.Println("  /organize-photos")
	return nil
}

// =============================================================================
// Photo Library Initialization
// =============================================================================

// initPhotoLibrary creates the expected directory structure for a photo library.
// Returns nil on success, error on failure.
func initPhotoLibrary(targetDir string) error {
	// Define required directories
	dirs := []struct {
		path string
		desc string
	}{
		{filepath.Join(targetDir, "Incoming"), "Drop new photos here"},
		{filepath.Join(targetDir, "Originals"), "Organized photos (YYYY/YYYY-MM-DD/)"},
		{filepath.Join(targetDir, "Exports"), "Curated/edited photos"},
		{filepath.Join(targetDir, "_Manifest"), "Tracking CSV"},
	}

	fmt.Printf("Initializing photo library at: %s\n\n", targetDir)

	created := 0
	skipped := 0

	for _, dir := range dirs {
		// Check if directory already exists
		if _, err := os.Stat(dir.path); err == nil {
			fmt.Printf("⊘ %s (already exists)\n", filepath.Base(dir.path))
			skipped++
			continue
		}

		// Create directory
		if err := os.MkdirAll(dir.path, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %v", dir.path, err)
		}

		fmt.Printf("✓ %s/ - %s\n", filepath.Base(dir.path), dir.desc)
		created++
	}

	fmt.Printf("\n")
	if created > 0 {
		fmt.Printf("Created %d directories\n", created)
	}
	if skipped > 0 {
		fmt.Printf("Skipped %d existing directories\n", skipped)
	}

	fmt.Println("\nPhoto library is ready!")
	fmt.Println("Next steps:")
	fmt.Println("  1. Copy photos to Incoming/")
	fmt.Println("  2. Run: photo-organizer (preview)")
	fmt.Println("  3. Run: photo-organizer -x (organize)")

	return nil
}

// =============================================================================
// Main Entry Point
// =============================================================================

func main() {
	// Define command-line flags
	execute := flag.Bool("execute", false, "Actually move files (default is dry-run)")
	executeShort := flag.Bool("x", false, "Actually move files (short for --execute)")
	updateManifestFlag := flag.Bool("update-manifest", false, "Update the manifest CSV after organizing")
	updateManifestShort := flag.Bool("m", false, "Update manifest (short for --update-manifest)")
	rootDir := flag.String("root", "", "Photo library root directory (default: current directory)")
	installSkillFlag := flag.Bool("install-skill", false, "Install Claude Code skill to .claude/skills/")
	initFlag := flag.Bool("init", false, "Initialize photo library directory structure")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Photo Organizer - Organize photos by capture date\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s                  # Preview (dry-run, default)\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -x               # Execute file moves\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -x -m            # Execute and update manifest\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --root /path     # Use custom root directory\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --init           # Initialize photo library structure\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --install-skill  # Install Claude Code skill\n", os.Args[0])
	}

	flag.Parse()

	// Handle library initialization
	if *initFlag {
		targetDir := *rootDir
		if targetDir == "" {
			var err error
			targetDir, err = os.Getwd()
			if err != nil {
				fmt.Println("Error getting current directory:", err)
				os.Exit(1)
			}
		}
		if err := initPhotoLibrary(targetDir); err != nil {
			fmt.Printf("Error initializing library: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Handle skill installation
	if *installSkillFlag {
		targetDir := *rootDir
		if targetDir == "" {
			var err error
			targetDir, err = os.Getwd()
			if err != nil {
				fmt.Println("Error getting current directory:", err)
				os.Exit(1)
			}
		}
		if err := installSkill(targetDir); err != nil {
			fmt.Printf("Error installing skill: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Combine short and long flags
	doExecute := *execute || *executeShort
	doUpdateManifest := *updateManifestFlag || *updateManifestShort
	dryRun := !doExecute

	// Set paths based on root directory
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

	// Validate that Incoming directory exists
	if _, err := os.Stat(incomingDir); os.IsNotExist(err) {
		fmt.Printf("Error: Incoming directory not found at %s\n", incomingDir)
		os.Exit(1)
	}

	// Print banner
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println("Photo Organizer")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Incoming:  %s\n", incomingDir)
	fmt.Printf("Originals: %s\n", originalsDir)
	fmt.Println()

	if dryRun {
		fmt.Println("[DRY RUN MODE - use --execute or -x to actually move files]\n")
	}

	// Run organization
	organized, err := organizeFiles(dryRun)
	if err != nil {
		fmt.Println("Error organizing files:", err)
		os.Exit(1)
	}

	// Post-processing (only when actually executing)
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
