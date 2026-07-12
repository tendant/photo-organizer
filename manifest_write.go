package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =============================================================================
// Manifest Writing & Maintenance
// =============================================================================

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
