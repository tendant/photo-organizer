package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// extractManifestFields: CSV Field Extraction (Pure Function)
// =============================================================================

// extractManifestFields extracts specific fields from a CSV record using header indices.
// Pure: no side effects, deterministic, testable without I/O.
// Returns a map of field name → value. Missing fields default to empty string.
func extractManifestFields(record []string, headerIndex map[string]int, fieldNames ...string) map[string]string {
	result := make(map[string]string)
	for _, field := range fieldNames {
		if idx, ok := headerIndex[field]; ok && idx < len(record) {
			result[field] = record[idx]
		} else {
			result[field] = ""
		}
	}
	return result
}

// buildHeaderIndex converts a CSV header row to an index map for fast lookups.
// Pure: no side effects, deterministic.
func buildHeaderIndex(headerRow []string) map[string]int {
	index := make(map[string]int)
	for i, h := range headerRow {
		index[h] = i
	}
	return index
}

// =============================================================================
// pathMatches: Path Prefix Matching (Pure Function)
// =============================================================================

// pathMatches checks if scanPath belongs to (or is under) targetPath.
// Handles path normalization and directory boundary checking.
// Pure: no side effects, deterministic.
// Returns true if scanPath == targetPath or scanPath is a child of targetPath.
func pathMatches(scanPath, targetPath string) bool {
	scanPath = filepath.Clean(scanPath)
	targetPath = filepath.Clean(targetPath)

	if scanPath == targetPath {
		return true
	}

	// Check if scanPath is under targetPath (with directory boundary).
	// targetPath must end with separator to ensure we match /a/photos, not /a/photos2.
	prefix := targetPath + string(filepath.Separator)
	return strings.HasPrefix(scanPath, prefix)
}

// =============================================================================
// calculateFolderMetrics: Folder Size & File Count (Pure-ish Function)
// =============================================================================

// FolderMetrics holds the result of a folder walk calculation.
type FolderMetrics struct {
	TotalSize int64
	FileCount int
	Error     error
}

// calculateFolderMetrics walks the given folder and returns total size and file count.
// Note: This function performs I/O (filepath.Walk), but the accumulation logic is pure.
// Returns error if the folder cannot be read.
func calculateFolderMetrics(folderPath string) FolderMetrics {
	var totalSize int64
	var fileCount int

	err := filepath.Walk(folderPath, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			// Skip walk errors (permission denied on subdirs), continue walking
			return nil
		}
		if !info.IsDir() {
			totalSize += info.Size()
			fileCount++
		}
		return nil
	})

	return FolderMetrics{
		TotalSize: totalSize,
		FileCount: fileCount,
		Error:     err,
	}
}

// =============================================================================
// parseArchiveTimestamp: Timestamp Parsing (Pure Function)
// =============================================================================

// parseArchiveTimestamp extracts the timestamp from an archive folder name.
// Expected format: YYYY-MM-DD-HHMMSSname (where HHMMSSname can be HHMMSS-name or HHMMSSname).
// Pure: no side effects, deterministic, error-safe.
func parseArchiveTimestamp(folderName string) (string, error) {
	// Archive folder names are: YYYY-MM-DD-HHMMSSfoldername or YYYY-MM-DD-HHMMSS-foldername
	// Split by "-" gives: [YYYY, MM, DD, HHMMSSfoldername, ...]
	parts := strings.SplitN(folderName, "-", 4)

	// Need at least: YYYY, MM, DD, HHMMSS...
	if len(parts) < 4 {
		return "", fmt.Errorf("invalid archive folder format: %q (expected YYYY-MM-DD-HHMMSS...)", folderName)
	}

	// parts[3] starts with the time component (HHMMSS) followed by folder name
	timeStr := parts[3]
	if len(timeStr) < 6 {
		return "", fmt.Errorf("invalid time component in folder name: %q", folderName)
	}

	// Extract HH:MM:SS from first 6 characters
	timestamp := fmt.Sprintf("%s-%s-%s %s:%s:%s",
		parts[0], parts[1], parts[2],
		timeStr[0:2], timeStr[2:4], timeStr[4:6])

	return timestamp, nil
}

// =============================================================================
// pruneManifestRecords: Prune Logic for Archive Entries (Pure Function)
// =============================================================================

// PruneResult represents the output of pruning a manifest.
type PruneResult struct {
	ToKeep   [][]string // rows to keep (includes header)
	ToDelete int        // count of entries deleted
}

// pruneManifestRecords filters manifest records, keeping only entries whose files exist.
// Pure: filtering logic is deterministic; file existence check is injected.
// The fileExists function is called for each entry that matches archivePath.
func pruneManifestRecords(
	records [][]string,
	headerIndex map[string]int,
	archivePath string,
	fileExists func(path string) bool,
) PruneResult {
	result := PruneResult{
		ToKeep: make([][]string, 0, len(records)),
	}

	if len(records) < 1 {
		return result
	}

	// Always keep header
	result.ToKeep = append(result.ToKeep, records[0])

	for _, record := range records[1:] {
		if len(record) < 2 {
			// Malformed record, skip it
			continue
		}

		fields := extractManifestFields(record, headerIndex, "scan_path", "relative_path")
		scanPath := fields["scan_path"]
		relativePath := fields["relative_path"]

		// Only filter entries matching this archive folder
		if !pathMatches(scanPath, archivePath) {
			// Entry is for a different archive, keep it
			result.ToKeep = append(result.ToKeep, record)
			continue
		}

		// Entry is for this archive. Check if the file still exists.
		fullPath := filepath.Join(scanPath, relativePath)
		if fileExists(fullPath) {
			// File exists, keep it
			result.ToKeep = append(result.ToKeep, record)
		} else {
			// File was deleted, don't keep it
			result.ToDelete++
		}
	}

	return result
}

// =============================================================================
// findStalledManifests: Stalled Manifest Detection (Pure Function)
// =============================================================================

// StalledManifestInfo represents a manifest whose scan path no longer exists.
type StalledManifestInfo struct {
	ManifestPath string
	ScanPath     string
	Machine      string
	LastScanned  string
	EntryCount   int
}

// findStalledManifests identifies manifests whose scan_path no longer exists.
// Pure: filtering and logic are deterministic; file existence check is injected.
// The pathExists function is called to check if each scan path still exists.
func findStalledManifests(
	manifests []*ManifestSource,
	localMachine string,
	allMachines bool,
	pathExists func(path string) bool,
) []StalledManifestInfo {
	var result []StalledManifestInfo

	for _, src := range manifests {
		if len(src.Rows) == 0 {
			continue
		}

		machine := src.Rows[0].MachineName

		// Filter by machine
		if !allMachines && machine != localMachine {
			continue
		}

		scanPath := src.Rows[0].ScanPath
		lastScanned := src.Rows[0].FileModified

		// Check if scan path still exists
		if !pathExists(scanPath) {
			result = append(result, StalledManifestInfo{
				ManifestPath: src.FilePath,
				ScanPath:     scanPath,
				Machine:      machine,
				LastScanned:  lastScanned,
				EntryCount:   len(src.Rows),
			})
		}
	}

	return result
}

// =============================================================================
// generateArchiveFolderName: Archive Timestamp Generation (Pure Function)
// =============================================================================

// generateArchiveFolderName creates a timestamped archive folder name.
// Pure: deterministic, no side effects. Time is passed as parameter for testability.
// Format: YYYY-MM-DD-HHMMSSfoldername (e.g., 2026-06-19-060012DJI_001)
func generateArchiveFolderName(folderName string, timestamp time.Time) string {
	timeStr := timestamp.Format("2006-01-02-150405")
	return fmt.Sprintf("%s%s", timeStr, folderName)
}

// =============================================================================
// parseCollectArgs: Argument Parsing for Collect Command (Pure Function)
// =============================================================================

// CollectArgs represents parsed arguments for the collect command.
type CollectArgs struct {
	FromMachines []string // machines to collect from
	Remaining    []string // remaining args for flag parsing (--add, --list, etc)
}

// parseCollectArgs parses CLI arguments for the collect command.
// Pure: no side effects, deterministic parsing.
// Handles: --from/-f machine, --add/-a machine=host, --list/-l, --root/-r dir, --sync-delete
func parseCollectArgs(args []string) CollectArgs {
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

	return CollectArgs{
		FromMachines: fromMachines,
		Remaining:    remaining,
	}
}
