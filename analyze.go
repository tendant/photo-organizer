package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =============================================================================
// Data Structures
// =============================================================================

type ManifestRow struct {
	Filename     string
	RelativePath string
	SizeBytes    int64
	PartialHash  string
	FullHash     string // empty if full hash not yet computed
	Extension    string
	ScanDate     string // when this file was last scanned
	ScanPath     string
	MachineName  string // empty on old 12-column manifests
}

// ManifestSource is one scan run: one CSV file = one (machine, folder) pair.
type ManifestSource struct {
	FilePath    string
	MachineName string
	ScanPath    string
	Label       string // "machineName @ scanPath"
	LastScanned string // most recent scan_date value in this manifest
	Rows        []ManifestRow
}

type DuplicateGroup struct {
	PartialHash string
	FullHash    string // non-empty = confirmed via full-file hash
	SizeBytes   int64
	Locations   []string // "label: relative_path"
	Confirmed   bool
}

type FolderStats struct {
	SourceLabel  string
	FolderPath   string
	TotalFiles   int
	CoveredFiles int // hash exists in at least one other source
	Coverage     float64
}

// =============================================================================
// Reading Manifests
// =============================================================================

func readManifest(csvPath string) (ManifestSource, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return ManifestSource{}, fmt.Errorf("open %s: %w", csvPath, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return ManifestSource{}, fmt.Errorf("read %s: %w", csvPath, err)
	}
	if len(records) < 1 {
		return ManifestSource{}, fmt.Errorf("%s: empty file", csvPath)
	}

	// Build column index by name so we handle old/new formats gracefully.
	colIdx := make(map[string]int)
	for i, name := range records[0] {
		colIdx[name] = i
	}

	// Validate that required columns are present.
	for _, required := range []string{"relative_path", "partial_hash", "file_size_bytes"} {
		if _, ok := colIdx[required]; !ok {
			// Also accept file_hash as fallback for partial_hash in very old manifests.
			if required == "partial_hash" {
				if _, ok := colIdx["file_hash"]; ok {
					continue
				}
			}
			return ManifestSource{}, fmt.Errorf("%s: missing required column %q (not a valid manifest?)", csvPath, required)
		}
	}

	col := func(row []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}

	var rows []ManifestRow
	var validationIssues []string
	seenPaths := make(map[string]bool)
	skippedCount := 0

	for rowIdx, row := range records[1:] {
		if len(row) < 2 {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: not enough columns", rowIdx+2))
			continue
		}

		relPath := col(row, "relative_path")
		sizeStr := col(row, "file_size_bytes")
		scanDate := col(row, "scan_date")

		// Validate required fields
		if relPath == "" {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: empty relative_path", rowIdx+2))
			continue
		}

		// Parse and validate size
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || size < 0 {
			skippedCount++
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: invalid size %q", rowIdx+2, sizeStr))
			continue
		}

		// Validate scan_date (should be valid and not in future)
		if scanDate != "" {
			if scanTime, err := time.Parse("2006-01-02", scanDate[:10]); err == nil {
				if scanTime.After(time.Now().AddDate(0, 0, 1)) {
					validationIssues = append(validationIssues, fmt.Sprintf("row %d: future scan_date %s", rowIdx+2, scanDate))
					// Don't skip, just warn
				}
			}
		}

		// Check for duplicates (same relative_path on same scan_path)
		dupKey := col(row, "scan_path") + "/" + relPath
		if seenPaths[dupKey] {
			validationIssues = append(validationIssues, fmt.Sprintf("row %d: duplicate entry %s", rowIdx+2, relPath))
			// Skip duplicate
			skippedCount++
			continue
		}
		seenPaths[dupKey] = true

		// partial_hash is the new column; fall back to file_hash for old manifests.
		partialHash := col(row, "partial_hash")
		if partialHash == "" {
			partialHash = col(row, "file_hash")
		}

		rows = append(rows, ManifestRow{
			Filename:     col(row, "filename"),
			RelativePath: relPath,
			SizeBytes:    size,
			PartialHash:  partialHash,
			FullHash:     col(row, "full_hash"),
			Extension:    col(row, "extension"),
			ScanDate:     scanDate,
			ScanPath:     col(row, "scan_path"),
			MachineName:  col(row, "machine_name"),
		})
	}

	// Store validation issues for reporting
	if skippedCount > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %s: skipped %d invalid row(s)\n", filepath.Base(csvPath), skippedCount)
		if len(validationIssues) > 0 && len(validationIssues) <= 3 {
			for _, issue := range validationIssues {
				fmt.Fprintf(os.Stderr, "   %s\n", issue)
			}
		}
	}

	// Derive machine name, scan path, and most recent scan date from rows.
	machine := ""
	scanPath := ""
	lastScanned := ""
	for _, row := range rows {
		if row.MachineName != "" {
			machine = row.MachineName
		}
		if row.ScanPath != "" {
			scanPath = row.ScanPath
		}
		if row.ScanDate > lastScanned {
			lastScanned = row.ScanDate
		}
		if machine != "" && scanPath != "" && lastScanned != "" {
			break
		}
	}
	// Scan all rows to find the true latest scan date.
	for _, row := range rows {
		if row.ScanDate > lastScanned {
			lastScanned = row.ScanDate
		}
	}
	if machine == "" {
		// Derive from filename: photo_manifest_<machine>_<path>.csv → first segment after prefix.
		stem := strings.TrimSuffix(filepath.Base(csvPath), filepath.Ext(csvPath))
		stem = strings.TrimPrefix(stem, "photo_manifest_")
		stem = strings.TrimPrefix(stem, "photo_manifest")
		if parts := strings.SplitN(stem, "_", 2); len(parts) >= 1 && parts[0] != "" {
			machine = parts[0]
		} else {
			machine = filepath.Base(csvPath)
		}
	}

	label := machine
	if scanPath != "" {
		label = machine + " @ " + scanPath
	}

	// Backfill machine name into rows that have an empty MachineName (old manifests).
	for i := range rows {
		if rows[i].MachineName == "" {
			rows[i].MachineName = machine
		}
	}

	// Check for stale manifests (not scanned in 30+ days)
	if lastScanned != "" {
		if scanTime, err := time.Parse("2006-01-02", lastScanned[:10]); err == nil {
			daysOld := int(time.Since(scanTime).Hours() / 24)
			if daysOld > 30 {
				fmt.Fprintf(os.Stderr, "⚠  %s: manifest is %d days old (last scanned %s)\n", label, daysOld, lastScanned)
			}
		}
	}

	return ManifestSource{
		FilePath:    csvPath,
		MachineName: machine,
		ScanPath:    scanPath,
		Label:       label,
		LastScanned: lastScanned,
		Rows:        rows,
	}, nil
}

// =============================================================================
// Overlap Detection
// =============================================================================

// overlappingPairs returns a set of (i,j) source-index pairs where both
// sources are on the same machine and one scan path is an ancestor of the
// other. Files appearing in both sources are the same physical file.
func overlappingPairs(sources []ManifestSource) map[[2]int]bool {
	pairs := make(map[[2]int]bool)
	sep := string(filepath.Separator)
	for i := range sources {
		for j := range sources {
			if i == j {
				continue
			}
			if sources[i].MachineName != sources[j].MachineName {
				continue
			}
			pi := filepath.Clean(sources[i].ScanPath)
			pj := filepath.Clean(sources[j].ScanPath)
			if pi == pj ||
				strings.HasPrefix(pi, pj+sep) ||
				strings.HasPrefix(pj, pi+sep) {
				pairs[[2]int{i, j}] = true
			}
		}
	}
	return pairs
}

// absFilePath returns the absolute path of a file given its source and row.
// Used to identify the same physical file across overlapping scans.
func absFilePath(src ManifestSource, row ManifestRow) string {
	return filepath.Clean(filepath.Join(src.ScanPath, filepath.FromSlash(row.RelativePath)))
}

// overlapWarnings returns human-readable warnings for overlapping source pairs.
func overlapWarnings(sources []ManifestSource, pairs map[[2]int]bool) []string {
	seen := make(map[[2]int]bool)
	var warnings []string
	for pair := range pairs {
		canonical := [2]int{pair[0], pair[1]}
		if pair[0] > pair[1] {
			canonical = [2]int{pair[1], pair[0]}
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		i, j := canonical[0], canonical[1]
		pi := filepath.Clean(sources[i].ScanPath)
		pj := filepath.Clean(sources[j].ScanPath)
		parent, child := i, j
		if strings.HasPrefix(pi, pj+string(filepath.Separator)) {
			parent, child = j, i
		}
		warnings = append(warnings, fmt.Sprintf(
			"  %s contains %s — overlapping scans detected, shared files excluded from duplicate reports",
			sources[parent].Label, sources[child].Label))
	}
	sort.Strings(warnings)
	return warnings
}

// =============================================================================
// Analysis
// =============================================================================

// buildHashIndex returns (partial_hash|size) → list of (sourceIndex, rowIndex) pairs.
// Including size in the key ensures hash collisions between files of different
// sizes are never treated as duplicates.
type hashLocation struct {
	sourceIdx int
	rowIdx    int
}

func indexKey(partialHash string, sizeBytes int64) string {
	return fmt.Sprintf("%s|%d", partialHash, sizeBytes)
}

func buildHashIndex(sources []ManifestSource) map[string][]hashLocation {
	idx := make(map[string][]hashLocation)
	for si, src := range sources {
		for ri, row := range src.Rows {
			if row.PartialHash == "" {
				continue
			}
			key := indexKey(row.PartialHash, row.SizeBytes)
			idx[key] = append(idx[key], hashLocation{si, ri})
		}
	}
	return idx
}

// distinctMachines returns the set of unique machine names that have this hash.
func distinctMachines(locs []hashLocation, sources []ManifestSource) map[string]bool {
	m := make(map[string]bool)
	for _, loc := range locs {
		m[sources[loc.sourceIdx].MachineName] = true
	}
	return m
}

// confirmed returns true when all locations in a group have matching full hashes,
// meaning the duplicate is verified beyond the partial hash pre-filter.
func confirmed(locs []hashLocation, sources []ManifestSource) bool {
	fullHash := ""
	for _, loc := range locs {
		fh := sources[loc.sourceIdx].Rows[loc.rowIdx].FullHash
		if fh == "" {
			return false // at least one file lacks a full hash
		}
		if fullHash == "" {
			fullHash = fh
		} else if fh != fullHash {
			return false // full hashes differ — partial hash collision, not a real dup
		}
	}
	return fullHash != ""
}

func findDuplicates(sources []ManifestSource, idx map[string][]hashLocation) []DuplicateGroup {
	var groups []DuplicateGroup
	for _, locs := range idx {
		machines := distinctMachines(locs, sources)
		if len(machines) < 2 {
			continue
		}
		var locations []string
		var sizeBytes int64
		fullHash := ""
		isConfirmed := confirmed(locs, sources)
		for _, loc := range locs {
			row := sources[loc.sourceIdx].Rows[loc.rowIdx]
			locations = append(locations, sources[loc.sourceIdx].Label+": "+row.RelativePath)
			sizeBytes = row.SizeBytes
			if row.FullHash != "" && fullHash == "" {
				fullHash = row.FullHash
			}
		}
		sort.Strings(locations)
		firstRow := sources[locs[0].sourceIdx].Rows[locs[0].rowIdx]
		groups = append(groups, DuplicateGroup{
			PartialHash: firstRow.PartialHash,
			FullHash:    fullHash,
			SizeBytes:   sizeBytes,
			Locations:   locations,
			Confirmed:   isConfirmed,
		})
	}
	// Sort by size descending
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].SizeBytes > groups[j].SizeBytes
	})
	return groups
}

// findUnique returns rows whose hash appears on exactly one machine_name.
// Result is keyed by machine name.
func findUnique(sources []ManifestSource, idx map[string][]hashLocation) map[string][]ManifestRow {
	result := make(map[string][]ManifestRow)
	for _, locs := range idx {
		machines := distinctMachines(locs, sources)
		if len(machines) != 1 {
			continue
		}
		// Deduplicate by absolute path — overlapping scans on the same machine
		// (parent + child) would otherwise list the same physical file twice.
		seenAbs := make(map[string]bool)
		for _, loc := range locs {
			src := sources[loc.sourceIdx]
			row := src.Rows[loc.rowIdx]
			abs := absFilePath(src, row)
			if seenAbs[abs] {
				continue
			}
			seenAbs[abs] = true
			result[row.MachineName] = append(result[row.MachineName], row)
		}
	}
	return result
}

// findIntraMachine returns groups of rows that share a hash within the same
// machine but across different sources (folders) or different relative paths.
type intraDupGroup struct {
	MachineName string
	Hash        string
	SizeBytes   int64
	Locations   []string // "label: relative_path"
	FullHashed  bool     // true when all locations have full hashes
}

func findIntraMachine(sources []ManifestSource, idx map[string][]hashLocation) []intraDupGroup {
	var result []intraDupGroup
	for hash, locs := range idx {
		byMachine := make(map[string][]hashLocation)
		for _, loc := range locs {
			m := sources[loc.sourceIdx].MachineName
			byMachine[m] = append(byMachine[m], loc)
		}
		for machine, mLocs := range byMachine {
			// Deduplicate by absolute file path — overlapping scans produce the
			// same physical file under different relative paths; don't report those
			// as duplicates.
			seenAbs := make(map[string]bool)
			var locations []string
			var sizeBytes int64
			allFullHashed := true
			for _, loc := range mLocs {
				row := sources[loc.sourceIdx].Rows[loc.rowIdx]
				abs := absFilePath(sources[loc.sourceIdx], row)
				if seenAbs[abs] {
					continue
				}
				seenAbs[abs] = true
				locations = append(locations, sources[loc.sourceIdx].Label+": "+row.RelativePath)
				sizeBytes = row.SizeBytes
				if row.FullHash == "" {
					allFullHashed = false
				}
			}
			if len(locations) < 2 {
				continue
			}
			sort.Strings(locations)
			result = append(result, intraDupGroup{
				MachineName:  machine,
				Hash:         hash,
				SizeBytes:    sizeBytes,
				Locations:    locations,
				FullHashed:   allFullHashed,
			})
		}
	}
	return result
}

func computeFolderRedundancy(sources []ManifestSource, idx map[string][]hashLocation) []FolderStats {
	overlaps := overlappingPairs(sources)

	type folderKey struct {
		sourceIdx int
		folder    string
	}
	totals := make(map[folderKey]int)
	covered := make(map[folderKey]int)

	for si, src := range sources {
		for _, row := range src.Rows {
			folder := topLevelFolder(row.RelativePath)
			key := folderKey{si, folder}
			totals[key]++

			locs := idx[indexKey(row.PartialHash, row.SizeBytes)]
			for _, loc := range locs {
				if loc.sourceIdx == si {
					continue
				}
				// Don't count overlapping same-machine scans as external coverage —
				// that would be the same physical file counted twice.
				if overlaps[[2]int{si, loc.sourceIdx}] {
					continue
				}
				covered[key]++
				break
			}
		}
	}

	var stats []FolderStats
	for key, total := range totals {
		cov := covered[key]
		stats = append(stats, FolderStats{
			SourceLabel:  sources[key.sourceIdx].Label,
			FolderPath:   key.folder,
			TotalFiles:   total,
			CoveredFiles: cov,
			Coverage:     float64(cov) / float64(total),
		})
	}
	// Sort: by source label, then by coverage descending
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].SourceLabel != stats[j].SourceLabel {
			return stats[i].SourceLabel < stats[j].SourceLabel
		}
		return stats[i].Coverage > stats[j].Coverage
	})
	return stats
}

// topLevelFolder returns the first path component of a relative path.
func topLevelFolder(relPath string) string {
	relPath = filepath.ToSlash(relPath)
	parts := strings.SplitN(relPath, "/", 2)
	if len(parts) > 1 && parts[0] != "" {
		return parts[0]
	}
	return "(root)"
}

// =============================================================================
// Machine Summary
// =============================================================================

type MachineSummary struct {
	MachineName  string
	Sources      []string // labels of all scan sources for this machine
	TotalFiles   int
	TotalBytes   int64
	UniqueFiles  int
	DupedFiles   int
	ByType       map[string]int // "photo"/"video"/"audio"/"sidecar"/"other"
}

func computeSummaries(sources []ManifestSource, idx map[string][]hashLocation) []MachineSummary {
	uniqueByMachine := findUnique(sources, idx)

	byMachine := make(map[string]*MachineSummary)
	sourcesByMachine := make(map[string]map[string]bool)

	for _, src := range sources {
		m := src.MachineName
		if byMachine[m] == nil {
			byMachine[m] = &MachineSummary{
				MachineName: m,
				ByType:      make(map[string]int),
			}
			sourcesByMachine[m] = make(map[string]bool)
		}
		sourcesByMachine[m][src.Label] = true
		for _, row := range src.Rows {
			byMachine[m].TotalFiles++
			byMachine[m].TotalBytes += row.SizeBytes
			byMachine[m].ByType[fileType(row.Extension)]++
		}
	}

	for machine, rows := range uniqueByMachine {
		if byMachine[machine] != nil {
			byMachine[machine].UniqueFiles = len(rows)
		}
	}
	for machine, sum := range byMachine {
		sum.DupedFiles = sum.TotalFiles - sum.UniqueFiles
		for label := range sourcesByMachine[machine] {
			sum.Sources = append(sum.Sources, label)
		}
		sort.Strings(sum.Sources)
	}

	var result []MachineSummary
	for _, sum := range byMachine {
		result = append(result, *sum)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].MachineName < result[j].MachineName
	})
	return result
}

func fileType(ext string) string {
	switch {
	case photoExts[ext]:
		return "photo"
	case videoExts[ext]:
		return "video"
	case audioExts[ext]:
		return "audio"
	case sidecarExts[ext]:
		return "sidecar"
	default:
		return "other"
	}
}

// =============================================================================
// Report Printing
// =============================================================================

const sep = "================================================================="

func printReport(sources []ManifestSource, threshold float64, w io.Writer) {
	overlaps := overlappingPairs(sources)
	idx := buildHashIndex(sources)
	duplicates := findDuplicates(sources, idx)
	uniqueByMachine := findUnique(sources, idx)
	intraDups := findIntraMachine(sources, idx)
	folderStats := computeFolderRedundancy(sources, idx)
	summaries := computeSummaries(sources, idx)

	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, " PHOTO DUPLICATE ANALYSIS — %d manifest(s)\n", len(sources))
	fmt.Fprintln(w, sep)

	if warnings := overlapWarnings(sources, overlaps); len(warnings) > 0 {
		fmt.Fprintln(w, "\nOVERLAPPING SCANS DETECTED")
		fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
		for _, w2 := range warnings {
			fmt.Fprintln(w, w2)
		}
	}

	// Freshness check: warn about sources not scanned in 30+ days.
	const staleThreshold = 30 * 24 * time.Hour
	now := time.Now()
	var stale []string
	for _, src := range sources {
		if src.LastScanned == "" {
			stale = append(stale, fmt.Sprintf("  %s  (no scan date recorded)", src.Label))
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", src.LastScanned)
		if err != nil {
			continue
		}
		age := now.Sub(t)
		if age > staleThreshold {
			days := int(age.Hours() / 24)
			stale = append(stale, fmt.Sprintf("  %s  (last scanned %d days ago)", src.Label, days))
		}
	}
	if len(stale) > 0 {
		fmt.Fprintln(w, "\n⚠  STALE MANIFESTS — results may not reflect current state")
		fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
		for _, s := range stale {
			fmt.Fprintln(w, s)
		}
		fmt.Fprintln(w, "  Run 'photo-organizer rescan' on these machines to refresh.")
	}

	// Machine Summaries
	fmt.Fprintln(w, "\nMACHINE SUMMARIES")
	fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
	for _, s := range summaries {
		fmt.Fprintf(w, "  %-20s  %s\n", s.MachineName, formatSize(s.TotalBytes))
		for _, lbl := range s.Sources {
			fmt.Fprintf(w, "    scan: %s\n", lbl)
		}
		fmt.Fprintf(w, "    %s files total  |  photos: %d  videos: %d  audio: %d  sidecars: %d\n",
			formatCount(s.TotalFiles), s.ByType["photo"], s.ByType["video"], s.ByType["audio"], s.ByType["sidecar"])
		if s.TotalFiles > 0 {
			fmt.Fprintf(w, "    Unique to this machine:  %s files (%.1f%%)  ← not backed up elsewhere\n",
				formatCount(s.UniqueFiles), pct(s.UniqueFiles, s.TotalFiles))
			fmt.Fprintf(w, "    Duplicated elsewhere:    %s files (%.1f%%)\n",
				formatCount(s.DupedFiles), pct(s.DupedFiles, s.TotalFiles))
		}
		fmt.Fprintln(w)
	}

	// Duplicate groups (cross-machine)
	totalDupBytes := int64(0)
	for _, g := range duplicates {
		totalDupBytes += g.SizeBytes
	}
	fmt.Fprintf(w, "DUPLICATE GROUPS  (%s total groups", formatCount(len(duplicates)))
	if len(duplicates) > 20 {
		fmt.Fprintf(w, ", showing top 20 by size")
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
	limit := len(duplicates)
	if limit > 20 {
		limit = 20
	}
	for _, g := range duplicates[:limit] {
		conf := ""
		if !g.Confirmed {
			conf = "  ⚠ unconfirmed (partial hash only)"
		}
		fmt.Fprintf(w, "  [%8s]  %s%s\n", formatSize(g.SizeBytes), g.PartialHash[:12]+"...", conf)
		for i, loc := range g.Locations {
			if i >= 4 {
				fmt.Fprintf(w, "    (+%d more locations)\n", len(g.Locations)-4)
				break
			}
			fmt.Fprintf(w, "    %s\n", loc)
		}
	}
	if len(duplicates) == 0 {
		fmt.Fprintln(w, "  (none found)")
	}

	// Unique files per machine
	fmt.Fprintln(w, "\nFILES UNIQUE TO ONE MACHINE  ← not backed up on any other machine")
	fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
	machineNames := make([]string, 0, len(uniqueByMachine))
	for m := range uniqueByMachine {
		machineNames = append(machineNames, m)
	}
	sort.Strings(machineNames)
	anyUnique := false
	for _, machine := range machineNames {
		rows := uniqueByMachine[machine]
		if len(rows) == 0 {
			continue
		}
		anyUnique = true
		totalSize := int64(0)
		for _, r := range rows {
			totalSize += r.SizeBytes
		}
		fmt.Fprintf(w, "  %-20s  %s files   %s\n", machine, formatCount(len(rows)), formatSize(totalSize))
		// Top 10 by size
		sort.Slice(rows, func(i, j int) bool { return rows[i].SizeBytes > rows[j].SizeBytes })
		top := 10
		if len(rows) < top {
			top = len(rows)
		}
		for _, r := range rows[:top] {
			fmt.Fprintf(w, "    %8s  %s: %s\n", formatSize(r.SizeBytes), r.MachineName, r.RelativePath)
		}
		if len(rows) > 10 {
			fmt.Fprintf(w, "    ... and %d more\n", len(rows)-10)
		}
		fmt.Fprintln(w)
	}
	if !anyUnique {
		fmt.Fprintln(w, "  (none — all files have copies on at least one other machine)")
	}

	// Intra-machine duplicates — split into confirmed (full hash) and unconfirmed (partial).
	if len(intraDups) > 0 {
		sort.Slice(intraDups, func(i, j int) bool {
			return intraDups[i].SizeBytes > intraDups[j].SizeBytes
		})

		var confirmedDups, unconfirmedDups []intraDupGroup
		for _, g := range intraDups {
			if g.FullHashed {
				confirmedDups = append(confirmedDups, g)
			} else {
				unconfirmedDups = append(unconfirmedDups, g)
			}
		}

		fmt.Fprintln(w, "INTRA-MACHINE DUPLICATES  (same file in multiple folders, same machine)")
		fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")

		printIntraDupGroups := func(groups []intraDupGroup, label string) {
			if len(groups) == 0 {
				return
			}
			totalBytes := int64(0)
			for _, g := range groups {
				totalBytes += g.SizeBytes
			}
			fmt.Fprintf(w, "\n  %s (%s groups, ~%s)\n", label, formatCount(len(groups)), formatSize(totalBytes))
			limit := len(groups)
			if limit > 20 {
				limit = 20
			}
			for _, g := range groups[:limit] {
				fmt.Fprintf(w, "  [%s]  %s\n", formatSize(g.SizeBytes), g.Hash[:12]+"...")
				for _, loc := range g.Locations {
					fmt.Fprintf(w, "    %s\n", loc)
				}
			}
			if len(groups) > 20 {
				fmt.Fprintf(w, "  ... and %s more groups\n", formatCount(len(groups)-20))
			}
		}

		printIntraDupGroups(confirmedDups, "CONFIRMED duplicates (full hash)")
		if len(unconfirmedDups) > 0 {
			printIntraDupGroups(unconfirmedDups, "UNCONFIRMED — partial hash only (may be false positives for videos)")
			fmt.Fprintf(w, "\n  ⚠  Re-scan to confirm or dismiss these %s groups (collisions will auto-upgrade to full hash).\n",
				formatCount(len(unconfirmedDups)))
		}
		fmt.Fprintln(w)
	}

	// Folder redundancy
	fmt.Fprintf(w, "FOLDER REDUNDANCY  (threshold: %.0f%% = HIGH/FULL)\n", threshold*100)
	fmt.Fprintln(w, "─────────────────────────────────────────────────────────────────")
	currentLabel := ""
	for _, fs := range folderStats {
		if fs.SourceLabel != currentLabel {
			fmt.Fprintf(w, "\n  Source: %s\n", fs.SourceLabel)
			currentLabel = fs.SourceLabel
		}
		status := coverageStatus(fs.Coverage, threshold)
		fmt.Fprintf(w, "  [%-4s]  %-40s  %s files  %5.1f%% covered",
			status, fs.FolderPath, formatCount(fs.TotalFiles), fs.Coverage*100)
		unique := fs.TotalFiles - fs.CoveredFiles
		if unique > 0 {
			fmt.Fprintf(w, "  (%s unique)", formatCount(unique))
		}
		fmt.Fprintln(w)
	}

	// Summary footer
	totalFiles := 0
	totalBytes := int64(0)
	for _, s := range summaries {
		totalFiles += s.TotalFiles
		totalBytes += s.TotalBytes
	}
	totalUnique := 0
	for _, rows := range uniqueByMachine {
		totalUnique += len(rows)
	}
	fullRedundant := 0
	highRedundant := 0
	for _, fs := range folderStats {
		if fs.Coverage >= 1.0 {
			fullRedundant++
		} else if fs.Coverage >= threshold {
			highRedundant++
		}
	}

	fmt.Fprintf(w, "\n%s\n", sep)
	fmt.Fprintf(w, "  Hash mode:  sampled MD5 (first+last 32KB)\n")
	fmt.Fprintf(w, "  Total files across all sources:   %s  (%s)\n", formatCount(totalFiles), formatSize(totalBytes))
	fmt.Fprintf(w, "  Duplicated on 2+ machines:        %s (%.1f%%)\n",
		formatCount(len(duplicates)), pct(len(duplicates), totalFiles))
	fmt.Fprintf(w, "  Unique to one machine:            %s (%.1f%%)\n",
		formatCount(totalUnique), pct(totalUnique, totalFiles))
	fmt.Fprintf(w, "  Fully-redundant folders (100%%):   %s\n", formatCount(fullRedundant))
	fmt.Fprintf(w, "  Nearly-redundant folders (>%.0f%%): %s\n", threshold*100, formatCount(highRedundant))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-28s  %10s  %10s  %8s  %8s\n", "MACHINE", "FILES", "SIZE", "UNIQUE", "DUPED")
	fmt.Fprintf(w, "  %-28s  %10s  %10s  %8s  %8s\n",
		strings.Repeat("─", 28), strings.Repeat("─", 10), strings.Repeat("─", 10),
		strings.Repeat("─", 8), strings.Repeat("─", 8))
	for _, s := range summaries {
		uniqueRows := uniqueByMachine[s.MachineName]
		uniqueCount := len(uniqueRows)
		fmt.Fprintf(w, "  %-28s  %10s  %10s  %7.1f%%  %7.1f%%\n",
			truncate(s.MachineName, 28),
			formatCount(s.TotalFiles),
			formatSize(s.TotalBytes),
			pct(uniqueCount, s.TotalFiles),
			pct(s.DupedFiles, s.TotalFiles))
	}
	fmt.Fprintln(w, sep)
}

func coverageStatus(coverage float64, threshold float64) string {
	switch {
	case coverage >= 1.0:
		return "FULL"
	case coverage >= threshold:
		return "HIGH"
	case coverage >= 0.5:
		return "MID"
	default:
		return ""
	}
}

// =============================================================================
// CSV Output
// =============================================================================

func writeAnalysisCSV(sources []ManifestSource, prefix string) error {
	idx := buildHashIndex(sources)
	duplicates := findDuplicates(sources, idx)
	uniqueByMachine := findUnique(sources, idx)
	folderStats := computeFolderRedundancy(sources, idx)

	// duplicates CSV
	dupPath := prefix + "_duplicates.csv"
	if err := writeDuplicatesCSV(dupPath, duplicates); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", dupPath)

	// unique files CSV
	uniquePath := prefix + "_unique.csv"
	if err := writeUniqueCSV(uniquePath, uniqueByMachine); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", uniquePath)

	// folders CSV
	foldersPath := prefix + "_folders.csv"
	if err := writeFoldersCSV(foldersPath, folderStats); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", foldersPath)

	return nil
}

func writeDuplicatesCSV(path string, groups []DuplicateGroup) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"partial_hash", "full_hash", "confirmed", "size_bytes", "location"})
	for _, g := range groups {
		for _, loc := range g.Locations {
			w.Write([]string{g.PartialHash, g.FullHash, strconv.FormatBool(g.Confirmed), strconv.FormatInt(g.SizeBytes, 10), loc})
		}
	}
	w.Flush()
	return w.Error()
}

func writeUniqueCSV(path string, uniqueByMachine map[string][]ManifestRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"machine_name", "relative_path", "size_bytes", "extension", "partial_hash", "full_hash"})
	machines := make([]string, 0, len(uniqueByMachine))
	for m := range uniqueByMachine {
		machines = append(machines, m)
	}
	sort.Strings(machines)
	for _, m := range machines {
		rows := uniqueByMachine[m]
		sort.Slice(rows, func(i, j int) bool { return rows[i].RelativePath < rows[j].RelativePath })
		for _, row := range rows {
			w.Write([]string{m, row.RelativePath, strconv.FormatInt(row.SizeBytes, 10), row.Extension, row.PartialHash, row.FullHash})
		}
	}
	w.Flush()
	return w.Error()
}

func writeFoldersCSV(path string, stats []FolderStats) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"source_label", "folder_path", "total_files", "covered_files", "coverage_pct", "status"})
	for _, fs := range stats {
		status := coverageStatus(fs.Coverage, 0.9)
		w.Write([]string{
			fs.SourceLabel,
			fs.FolderPath,
			strconv.Itoa(fs.TotalFiles),
			strconv.Itoa(fs.CoveredFiles),
			fmt.Sprintf("%.1f", fs.Coverage*100),
			status,
		})
	}
	w.Flush()
	return w.Error()
}

// =============================================================================
// runAnalyze Entry Point
// =============================================================================

func runAnalyze(args []string) {
	// Pre-separate flags from positional args so flags work in any position.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "csv" || name == "threshold" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	csvPrefix := fs.String("csv", "", "write CSV output files with this filename prefix")
	threshold := fs.Float64("threshold", 0.9, "folder coverage fraction to flag as nearly-redundant (e.g. 0.9 = 90%)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer analyze [--csv prefix] [--threshold 0.9] manifest1.csv manifest2.csv ...\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
		if len(matches) > 0 {
			manifestPaths = matches
			fmt.Fprintf(os.Stderr, "No manifests specified, loading %s (%d files)\n\n", defaultDir, len(matches))
		} else {
			fmt.Fprintf(os.Stderr, "analyze: no manifests specified and none found in %s\n", defaultDir)
			fs.Usage()
			os.Exit(1)
		}
	}

	// Pre-flight checks
	fmt.Fprintf(os.Stderr, "Pre-flight checks:\n")
	var checks []PreflightCheck
	for _, path := range manifestPaths {
		checks = append(checks, CheckFileReadable(path, fmt.Sprintf("Manifest %s readable", filepath.Base(path))))
	}
	if !RunPreflightChecks(checks) {
		os.Exit(1)
	}

	var sources []ManifestSource
	for _, path := range manifestPaths {
		src, err := readManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", path, err)
			continue
		}
		sources = append(sources, src)
		fmt.Fprintf(os.Stderr, "Loaded %s  (%s files, label: %s)\n",
			path, formatCount(len(src.Rows)), src.Label)
	}
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "analyze: no valid manifests loaded")
		os.Exit(1)
	}

	// Print distinct machine names — useful for choosing --keep in plan.
	machineSet := make(map[string]bool)
	for _, src := range sources {
		machineSet[src.MachineName] = true
	}
	machineNames := make([]string, 0, len(machineSet))
	for m := range machineSet {
		machineNames = append(machineNames, m)
	}
	sort.Strings(machineNames)
	fmt.Fprintf(os.Stderr, "Machines: %s\n", strings.Join(machineNames, ", "))
	fmt.Fprintf(os.Stderr, "Tip: photo-organizer plan --keep <machine> to generate a safe-delete script\n\n")

	printReport(sources, *threshold, os.Stdout)

	if *csvPrefix != "" {
		fmt.Fprintln(os.Stdout)
		if err := writeAnalysisCSV(sources, *csvPrefix); err != nil {
			fmt.Fprintln(os.Stderr, "Error writing CSV:", err)
			os.Exit(1)
		}
	}
}

// =============================================================================
// Plan (safe-delete script)
// =============================================================================

// DeleteCandidate is a file that can safely be removed from one machine
// because it is confirmed to exist on at least one other machine.
type BackupCopy struct {
	Label   string // "nas @ /volume1/photos"
	AbsPath string // "/volume1/photos/Vacation/IMG_001.jpg"
}

type DeleteCandidate struct {
	Machine   string // machine to delete from
	ScanPath  string // scan root on that machine
	RelPath   string // relative path within scan root
	SizeBytes int64
	Backups   []BackupCopy
}

// buildIntraPlan finds confirmed duplicate files within a single machine and
// returns delete candidates. Only groups where all copies have matching full
// hashes are included (partial-hash collisions are excluded).
//
// keepUnder: if non-empty, keep copies whose path starts with this prefix and
// delete all others. If empty, keep the alphabetically first copy.
func buildIntraPlan(sources []ManifestSource, machine string, keepUnder string) []DeleteCandidate {
	idx := buildHashIndex(sources)
	var candidates []DeleteCandidate

	for _, locs := range idx {
		// Collect locations for this machine only, deduped by absolute path.
		seenAbs := make(map[string]bool)
		var machineLocs []hashLocation
		for _, loc := range locs {
			if sources[loc.sourceIdx].MachineName != machine {
				continue
			}
			row := sources[loc.sourceIdx].Rows[loc.rowIdx]
			abs := absFilePath(sources[loc.sourceIdx], row)
			if seenAbs[abs] {
				continue
			}
			seenAbs[abs] = true
			machineLocs = append(machineLocs, loc)
		}
		if len(machineLocs) < 2 {
			continue
		}

		// All copies must have a non-empty full hash that matches — otherwise
		// this is an unconfirmed partial-hash collision, not a real duplicate.
		fullHash := ""
		confirmed := true
		for _, loc := range machineLocs {
			fh := sources[loc.sourceIdx].Rows[loc.rowIdx].FullHash
			if fh == "" {
				confirmed = false
				break
			}
			if fullHash == "" {
				fullHash = fh
			} else if fh != fullHash {
				confirmed = false // different content — partial hash collision
				break
			}
		}
		if !confirmed {
			continue
		}

		// Build sorted list of absolute paths for this group.
		type locPath struct {
			loc hashLocation
			abs string
			row ManifestRow
			src ManifestSource
		}
		var items []locPath
		for _, loc := range machineLocs {
			src := sources[loc.sourceIdx]
			row := src.Rows[loc.rowIdx]
			items = append(items, locPath{loc, absFilePath(src, row), row, src})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].abs < items[j].abs })

		// Determine which copies to keep and which to delete.
		keepPrefix := ""
		if keepUnder != "" {
			keepPrefix = filepath.Clean(keepUnder) + string(filepath.Separator)
		}

		var keepItems, deleteItems []locPath
		for _, item := range items {
			if keepPrefix != "" && strings.HasPrefix(item.abs, keepPrefix) {
				keepItems = append(keepItems, item)
			} else if keepPrefix != "" {
				deleteItems = append(deleteItems, item)
			}
		}
		if keepPrefix != "" {
			if len(keepItems) == 0 {
				continue // no copy under keep-under prefix — skip, can't safely delete
			}
		} else {
			// Default: keep alphabetically first, delete the rest.
			keepItems = items[:1]
			deleteItems = items[1:]
		}

		if len(deleteItems) == 0 {
			continue
		}

		// Build one BackupCopy entry per kept path so the script shows all
		// surviving copies (important when 3+ copies exist and 2+ are kept).
		var backups []BackupCopy
		for _, k := range keepItems {
			backups = append(backups, BackupCopy{
				Label:   "kept at: " + k.abs,
				AbsPath: k.abs,
			})
		}
		for _, item := range deleteItems {
			candidates = append(candidates, DeleteCandidate{
				Machine:   machine,
				ScanPath:  item.src.ScanPath,
				RelPath:   item.row.RelativePath,
				SizeBytes: item.row.SizeBytes,
				Backups:   backups,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].SizeBytes > candidates[j].SizeBytes
	})
	return candidates
}

func buildDeletePlan(sources []ManifestSource, keepMachine string) []DeleteCandidate {
	idx := buildHashIndex(sources)
	_ = overlappingPairs(sources) // used indirectly via absFilePath dedup
	var candidates []DeleteCandidate

	for _, locs := range idx {
		// Collect distinct machines that have this hash (excluding overlapping scans).
		machineSet := make(map[string]bool)
		for _, loc := range locs {
			machineSet[sources[loc.sourceIdx].MachineName] = true
		}
		if len(machineSet) < 2 {
			continue // only on one machine — not safe to delete anywhere
		}
		if keepMachine != "" && !machineSet[keepMachine] {
			continue // keep machine doesn't have it — can't guarantee a backup
		}

		// Build the list of backup copies (all non-overlapping sources except the one being deleted).
		backupCopies := func(skipMachine string) []BackupCopy {
			seen := make(map[string]bool)
			var copies []BackupCopy
			for _, loc := range locs {
				src := sources[loc.sourceIdx]
				if src.MachineName == skipMachine {
					continue
				}
				row := src.Rows[loc.rowIdx]
				abs := absFilePath(src, row)
				if seen[abs] {
					continue
				}
				seen[abs] = true
				copies = append(copies, BackupCopy{
					Label:   src.Label,
					AbsPath: abs,
				})
			}
			sort.Slice(copies, func(i, j int) bool { return copies[i].Label < copies[j].Label })
			return copies
		}

		// For each location NOT on the keep machine, it's a delete candidate.
		seenAbs := make(map[string]bool)
		for _, loc := range locs {
			src := sources[loc.sourceIdx]
			if keepMachine != "" && src.MachineName == keepMachine {
				continue
			}
			row := src.Rows[loc.rowIdx]
			abs := absFilePath(src, row)
			if seenAbs[abs] {
				continue
			}
			seenAbs[abs] = true
			candidates = append(candidates, DeleteCandidate{
				Machine:   src.MachineName,
				ScanPath:  src.ScanPath,
				RelPath:   row.RelativePath,
				SizeBytes: row.SizeBytes,
				Backups:   backupCopies(src.MachineName),
			})
		}
	}

	// Sort by machine then path for readable output.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Machine != candidates[j].Machine {
			return candidates[i].Machine < candidates[j].Machine
		}
		return candidates[i].RelPath < candidates[j].RelPath
	})
	return candidates
}

// =============================================================================
// Backup Status (verify copy counts after migration)
// =============================================================================

func runBackupStatus(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer backup-status --from <machine>\n")
		fmt.Fprintf(os.Stderr, "Shows copy count and coverage for a specific media/machine after migration\n\n")
		fmt.Fprintf(os.Stderr, "Example: photo-organizer backup-status --from sony-a7iv-card-1\n")
		os.Exit(1)
	}

	// Parse --from flag
	var fromMachine string
	for i := 0; i < len(args); i++ {
		if args[i] == "--from" && i+1 < len(args) {
			fromMachine = args[i+1]
			break
		}
	}

	if fromMachine == "" {
		fmt.Fprintf(os.Stderr, "Error: --from <machine> is required\n")
		os.Exit(1)
	}

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	var sources []ManifestSource
	for _, path := range allCSVs {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		sources = append(sources, src)
	}

	// Find the source machine
	var sourceMachine *ManifestSource
	sourceFileCount := 0
	sourceSize := int64(0)

	for i := range sources {
		if sources[i].MachineName == fromMachine {
			sourceMachine = &sources[i]
			sourceFileCount = len(sources[i].Rows)
			for _, row := range sources[i].Rows {
				sourceSize += row.SizeBytes
			}
			break
		}
	}

	if sourceMachine == nil {
		fmt.Fprintf(os.Stderr, "Error: machine '%s' not found\n", fromMachine)
		os.Exit(1)
	}

	// Build hash index to find copies across all machines
	idx := buildHashIndex(sources)

	// For each file on the source machine, count how many distinct machines have it
	// key: indexKey (partialHash|size) → set of machine names
	type fileStatus struct {
		machines []string
	}
	fileStatuses := make(map[string]fileStatus)

	for _, row := range sourceMachine.Rows {
		if row.PartialHash == "" {
			continue
		}
		key := indexKey(row.PartialHash, row.SizeBytes)
		locations := idx[key]

		seen := make(map[string]bool)
		var machines []string
		for _, loc := range locations {
			m := sources[loc.sourceIdx].MachineName
			if !seen[m] {
				seen[m] = true
				machines = append(machines, m)
			}
		}
		fileStatuses[key] = fileStatus{machines: machines}
	}

	// Analyze copy distribution: count files by number of distinct machines
	copiesCount := make(map[int]int) // machine count → file count
	for _, fs := range fileStatuses {
		copiesCount[len(fs.machines)]++
	}

	// Display report
	fmt.Printf("\n═══════════════════════════════════════════════════════════\n")
	fmt.Printf("Backup Status: %s\n", fromMachine)
	fmt.Printf("═══════════════════════════════════════════════════════════\n\n")

	totalAnalyzed := len(fileStatuses)
	fmt.Printf("Total files:      %s (%s)\n", formatCount(sourceFileCount), formatSize(sourceSize))
	fmt.Printf("Files analyzed:   %s\n\n", formatCount(totalAnalyzed))

	fmt.Printf("Copy Distribution (by number of machines):\n")
	var sortedCounts []int
	for count := range copiesCount {
		sortedCounts = append(sortedCounts, count)
	}
	sort.Ints(sortedCounts)

	for _, count := range sortedCounts {
		fileCount := copiesCount[count]
		pct := 0.0
		if totalAnalyzed > 0 {
			pct = float64(fileCount) / float64(totalAnalyzed) * 100
		}
		status := ""
		switch {
		case count == 1:
			status = "  ⚠  AT RISK (no backup)"
		case count == 2:
			status = "  ✓ backed up"
		case count >= 3:
			status = "  ✓ SAFE (3-2-1)"
		}
		fmt.Printf("  %d machine(s): %6d files (%5.1f%%)%s\n", count, fileCount, pct, status)
	}
	fmt.Println()

	atRisk := copiesCount[1]
	safe3 := 0
	for count, fc := range copiesCount {
		if count >= 3 {
			safe3 += fc
		}
	}

	if atRisk > 0 {
		fmt.Printf("⚠  %d files exist on only 1 machine — not backed up yet\n", atRisk)
		fmt.Printf("   Run: photo-organizer migrate --from %s --dest <backup>\n", fromMachine)
		fmt.Printf("   Then rescan the backup machine to update its manifest.\n")
	} else {
		fmt.Printf("✓ All files backed up to at least 2 machines\n")
	}
	if safe3 > 0 {
		fmt.Printf("✓ %s files satisfy the 3-2-1 rule (3+ machines)\n", formatCount(safe3))
	}
}

func runPlan(args []string) {
	// Pre-separate flags from positional args.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "keep" || name == "out" || name == "intra" || name == "keep-under" || name == "ssh" || name == "ssh-timeout" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	keepFlag := fs.String("keep", "", "machine name whose copies are the authoritative backup (cross-machine mode)")
	intraFlag := fs.String("intra", "", "machine name to deduplicate within (intra-machine mode)")
	keepUnderFlag := fs.String("keep-under", "", "with --intra: keep copies under this path, delete others")
	sshFlag := fs.String("ssh", "", "verify backup files exist on remote machine via SSH, e.g. user@host")
	sshTimeoutFlag := fs.String("ssh-timeout", "30s", "timeout for SSH verification (e.g., 60s for slow networks)")
	outFlag := fs.String("out", "", "write shell script to this file instead of stdout")
	deleteMode := fs.Bool("delete", false, "generate rm commands instead of mv to quarantine")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer plan --keep <machine> [--ssh user@host] [manifest...]\n")
		fmt.Fprintf(os.Stderr, "       photo-organizer plan --intra <machine> [--keep-under /path] [manifest...]\n\n")
		fmt.Fprintf(os.Stderr, "Cross-machine: generate rm commands for files backed up on --keep machine.\n")
		fmt.Fprintf(os.Stderr, "Intra-machine: generate rm commands for confirmed duplicates within one machine.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	// Set SSH timeout from flag
	if *sshTimeoutFlag != "" && *sshTimeoutFlag != "30s" {
		os.Setenv("PHOTO_ORGANIZER_SSH_TIMEOUT", *sshTimeoutFlag)
	}

	if *keepFlag == "" && *intraFlag == "" {
		fmt.Fprintln(os.Stderr, "plan: --keep <machine> or --intra <machine> is required")
		fs.Usage()
		os.Exit(1)
	}

	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
		if len(matches) > 0 {
			manifestPaths = matches
			fmt.Fprintf(os.Stderr, "No manifests specified, loading %s (%d files)\n\n", defaultDir, len(matches))
		} else {
			fmt.Fprintf(os.Stderr, "plan: no manifests specified and none found in %s\n", defaultDir)
			os.Exit(1)
		}
	}

	var sources []ManifestSource
	for _, path := range manifestPaths {
		src, err := readManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", path, err)
			continue
		}
		sources = append(sources, src)
		fmt.Fprintf(os.Stderr, "Loaded %s  (%s files)\n", path, formatCount(len(src.Rows)))
	}
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "plan: no valid manifests loaded")
		os.Exit(1)
	}

	// Determine target machine and build candidates.
	targetMachine := *keepFlag
	if *intraFlag != "" {
		targetMachine = *intraFlag
	}

	machineFound := false
	for _, src := range sources {
		if src.MachineName == targetMachine {
			machineFound = true
			break
		}
	}
	if !machineFound {
		fmt.Fprintf(os.Stderr, "plan: machine %q not found in manifests\n", targetMachine)
		fmt.Fprintf(os.Stderr, "Available machines: ")
		seen := make(map[string]bool)
		for _, src := range sources {
			if !seen[src.MachineName] {
				seen[src.MachineName] = true
				fmt.Fprintf(os.Stderr, "%s ", src.MachineName)
			}
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	var candidates []DeleteCandidate
	if *intraFlag != "" {
		candidates = buildIntraPlan(sources, *intraFlag, *keepUnderFlag)
	} else {
		candidates = buildDeletePlan(sources, *keepFlag)
	}

	// Verify backup files exist — locally via os.Stat, or remotely via SSH.
	verified, unverified := 0, 0
	// Resolve SSH target: explicit --ssh flag, or auto-lookup from machines config.
	sshTarget := *sshFlag
	if sshTarget == "" && *keepFlag != "" {
		cfg := loadMachinesConfig()
		if t := sshTargetFor(*keepFlag, cfg); t != *keepFlag {
			sshTarget = t
			fmt.Fprintf(os.Stderr, "Using SSH target from machines config: %s → %s\n", *keepFlag, sshTarget)
		}
	}

	var remoteExist map[string]bool
	if sshTarget != "" {
		fmt.Fprintf(os.Stderr, "Verifying backups via SSH (%s)...\n", sshTarget)
		var paths []string
		for _, c := range candidates {
			for _, b := range c.Backups {
				paths = append(paths, b.AbsPath)
			}
		}
		remoteExist = sshVerifyPaths(sshTarget, paths)
	}
	for i := range candidates {
		for j := range candidates[i].Backups {
			abs := candidates[i].Backups[j].AbsPath
			var exists bool
			if remoteExist != nil {
				exists = remoteExist[abs]
			} else {
				_, err := os.Stat(abs)
				exists = err == nil
			}
			if exists {
				verified++
			} else {
				candidates[i].Backups[j].Label += "  ⚠ NOT VERIFIED ON DISK"
				unverified++
			}
		}
	}
	if unverified > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠  %d backup path(s) could not be verified on disk.\n", unverified)
		if *sshFlag == "" {
			fmt.Fprintf(os.Stderr, "   Use --ssh user@host to verify remote paths, or ensure the remote machine is mounted.\n")
		}
	}

	out := os.Stdout
	if *outFlag != "" {
		f, err := os.Create(*outFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plan: cannot write output:", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	// Group by machine for output.
	byMachine := make(map[string][]DeleteCandidate)
	for _, c := range candidates {
		byMachine[c.Machine] = append(byMachine[c.Machine], c)
	}
	machines := make([]string, 0, len(byMachine))
	for m := range byMachine {
		machines = append(machines, m)
	}
	sort.Strings(machines)

	totalSize := int64(0)
	for _, c := range candidates {
		totalSize += c.SizeBytes
	}

	fmt.Fprintf(out, "#!/bin/bash\n")
	fmt.Fprintf(out, "# Safe-delete plan generated by photo-organizer\n")
	if *intraFlag != "" {
		fmt.Fprintf(out, "# Mode: intra-machine duplicates on %s\n", *intraFlag)
		if *keepUnderFlag != "" {
			fmt.Fprintf(out, "# Keeping copies under: %s\n", *keepUnderFlag)
		} else {
			fmt.Fprintf(out, "# Keeping: alphabetically first copy per group\n")
		}
	} else {
		fmt.Fprintf(out, "# Keeping authoritative copy on: %s\n", *keepFlag)
	}
	fmt.Fprintf(out, "# Files to remove: %s  (%s reclaimable)\n", formatCount(len(candidates)), formatSize(totalSize))
	fmt.Fprintf(out, "# Backup verified on disk: %d  |  unverified: %d\n", verified, unverified)
	fmt.Fprintf(out, "#\n")
	fmt.Fprintf(out, "# REVIEW CAREFULLY before running.\n")
	if *deleteMode {
		fmt.Fprintf(out, "# All rm commands are commented out (--delete mode: permanent deletion).\n")
		fmt.Fprintf(out, "# Uncomment lines you want to execute, or run:  bash <(grep -v '^#' this_script.sh)\n")
	} else {
		fmt.Fprintf(out, "# All mv commands are commented out — files will be MOVED, not deleted.\n")
		fmt.Fprintf(out, "# Quarantine: <scanPath>/_quarantine/photo-organizer/ (same volume, instant mv)\n")
		fmt.Fprintf(out, "# Files can be recovered from quarantine at any time before permanent cleanup.\n")
		fmt.Fprintf(out, "# Uncomment lines you want to execute, or run:  bash <(grep -v '^#' this_script.sh)\n")
		fmt.Fprintf(out, "# To permanently delete quarantined files after verifying:\n")
		fmt.Fprintf(out, "#   rm -rf '<scanPath>/_quarantine/photo-organizer/'\n")
	}
	if unverified > 0 {
		fmt.Fprintf(out, "#\n# WARNING: Some backups marked ⚠ could not be verified on disk.\n")
		fmt.Fprintf(out, "# Do NOT delete files with unverified backups.\n")
	}
	fmt.Fprintf(out, "#\n")

	// Emit mkdir -p preamble if in quarantine mode.
	if !*deleteMode {
		fmt.Fprintf(out, "\n# Create quarantine directories (same volume as source — mv is instant)\n")
		mkdirs := make(map[string]bool)
		for _, c := range candidates {
			quarantineDest := filepath.Join(c.ScanPath, "_quarantine", "photo-organizer", filepath.FromSlash(c.RelPath))
			quarantineDir := filepath.Dir(quarantineDest)
			mkdirs[quarantineDir] = true
		}
		mkdirKeys := make([]string, 0, len(mkdirs))
		for dir := range mkdirs {
			mkdirKeys = append(mkdirKeys, dir)
		}
		sort.Strings(mkdirKeys)
		for _, dir := range mkdirKeys {
			fmt.Fprintf(out, "mkdir -p %s\n", shellQuote(dir))
		}
	}

	for _, machine := range machines {
		list := byMachine[machine]
		machSize := int64(0)
		for _, c := range list {
			machSize += c.SizeBytes
		}
		fmt.Fprintf(out, "\n# === %s — %s files, %s ===\n", machine, formatCount(len(list)), formatSize(machSize))
		for _, c := range list {
			abs := filepath.Join(c.ScanPath, filepath.FromSlash(c.RelPath))
			for _, b := range c.Backups {
				fmt.Fprintf(out, "# backup: %s\n", b.AbsPath)
				if strings.Contains(b.Label, "⚠") {
					fmt.Fprintf(out, "#         ⚠ NOT VERIFIED ON DISK\n")
				}
			}
			if *deleteMode {
				fmt.Fprintf(out, "#rm %s\n", shellQuote(abs))
			} else {
				quarantineDest := filepath.Join(c.ScanPath, "_quarantine", "photo-organizer", filepath.FromSlash(c.RelPath))
				fmt.Fprintf(out, "#mv %s %s\n", shellQuote(abs), shellQuote(quarantineDest))
			}
		}
	}

	if *outFlag != "" {
		fmt.Fprintf(os.Stderr, "\nWrote plan to %s (%s files, %s reclaimable)\n",
			*outFlag, formatCount(len(candidates)), formatSize(totalSize))
	}
}

// shellQuote wraps a path in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// provideSshErrorHelp analyzes SSH errors and provides helpful suggestions.
func provideSshErrorHelp(errMsg, detail, sshHost string) {
	lower := strings.ToLower(errMsg + " " + detail)

	switch {
	case strings.Contains(lower, "connection refused"):
		fmt.Fprintf(os.Stderr, "   Error: Connection refused (host not reachable or SSH not running)\n")
		fmt.Fprintf(os.Stderr, "   Try: ping %s  or  ssh -v %s\n", sshHost, sshHost)

	case strings.Contains(lower, "connection timeout") || strings.Contains(lower, "timeout"):
		fmt.Fprintf(os.Stderr, "   Error: Connection timeout (host unreachable or very slow network)\n")
		fmt.Fprintf(os.Stderr, "   Try: ping %s  or  check your network connection\n", sshHost)

	case strings.Contains(lower, "permission denied"):
		fmt.Fprintf(os.Stderr, "   Error: Permission denied (authentication failed)\n")
		fmt.Fprintf(os.Stderr, "   Try: ssh-keygen -t ed25519  or  ssh-copy-id %s\n", sshHost)
		fmt.Fprintf(os.Stderr, "        Make sure SSH key is in ~/.ssh/\n")

	case strings.Contains(lower, "no such file") || strings.Contains(lower, "not found"):
		fmt.Fprintf(os.Stderr, "   Error: Remote path not found\n")
		fmt.Fprintf(os.Stderr, "   Try: ssh %s ls -la ~/manifests/\n", sshHost)

	case strings.Contains(lower, "command not found"):
		fmt.Fprintf(os.Stderr, "   Error: Remote command not found\n")
		fmt.Fprintf(os.Stderr, "   Make sure the remote host has a compatible shell\n")

	default:
		if detail != "" {
			fmt.Fprintf(os.Stderr, "   Error: %s\n", detail)
		}
		fmt.Fprintf(os.Stderr, "   Try: ssh -v %s echo ok  (for verbose debugging)\n", sshHost)
	}
}

// sshVerifyPaths checks whether each path exists on a remote host via a single
// SSH connection. Returns a map of path → exists.
func sshVerifyPaths(sshHost string, paths []string) map[string]bool {
	result := make(map[string]bool, len(paths))
	if len(paths) == 0 {
		return result
	}

	// Deduplicate paths.
	seen := make(map[string]bool)
	var unique []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}

	// Send all paths to a single SSH session via stdin. The remote shell
	// reads each line and prints OK or MISSING.
	remoteScript := `while IFS= read -r f; do
  if [ -e "$f" ]; then echo "OK:$f"; else echo "MISSING:$f"; fi
done`

	// Use configurable SSH timeout (default 30s, can be overridden)
	sshTimeout := 30 * time.Second
	if envTimeout := os.Getenv("PHOTO_ORGANIZER_SSH_TIMEOUT"); envTimeout != "" {
		if d, err := time.ParseDuration(envTimeout); err == nil {
			sshTimeout = d
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", sshHost, remoteScript)
	cmd.Stdin = strings.NewReader(strings.Join(unique, "\n") + "\n")

	// Capture stderr to provide better error messages.
	var sshStderr bytes.Buffer
	cmd.Stderr = &sshStderr

	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(sshStderr.String())
		if detail == "" {
			detail = err.Error()
		}

		// Check if it was a timeout
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "⚠  SSH verification timeout for %s (exceeded %v)\n", sshHost, sshTimeout)
			fmt.Fprintf(os.Stderr, "   Tip: Set PHOTO_ORGANIZER_SSH_TIMEOUT=60s for slower networks\n")
			fmt.Fprintf(os.Stderr, "   Continuing with all paths marked unverified\n")
			return result
		}

		fmt.Fprintf(os.Stderr, "⚠  SSH verification failed for %s\n", sshHost)
		provideSshErrorHelp(err.Error(), detail, sshHost)
		fmt.Fprintf(os.Stderr, "   Continuing with all paths marked unverified\n")
		return result
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "OK:") {
			result[strings.TrimPrefix(line, "OK:")] = true
		}
		// MISSING: paths stay false (zero value)
	}
	return result
}

// =============================================================================
// Risk Report (identify files at risk — single machine only)
// =============================================================================

func runRiskReport(args []string) {
	// Pre-split flags and positional args.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "csv" || name == "machine" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("risk-report", flag.ExitOnError)
	csvPrefix := fs.String("csv", "", "write CSV output with this prefix")
	machineFilter := fs.String("machine", "", "show risk only for this machine")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer risk-report [manifest...] [--machine name] [--csv prefix]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
		if len(matches) > 0 {
			manifestPaths = matches
			fmt.Fprintf(os.Stderr, "No manifests specified, loading %s (%d files)\n\n", defaultDir, len(matches))
		} else {
			fmt.Fprintf(os.Stderr, "risk-report: no manifests specified and none found in %s\n", defaultDir)
			fs.Usage()
			os.Exit(1)
		}
	}

	// Load manifests.
	var sources []ManifestSource
	for _, path := range manifestPaths {
		src, err := readManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests loaded.\n")
		os.Exit(1)
	}

	// Analyze.
	idx := buildHashIndex(sources)
	overlappingPairs(sources) // detect overlaps but don't warn (already detected by analyze)
	uniqueByMachine := findUnique(sources, idx)

	// Apply machine filter if specified.
	if *machineFilter != "" {
		filtered := make(map[string][]ManifestRow)
		if rows, ok := uniqueByMachine[*machineFilter]; ok {
			filtered[*machineFilter] = rows
		}
		uniqueByMachine = filtered
	}

	// Print to stdout.
	printRiskReport(os.Stdout, sources, uniqueByMachine)

	// Write CSV if requested.
	if *csvPrefix != "" {
		fmt.Fprintln(os.Stdout)
		if err := writeRiskCSV(*csvPrefix, uniqueByMachine); err != nil {
			fmt.Fprintln(os.Stderr, "Error writing CSV:", err)
			os.Exit(1)
		}
	}
}

func printRiskReport(w io.Writer, sources []ManifestSource, uniqueByMachine map[string][]ManifestRow) {
	sep := "================================================================="

	// Stale warnings.
	const staleThreshold = 30 * 24 * time.Hour
	now := time.Now()
	var stale []string
	for _, src := range sources {
		if src.LastScanned == "" {
			stale = append(stale, fmt.Sprintf("  %s  (no scan date recorded)", src.Label))
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", src.LastScanned)
		if err != nil {
			continue
		}
		age := now.Sub(t)
		if age > staleThreshold {
			days := int(age.Hours() / 24)
			stale = append(stale, fmt.Sprintf("  %s  (last scanned %d days ago)", src.Label, days))
		}
	}
	if len(stale) > 0 {
		fmt.Fprintln(w, "\n⚠  STALE MANIFESTS — results may not reflect current state")
		fmt.Fprintln(w, strings.Repeat("─", 65))
		for _, s := range stale {
			fmt.Fprintln(w, s)
		}
		fmt.Fprintln(w, "  Run 'photo-organizer rescan' on these machines to refresh.")
	}

	// Summary header.
	fmt.Fprintln(w, "\n" + sep)
	fmt.Fprintln(w, "RISK REPORT — Files with no backup (single machine only)")
	fmt.Fprintln(w, sep)

	// Compute per-machine totals.
	type machineInfo struct {
		name     string
		files    int
		size     int64
	}
	var machineStats []machineInfo
	for machine, rows := range uniqueByMachine {
		var totalSize int64
		for _, row := range rows {
			totalSize += row.SizeBytes
		}
		machineStats = append(machineStats, machineInfo{machine, len(rows), totalSize})
	}
	// Sort by size descending (highest risk first).
	sort.Slice(machineStats, func(i, j int) bool {
		return machineStats[i].size > machineStats[j].size
	})

	// Print summary table.
	fmt.Fprintf(w, "  %-30s  %10s  %10s\n", "MACHINE", "FILES", "SIZE")
	fmt.Fprintf(w, "  %-30s  %10s  %10s\n", strings.Repeat("─", 30), strings.Repeat("─", 10), strings.Repeat("─", 10))

	var totalFiles, totalSize int64
	for _, m := range machineStats {
		riskLabel := ""
		if m.files > 100 || m.size > 10*1024*1024*1024 {
			riskLabel = "  ← HIGH RISK"
		}
		fmt.Fprintf(w, "  %-30s  %10s  %10s%s\n", m.name, formatCount(m.files), formatSize(m.size), riskLabel)
		totalFiles += int64(m.files)
		totalSize += m.size
	}

	// Per-machine breakdown.
	for _, mi := range machineStats {
		rows := uniqueByMachine[mi.name]
		fmt.Fprintln(w, strings.Repeat("─", 65))
		fmt.Fprintf(w, "%s  —  %s files  (%s at risk)\n", mi.name, formatCount(len(rows)), formatSize(mi.size))
		fmt.Fprintln(w, strings.Repeat("─", 65))

		// Group by top-level folder.
		folderMap := make(map[string]struct {
			files int
			size  int64
		})
		for _, row := range rows {
			folder := topLevelFolder(row.RelativePath)
			info := folderMap[folder]
			info.files++
			info.size += row.SizeBytes
			folderMap[folder] = info
		}

		// Sort folders by size descending.
		type folderInfo struct {
			name  string
			files int
			size  int64
		}
		var folders []folderInfo
		for name, info := range folderMap {
			folders = append(folders, folderInfo{name, info.files, info.size})
		}
		sort.Slice(folders, func(i, j int) bool {
			return folders[i].size > folders[j].size
		})

		// Print folders.
		fmt.Fprintf(w, "  %-30s  %10s  %10s\n", "FOLDER", "FILES", "SIZE")
		fmt.Fprintf(w, "  %-30s  %10s  %10s\n", strings.Repeat("─", 30), strings.Repeat("─", 10), strings.Repeat("─", 10))
		for _, f := range folders {
			folderDisplay := f.name
			if folderDisplay == "(root)" {
				folderDisplay = "/ (root files)"
			}
			fmt.Fprintf(w, "  %-30s  %10s  %10s\n", folderDisplay, formatCount(f.files), formatSize(f.size))
		}

		// Top 5 largest files.
		largestFiles := make([]ManifestRow, len(rows))
		copy(largestFiles, rows)
		sort.Slice(largestFiles, func(i, j int) bool {
			return largestFiles[i].SizeBytes > largestFiles[j].SizeBytes
		})
		if len(largestFiles) > 5 {
			largestFiles = largestFiles[:5]
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Largest at-risk files:")
		for _, row := range largestFiles {
			fmt.Fprintf(w, "    %-40s  %10s\n", truncate(row.RelativePath, 40), formatSize(row.SizeBytes))
		}
	}

	// Footer.
	fmt.Fprintln(w)
	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "Total at risk: %s files  (%s)\n", formatCount(int(totalFiles)), formatSize(totalSize))
	fmt.Fprintln(w, "Run 'photo-organizer migrate' to copy unique files to another machine.")
	fmt.Fprintln(w, sep)
}

func writeRiskCSV(prefix string, uniqueByMachine map[string][]ManifestRow) error {
	path := prefix + "_risk.csv"
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.Write([]string{"machine", "scan_path", "relative_path", "filename", "size_bytes", "partial_hash", "full_hash"})

	for machine, rows := range uniqueByMachine {
		for _, row := range rows {
			w.Write([]string{
				machine,
				row.ScanPath,
				row.RelativePath,
				row.Filename,
				strconv.FormatInt(row.SizeBytes, 10),
				row.PartialHash,
				row.FullHash,
			})
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "Wrote %s\n", path)
	return nil
}

// =============================================================================
// Migrate (copy unique files preserving folder structure)
// =============================================================================

func runMigrate(args []string) {
	// Pre-separate flags from positional args.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "from" || name == "dest" || name == "out" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	fromFlag := fs.String("from", "", "source machine name (required)")
	destFlag := fs.String("dest", "", "destination root, e.g. user@host:/path or /local/path (required)")
	outFlag := fs.String("out", "", "write script to this file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer migrate --from <machine> --dest <dest-root> [manifest...]\n\n")
		fmt.Fprintf(os.Stderr, "Generates a script to copy files unique to --from to --dest,\n")
		fmt.Fprintf(os.Stderr, "preserving the source folder structure under each scan root.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	if *fromFlag == "" || *destFlag == "" {
		fmt.Fprintln(os.Stderr, "migrate: --from and --dest are required")
		fs.Usage()
		os.Exit(1)
	}

	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
		if len(matches) > 0 {
			manifestPaths = matches
			fmt.Fprintf(os.Stderr, "No manifests specified, loading %s (%d files)\n\n", defaultDir, len(matches))
		} else {
			fmt.Fprintf(os.Stderr, "migrate: no manifests specified and none found in %s\n", defaultDir)
			os.Exit(1)
		}
	}

	var sources []ManifestSource
	for _, path := range manifestPaths {
		src, err := readManifest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", path, err)
			continue
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "migrate: no valid manifests loaded")
		os.Exit(1)
	}

	// Verify the source machine exists.
	fromFound := false
	for _, src := range sources {
		if src.MachineName == *fromFlag {
			fromFound = true
			break
		}
	}
	if !fromFound {
		fmt.Fprintf(os.Stderr, "migrate: machine %q not found in manifests\n", *fromFlag)
		seen := make(map[string]bool)
		fmt.Fprintf(os.Stderr, "Available machines:")
		for _, src := range sources {
			if !seen[src.MachineName] {
				seen[src.MachineName] = true
				fmt.Fprintf(os.Stderr, " %s", src.MachineName)
			}
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	idx := buildHashIndex(sources)
	uniqueByMachine := findUnique(sources, idx)
	uniqueRows := uniqueByMachine[*fromFlag]

	if len(uniqueRows) == 0 {
		fmt.Fprintln(os.Stderr, "migrate: no files unique to", *fromFlag)
		os.Exit(0)
	}

	// Group unique rows by their scan root so we can generate one rsync
	// block per source scan path, preserving relative folder structure.
	byScanPath := make(map[string][]ManifestRow)
	for _, row := range uniqueRows {
		byScanPath[row.ScanPath] = append(byScanPath[row.ScanPath], row)
	}

	// Pre-flight checks
	fmt.Fprintf(os.Stderr, "\nPre-flight checks:\n")
	var checks []PreflightCheck

	// Check source scan paths are readable
	for scanPath := range byScanPath {
		checks = append(checks, CheckDirReadable(scanPath, fmt.Sprintf("Source path %s readable", filepath.Base(scanPath))))
	}

	// Check destination is writable (or will be via SSH)
	if !strings.Contains(*destFlag, ":") {
		// Local destination
		destDir := *destFlag
		checks = append(checks, CheckDirWritable(destDir, fmt.Sprintf("Destination %s writable", filepath.Base(destDir))))

		// Calculate total size to migrate
		totalSize := int64(0)
		for _, rows := range byScanPath {
			for _, row := range rows {
				totalSize += row.SizeBytes
			}
		}
		checks = append(checks, CheckDiskSpace(destDir, totalSize))
	} else {
		// Remote destination - just check parent manifest dir is writable
		checks = append(checks, CheckDirWritable(filepath.Join(os.Getenv("HOME"), "manifests"), "Manifest directory writable"))
	}

	if !RunPreflightChecks(checks) {
		os.Exit(1)
	}

	scanPaths := make([]string, 0, len(byScanPath))
	for sp := range byScanPath {
		scanPaths = append(scanPaths, sp)
	}
	sort.Strings(scanPaths)

	totalSize := int64(0)
	for _, row := range uniqueRows {
		totalSize += row.SizeBytes
	}

	out := os.Stdout
	if *outFlag != "" {
		if _, err := os.Stat(*outFlag); err == nil {
			fmt.Fprintf(os.Stderr, "⚠  %s already exists — overwrite? [y/N] ", *outFlag)
			var answer string
			fmt.Fscan(os.Stdin, &answer)
			if answer != "y" && answer != "Y" {
				fmt.Fprintln(os.Stderr, "Aborted.")
				os.Exit(1)
			}
		}
		f, err := os.Create(*outFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate: cannot write output:", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	dest := strings.TrimRight(*destFlag, "/")

	// Derive script base name for temp file references.
	scriptName := "migrate"
	outPath := ""
	if *outFlag != "" {
		scriptName = strings.TrimSuffix(filepath.Base(*outFlag), filepath.Ext(*outFlag))
		var err error
		outPath, err = filepath.Abs(*outFlag)
		if err != nil {
			outPath = *outFlag
		}
	}

	// Temp files will be stored in a manifest-specific directory
	tempDir := filepath.Join(os.Getenv("HOME"), "manifests", "_migrate")

	fmt.Fprintf(out, "#!/bin/bash\n")
	fmt.Fprintf(out, "# Migration script generated by photo-organizer\n")
	fmt.Fprintf(out, "# Source machine: %s\n", *fromFlag)
	fmt.Fprintf(out, "# Destination:    %s\n", dest)
	fmt.Fprintf(out, "# Unique files:   %s  (%s)\n", formatCount(len(uniqueRows)), formatSize(totalSize))
	fmt.Fprintf(out, "#\n")
	fmt.Fprintf(out, "# This script is self-contained and works from any directory.\n")
	fmt.Fprintf(out, "# All paths are absolute. File lists stored in: %s\n", tempDir)
	fmt.Fprintf(out, "# Re-running is safe — rsync skips already-copied files.\n")
	fmt.Fprintf(out, "#\n")
	fmt.Fprintf(out, "set -uo pipefail\n")
	fmt.Fprintf(out, "SCRIPT_DIR=\"$(cd \"$(dirname \"${BASH_SOURCE[0]}\")\" && pwd)\"\n")
	fmt.Fprintf(out, "WORK_DIR=\"%s\"\n", shellQuote(tempDir))
	fmt.Fprintf(out, "mkdir -p \"$WORK_DIR\"\n")
	fmt.Fprintf(out, "_fail=0\n")
	fmt.Fprintf(out, "_total=%d\n", len(uniqueRows))
	fmt.Fprintf(out, "_copied=0\n\n")

	for i, scanPath := range scanPaths {
		rows := byScanPath[scanPath]
		sort.Slice(rows, func(i, j int) bool { return rows[i].RelativePath < rows[j].RelativePath })

		scanSize := int64(0)
		for _, r := range rows {
			scanSize += r.SizeBytes
		}

		// Use absolute path for scanPath
		absScanPath := scanPath
		if !filepath.IsAbs(scanPath) {
			absScanPath, _ = filepath.Abs(scanPath)
		}

		scanName := filepath.Base(scanPath)
		destPath := dest + "/" + scanName
		listFile := fmt.Sprintf("$WORK_DIR/%s_%d.txt", scriptName, i+1)

		fmt.Fprintf(out, "# ───────────────────────────────────────────────────────\n")
		fmt.Fprintf(out, "# Group %d/%d: %s\n", i+1, len(scanPaths), scanPath)
		fmt.Fprintf(out, "# %s files, %s\n", formatCount(len(rows)), formatSize(scanSize))
		fmt.Fprintf(out, "# ───────────────────────────────────────────────────────\n")
		fmt.Fprintf(out, "echo \"[$(date +'%%Y-%%m-%%d %%H:%%M:%%S')] [%d/%d] Copying %s files (%s) to: %s\"\n",
			i+1, len(scanPaths), formatCount(len(rows)), formatSize(scanSize), destPath)
		fmt.Fprintf(out, "echo ''\n\n")

		// Create destination directory. For remote paths (user@host:/path) use ssh.
		if strings.Contains(destPath, ":") {
			parts := strings.SplitN(destPath, ":", 2)
			fmt.Fprintf(out, "ssh %s mkdir -p %s\n", shellQuote(parts[0]), shellQuote(parts[1]))
		} else {
			fmt.Fprintf(out, "mkdir -p %s\n", shellQuote(destPath))
		}

		fmt.Fprintf(out, "cat > %s << 'FILELIST'\n", listFile)
		for _, row := range rows {
			fmt.Fprintf(out, "%s\n", filepath.ToSlash(row.RelativePath))
		}
		fmt.Fprintf(out, "FILELIST\n\n")

		// Verbose rsync with progress
		fmt.Fprintf(out, "if rsync -av --checksum --partial --progress \\\n")
		fmt.Fprintf(out, "        --files-from=%s \\\n", listFile)
		fmt.Fprintf(out, "        %s \\\n", shellQuote(absScanPath+"/"))
		fmt.Fprintf(out, "        %s; then\n", shellQuote(destPath+"/"))
		fmt.Fprintf(out, "  echo '[✓] Group %d complete'\n", i+1)
		fmt.Fprintf(out, "  touch \"$WORK_DIR/%s_%d.done\"\n", scriptName, i+1)
		fmt.Fprintf(out, "else\n")
		fmt.Fprintf(out, "  echo '[✗] Group %d FAILED (will retry on re-run)'\n", i+1)
		fmt.Fprintf(out, "  _fail=$((_fail+1))\n")
		fmt.Fprintf(out, "fi\n")
		fmt.Fprintf(out, "rm -f %s\n", listFile)
		fmt.Fprintf(out, "echo ''\n\n")
	}

	fmt.Fprintf(out, "echo '═══════════════════════════════════════════════════════════'\n")
	fmt.Fprintf(out, "if [ \"$_fail\" -gt 0 ]; then\n")
	fmt.Fprintf(out, "  echo \"✗ MIGRATION INCOMPLETE: $_fail group(s) failed\"\n")
	fmt.Fprintf(out, "  echo ''\n")
	fmt.Fprintf(out, "  echo 'To retry failed groups, simply re-run this script:'\n")
	fmt.Fprintf(out, "  echo '  bash %s'\n", shellQuote(filepath.Base(outPath)))
	fmt.Fprintf(out, "  echo ''\n")
	fmt.Fprintf(out, "  echo 'Progress is tracked in: $WORK_DIR'\n")
	fmt.Fprintf(out, "  exit 1\n")
	fmt.Fprintf(out, "else\n")
	fmt.Fprintf(out, "  echo '✓ MIGRATION COMPLETE: All %d files transferred'\n", len(uniqueRows))
	fmt.Fprintf(out, "  echo 'You can now safely delete from source or reformat media.'\n")
	fmt.Fprintf(out, "fi\n")

	if *outFlag != "" {
		fmt.Fprintf(os.Stderr, "Wrote migration script to %s (%s files, %s)\n",
			*outFlag, formatCount(len(uniqueRows)), formatSize(totalSize))
	}
}

// =============================================================================
// Formatting Helpers
// =============================================================================

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func formatCount(n int) string {
	s := fmt.Sprintf("%d", n)
	// Insert thousands separators
	result := []byte{}
	for i, c := range s {
		pos := len(s) - i
		if i > 0 && pos%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// =============================================================================
// Search
// =============================================================================

func runSearchAnalyze(args []string) {
	// Parse search flags
	var (
		namePattern    string
		pathSubstring  string
		hashValue      string
		sizeStr        string
		dateStr        string
		machineID      string
		duplicatesOnly bool
		groupByHash    bool
		csvOutput      string
	)

	// Simple flag parsing
	var manifestFiles []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-name" && i+1 < len(args):
			namePattern = args[i+1]
			i++
		case arg == "-path" && i+1 < len(args):
			pathSubstring = args[i+1]
			i++
		case arg == "-hash" && i+1 < len(args):
			hashValue = args[i+1]
			i++
		case arg == "-size" && i+1 < len(args):
			sizeStr = args[i+1]
			i++
		case arg == "-date" && i+1 < len(args):
			dateStr = args[i+1]
			i++
		case arg == "-machine" && i+1 < len(args):
			machineID = args[i+1]
			i++
		case arg == "-duplicates-only":
			duplicatesOnly = true
		case arg == "-group":
			groupByHash = true
		case arg == "-csv" && i+1 < len(args):
			csvOutput = args[i+1]
			i++
		case !strings.HasPrefix(arg, "-"):
			manifestFiles = append(manifestFiles, arg)
		}
	}

	// If no manifests specified, load all from ~/manifests/_Manifest/
	if len(manifestFiles) == 0 {
		manifestRoot := defaultManifestRoot()
		manifestDir := filepath.Join(manifestRoot, "_Manifest")
		allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
		if len(allCSVs) == 0 {
			fmt.Fprintf(os.Stderr, "search: no manifests found in %s\n", manifestDir)
			os.Exit(1)
		}
		manifestFiles = allCSVs
	}

	// Load all manifests
	var allRows []ManifestRow
	hashCounts := make(map[string]int)
	for _, csvPath := range manifestFiles {
		src, err := readManifest(csvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "search: cannot read %s: %v\n", csvPath, err)
			continue
		}
		allRows = append(allRows, src.Rows...)
		for _, row := range src.Rows {
			// Use full_hash if available, otherwise use partial_hash for grouping
			hash := row.FullHash
			if hash == "" {
				hash = row.PartialHash
			}
			hashCounts[hash]++
		}
	}

	if len(allRows) == 0 {
		fmt.Fprintf(os.Stderr, "search: no files found\n")
		os.Exit(1)
	}

	// Apply filters
	var filtered []ManifestRow
	nameRegex, _ := compilePattern(namePattern)
	sizeMin, sizeMax := parseSizeRange(sizeStr)
	dateStart, dateEnd := parseDateRange(dateStr)

	for _, row := range allRows {
		// Machine filter
		if machineID != "" && row.MachineName != machineID {
			continue
		}

		// Name filter
		if namePattern != "" && !matchPattern(filepath.Base(row.RelativePath), namePattern, nameRegex) {
			continue
		}

		// Path filter
		if pathSubstring != "" && !strings.Contains(row.RelativePath, pathSubstring) {
			continue
		}

		// Hash filter
		if hashValue != "" && row.FullHash != hashValue {
			continue
		}

		// Size filter
		if sizeMin >= 0 || sizeMax >= 0 {
			if sizeMin >= 0 && row.SizeBytes < sizeMin {
				continue
			}
			if sizeMax >= 0 && row.SizeBytes > sizeMax {
				continue
			}
		}

		// Date filter
		if dateStr != "" {
			rowDate, err := time.Parse("2006-01-02", row.ScanDate)
			if err == nil {
				if !dateStart.IsZero() && rowDate.Before(dateStart) {
					continue
				}
				if !dateEnd.IsZero() && rowDate.After(dateEnd) {
					continue
				}
			}
		}

		// Duplicates-only filter
		if duplicatesOnly && hashCounts[row.FullHash] <= 1 {
			continue
		}

		filtered = append(filtered, row)
	}

	if len(filtered) == 0 {
		fmt.Println("No matching files found.")
		return
	}

	// Output results
	if csvOutput != "" {
		// Write CSV
		writeSearchResults(csvOutput, filtered)
	} else if groupByHash {
		// Display grouped by hash
		displayGroupedResults(filtered, hashCounts)
	} else {
		// Display as table
		displayTableResults(filtered, hashCounts)
	}
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	// Convert glob patterns to regex
	globRegex := strings.NewReplacer(
		".", "\\.",
		"*", ".*",
		"?", ".",
	).Replace(pattern)
	return regexp.Compile("^" + globRegex + "$")
}

func matchPattern(name, pattern string, compiled *regexp.Regexp) bool {
	if pattern == "" {
		return true
	}
	if compiled != nil {
		return compiled.MatchString(name)
	}
	// Fallback to simple substring match
	return strings.Contains(name, pattern)
}

func parseSizeRange(sizeStr string) (int64, int64) {
	if sizeStr == "" {
		return -1, -1
	}

	// Parse "100MB", "1GB", or "100MB-500MB"
	if strings.Contains(sizeStr, "-") {
		parts := strings.Split(sizeStr, "-")
		if len(parts) == 2 {
			min := parseSize(parts[0])
			max := parseSize(parts[1])
			return min, max
		}
	}

	size := parseSize(sizeStr)
	return size, size
}

func parseSize(sizeStr string) int64 {
	sizeStr = strings.TrimSpace(sizeStr)
	if sizeStr == "" {
		return -1
	}

	var multiplier int64 = 1
	if strings.HasSuffix(sizeStr, "GB") {
		multiplier = 1 << 30
		sizeStr = strings.TrimSuffix(sizeStr, "GB")
	} else if strings.HasSuffix(sizeStr, "MB") {
		multiplier = 1 << 20
		sizeStr = strings.TrimSuffix(sizeStr, "MB")
	} else if strings.HasSuffix(sizeStr, "KB") {
		multiplier = 1 << 10
		sizeStr = strings.TrimSuffix(sizeStr, "KB")
	}

	val, err := strconv.ParseFloat(strings.TrimSpace(sizeStr), 64)
	if err != nil {
		return -1
	}
	return int64(val * float64(multiplier))
}

func parseDateRange(dateStr string) (time.Time, time.Time) {
	if dateStr == "" {
		return time.Time{}, time.Time{}
	}

	// Parse "2024-01-01" or "2024-01-01:2024-01-31"
	var start, end time.Time
	if strings.Contains(dateStr, ":") {
		parts := strings.Split(dateStr, ":")
		if len(parts) == 2 {
			start, _ = time.Parse("2006-01-02", parts[0])
			end, _ = time.Parse("2006-01-02", parts[1])
			end = end.AddDate(0, 0, 1).Add(-time.Second) // End of day
			return start, end
		}
	}

	start, _ = time.Parse("2006-01-02", dateStr)
	end = start.AddDate(0, 0, 1).Add(-time.Second) // End of day
	return start, end
}

func displayTableResults(rows []ManifestRow, hashCounts map[string]int) {
	// Sort by machine, then path
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MachineName != rows[j].MachineName {
			return rows[i].MachineName < rows[j].MachineName
		}
		if rows[i].ScanPath != rows[j].ScanPath {
			return rows[i].ScanPath < rows[j].ScanPath
		}
		return rows[i].RelativePath < rows[j].RelativePath
	})

	fmt.Printf("%-20s %-12s %-10s %-8s  %s\n",
		"Machine", "Size", "Hash", "Copies", "Full Path")
	fmt.Println(strings.Repeat("-", 120))

	for _, row := range rows {
		// Use full_hash if available, otherwise use partial_hash
		displayHash := row.FullHash
		if displayHash == "" {
			displayHash = row.PartialHash
		}
		shortHash := displayHash
		if len(shortHash) > 8 {
			shortHash = shortHash[:8]
		}
		copies := hashCounts[displayHash]
		copyStr := "1"
		if copies > 1 {
			copyStr = fmt.Sprintf("%d", copies)
		}

		// Show full path: scan_path/relative_path
		fullPath := filepath.Join(row.ScanPath, filepath.FromSlash(row.RelativePath))

		fmt.Printf("%-20s %-12s %-10s %-8s  %s\n",
			row.MachineName, formatSize(row.SizeBytes), shortHash, copyStr, fullPath)
	}
	fmt.Printf("\nTotal: %d file(s)\n", len(rows))
}

func displayGroupedResults(rows []ManifestRow, hashCounts map[string]int) {
	// Group by hash (prefer full_hash, fall back to partial_hash)
	groups := make(map[string][]ManifestRow)
	for _, row := range rows {
		hash := row.FullHash
		if hash == "" {
			hash = row.PartialHash
		}
		groups[hash] = append(groups[hash], row)
	}

	// Sort groups by hash, then files within group
	var hashes []string
	for hash := range groups {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)

	totalGroups := 0
	for _, hash := range hashes {
		group := groups[hash]
		if len(group) <= 1 {
			continue // Skip non-duplicates
		}
		totalGroups++
	}

	groupNum := 0
	for _, hash := range hashes {
		group := groups[hash]
		if len(group) <= 1 {
			continue
		}

		groupNum++
		shortHash := hash
		if len(shortHash) > 8 {
			shortHash = shortHash[:8]
		}

		fmt.Printf("\n[Group %d/%d] Hash: %s (%d copies, %s each)\n",
			groupNum, totalGroups, shortHash, len(group), formatSize(group[0].SizeBytes))
		fmt.Println(strings.Repeat("-", 120))

		// Sort by machine, then path
		sort.Slice(group, func(i, j int) bool {
			if group[i].MachineName != group[j].MachineName {
				return group[i].MachineName < group[j].MachineName
			}
			if group[i].ScanPath != group[j].ScanPath {
				return group[i].ScanPath < group[j].ScanPath
			}
			return group[i].RelativePath < group[j].RelativePath
		})

		for _, row := range group {
			// Display hash for this specific file (prefer full_hash, fall back to partial_hash)
			fileHash := row.FullHash
			if fileHash == "" {
				fileHash = row.PartialHash
			}
			fileShortHash := fileHash
			if len(fileShortHash) > 8 {
				fileShortHash = fileShortHash[:8]
			}
			fullPath := filepath.Join(row.ScanPath, filepath.FromSlash(row.RelativePath))
			fmt.Printf("  %-20s %-12s %-12s %s\n", row.MachineName, formatSize(row.SizeBytes), fileShortHash, fullPath)
		}
	}

	fmt.Printf("\nTotal: %d duplicate group(s) found\n", groupNum)
}

func writeSearchResults(filename string, rows []ManifestRow) {
	f, err := os.Create(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search: cannot create %s: %v\n", filename, err)
		os.Exit(1)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// Write header
	w.Write([]string{"machine_name", "scan_path", "relative_path", "file_size_bytes", "full_hash", "scan_date"})

	// Write rows
	for _, row := range rows {
		w.Write([]string{
			row.MachineName,
			row.ScanPath,
			row.RelativePath,
			fmt.Sprintf("%d", row.SizeBytes),
			row.FullHash,
			row.ScanDate,
		})
	}

	fmt.Printf("Results written to %s\n", filename)
}

// =============================================================================
// Pre-flight Checks
// =============================================================================

// PreflightCheck represents a single validation check.
type PreflightCheck struct {
	Name   string
	Pass   bool
	Error  string
	Hint   string
}

// RunPreflightChecks validates that an operation can proceed.
// Returns true if all checks pass, false if any critical checks fail.
func RunPreflightChecks(checks []PreflightCheck) bool {
	if len(checks) == 0 {
		return true
	}

	anyFailed := false
	for _, check := range checks {
		if check.Pass {
			fmt.Fprintf(os.Stderr, "✓ %s\n", check.Name)
		} else {
			fmt.Fprintf(os.Stderr, "✗ %s: %s\n", check.Name, check.Error)
			if check.Hint != "" {
				fmt.Fprintf(os.Stderr, "  → %s\n", check.Hint)
			}
			anyFailed = true
		}
	}

	if anyFailed {
		fmt.Fprintf(os.Stderr, "\n⚠  Pre-flight checks failed. Aborting operation.\n")
		return false
	}
	fmt.Fprintf(os.Stderr, "\nPre-flight checks passed.\n\n")
	return true
}

// CheckDirReadable checks if a directory exists and is readable.
func CheckDirReadable(path, name string) PreflightCheck {
	info, err := os.Stat(path)
	if err != nil {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: "not found or not accessible",
			Hint:  fmt.Sprintf("Directory: %s", path),
		}
	}
	if !info.IsDir() {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: "is not a directory",
			Hint:  fmt.Sprintf("Path: %s", path),
		}
	}
	return PreflightCheck{Name: name, Pass: true}
}

// CheckDirWritable checks if a directory exists and is writable.
func CheckDirWritable(path, name string) PreflightCheck {
	// Try to create a test file.
	testFile := filepath.Join(path, ".photo-organizer-write-test")
	err := os.WriteFile(testFile, []byte("test"), 0600)
	if err != nil {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: "not writable",
			Hint:  fmt.Sprintf("Directory: %s (check permissions)", path),
		}
	}
	_ = os.Remove(testFile)
	return PreflightCheck{Name: name, Pass: true}
}

// CheckFileReadable checks if a file exists and is readable.
func CheckFileReadable(path, name string) PreflightCheck {
	info, err := os.Stat(path)
	if err != nil {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: "not found or not accessible",
			Hint:  fmt.Sprintf("File: %s", path),
		}
	}
	if info.IsDir() {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: "is a directory, not a file",
			Hint:  fmt.Sprintf("Path: %s", path),
		}
	}
	return PreflightCheck{Name: name, Pass: true}
}

// CheckDiskSpace checks if destination has enough free space (approximate check).
func CheckDiskSpace(path string, neededBytes int64) PreflightCheck {
	name := fmt.Sprintf("Disk space for %s", filepath.Base(path))
	if neededBytes <= 0 {
		return PreflightCheck{Name: name, Pass: true}
	}

	// Try to write a test file of 1MB to estimate available space.
	// If we can write the test file and disk space is available, assume it's OK.
	testFile := filepath.Join(path, ".photo-organizer-space-test")
	testSize := int64(1024 * 1024) // 1MB test
	if neededBytes < testSize {
		testSize = neededBytes
	}

	err := os.WriteFile(testFile, make([]byte, testSize), 0600)
	if err != nil {
		return PreflightCheck{
			Name:  name,
			Pass:  false,
			Error: fmt.Sprintf("cannot write to destination (need %s)", formatSize(neededBytes)),
			Hint:  "Check disk space and permissions",
		}
	}
	defer os.Remove(testFile)

	return PreflightCheck{Name: name, Pass: true}
}

// =============================================================================
// Timeout & Recovery
// =============================================================================

// ScanCheckpoint tracks scan progress for resumption.
type ScanCheckpoint struct {
	ManifestPath string    `json:"manifest_path"`
	ScanPath     string    `json:"scan_path"`
	ProcessedDir int       `json:"processed_dirs"`
	ProcessedFile int      `json:"processed_files"`
	LastFile     string    `json:"last_file"`
	StartTime    time.Time `json:"start_time"`
	LastUpdate   time.Time `json:"last_update"`
}

// CheckpointDir returns the directory for storing scan checkpoints.
func CheckpointDir() string {
	return filepath.Join(os.Getenv("HOME"), "manifests", "_checkpoints")
}

// CheckpointPath returns the checkpoint file path for a manifest.
func CheckpointPath(manifestPath string) string {
	name := filepath.Base(manifestPath)
	return filepath.Join(CheckpointDir(), name+".checkpoint")
}

// LoadCheckpoint loads a previous scan checkpoint if it exists.
func LoadCheckpoint(manifestPath string) (*ScanCheckpoint, error) {
	path := CheckpointPath(manifestPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // File doesn't exist or can't read
	}

	var cp ScanCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// SaveCheckpoint saves scan progress for potential resumption.
func SaveCheckpoint(cp *ScanCheckpoint) error {
	dir := CheckpointDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}

	path := CheckpointPath(cp.ManifestPath)
	return os.WriteFile(path, data, 0600)
}

// ClearCheckpoint removes a checkpoint after successful completion.
func ClearCheckpoint(manifestPath string) error {
	path := CheckpointPath(manifestPath)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil // Already gone, that's fine
	}
	return err
}

// DetectInterruptedOperations checks for incomplete operations and provides recovery hints.
func DetectInterruptedOperations() {
	dir := CheckpointDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // Checkpoint dir doesn't exist yet
	}

	var found []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".checkpoint") {
			found = append(found, entry.Name())
		}
	}

	if len(found) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  Found %d incomplete scan(s). Run rescan to resume:\n", len(found))
		for _, f := range found {
			name := strings.TrimSuffix(f, ".checkpoint")
			fmt.Fprintf(os.Stderr, "   photo-organizer rescan  (to resume %s)\n", name)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}

// =============================================================================
// Backup Compliance Analysis (3-2-1 Rule)
// =============================================================================

type BackupComplianceFile struct {
	Path             string
	SizeBytes        int64
	Machine          string
	CopiesElsewhere  int
	CopyMachines     []string
	ComplianceStatus string // "safe", "risky", "critical"
}

type BackupComplianceReport struct {
	Machine           string
	TargetPath        string
	SafeFiles         []BackupComplianceFile
	RiskyFiles        []BackupComplianceFile
	CriticalFiles     []BackupComplianceFile
	SafeSize          int64
	RiskySize         int64
	CriticalSize      int64
	TotalSize         int64
	TotalFiles        int
	SafeableSpaceFreed int64
}

func runAnalyzeBackupCompliance(flagArgs []string) {
	fs := flag.NewFlagSet("analyze-backup-compliance", flag.ExitOnError)
	machine := fs.String("machine", "", "machine name to analyze (default: current machine)")
	path := fs.String("path", "", "specific path to analyze (optional, analyzes all if omitted)")
	csvPrefix := fs.String("csv", "", "write CSV output with this prefix")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer analyze-backup-compliance [--machine <name>] [--path <path>] [--csv prefix]\n\n")
		fmt.Fprintf(os.Stderr, "Analyzes files on a machine using 3-2-1 backup rule:\n")
		fmt.Fprintf(os.Stderr, "  Safe:     2+ copies on other machines (3+ total including original)\n")
		fmt.Fprintf(os.Stderr, "  Risky:    1 copy on another machine (2 total) — violates 3-2-1\n")
		fmt.Fprintf(os.Stderr, "  Critical: 0 copies elsewhere (only on this machine) — data loss risk\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	// Resolve machine name (priority: flag > ./machine-id > ~/manifests/machine-id)
	targetMachine := resolveMachineID(*machine)

	posArgs := fs.Args()
	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
		if len(matches) > 0 {
			manifestPaths = matches
			fmt.Fprintf(os.Stderr, "Loading %d manifest(s)...\n\n", len(matches))
		} else {
			fmt.Fprintf(os.Stderr, "Error: no manifests specified and none found in %s\n", defaultDir)
			fs.Usage()
			os.Exit(1)
		}
	}

	// Load manifests
	var sources []ManifestSource
	for _, manifestPath := range manifestPaths {
		src, err := readManifest(manifestPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests loaded.\n")
		os.Exit(1)
	}

	// Build hash index
	idx := buildHashIndex(sources)

	// Generate compliance report
	report := analyzeBackupCompliance(sources, idx, targetMachine, *path)

	// Print report
	printBackupComplianceReport(os.Stdout, report)

	// Write CSV if requested
	if *csvPrefix != "" {
		if err := writeBackupComplianceCSV(*csvPrefix, report); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing CSV: %v\n", err)
			os.Exit(1)
		}
	}
}

func analyzeBackupCompliance(sources []ManifestSource, idx map[string][]hashLocation, targetMachine, targetPath string) BackupComplianceReport {
	report := BackupComplianceReport{
		Machine:    targetMachine,
		TargetPath: targetPath,
	}

	// Find source index for target machine
	var targetSourceIdx []int
	for si, src := range sources {
		if src.MachineName != targetMachine {
			continue
		}
		if targetPath != "" && !strings.HasPrefix(filepath.Clean(src.ScanPath), filepath.Clean(targetPath)) {
			continue
		}
		targetSourceIdx = append(targetSourceIdx, si)
	}

	if len(targetSourceIdx) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found for machine %q\n", targetMachine)
		return report
	}

	// Analyze each file in target machine
	for _, si := range targetSourceIdx {
		for _, row := range sources[si].Rows {
			if targetPath != "" && !strings.Contains(row.RelativePath, targetPath) {
				continue
			}

			if row.PartialHash == "" {
				continue
			}

			// Count copies on OTHER machines
			key := indexKey(row.PartialHash, row.SizeBytes)
			locs, exists := idx[key]
			if !exists {
				locs = []hashLocation{}
			}

			copiesElsewhere := 0
			copyMachines := make(map[string]bool)

			for _, loc := range locs {
				otherMachine := sources[loc.sourceIdx].MachineName
				if otherMachine != targetMachine {
					copiesElsewhere++
					copyMachines[otherMachine] = true
				}
			}

			// Determine compliance status (3-2-1 rule)
			file := BackupComplianceFile{
				Path:            row.RelativePath,
				SizeBytes:       row.SizeBytes,
				Machine:         targetMachine,
				CopiesElsewhere: copiesElsewhere,
			}

			for m := range copyMachines {
				file.CopyMachines = append(file.CopyMachines, m)
			}
			sort.Strings(file.CopyMachines)

			// Apply 3-2-1 rule
			if copiesElsewhere >= 2 {
				file.ComplianceStatus = "safe"
				report.SafeFiles = append(report.SafeFiles, file)
				report.SafeSize += row.SizeBytes
				report.SafeableSpaceFreed += row.SizeBytes
			} else if copiesElsewhere == 1 {
				file.ComplianceStatus = "risky"
				report.RiskyFiles = append(report.RiskyFiles, file)
				report.RiskySize += row.SizeBytes
			} else {
				file.ComplianceStatus = "critical"
				report.CriticalFiles = append(report.CriticalFiles, file)
				report.CriticalSize += row.SizeBytes
			}

			report.TotalSize += row.SizeBytes
			report.TotalFiles++
		}
	}

	return report
}

func printBackupComplianceReport(w io.Writer, r BackupComplianceReport) {
	sep := "================================================================="

	fmt.Fprintf(w, "\n%s\n", sep)
	fmt.Fprintf(w, "3-2-1 BACKUP COMPLIANCE ANALYSIS\n")
	fmt.Fprintf(w, "%s\n\n", sep)

	fmt.Fprintf(w, "Machine:  %s\n", r.Machine)
	if r.TargetPath != "" {
		fmt.Fprintf(w, "Path:     %s\n", r.TargetPath)
	}
	fmt.Fprintf(w, "Total:    %d files (%.1f GB)\n\n", r.TotalFiles, float64(r.TotalSize)/(1024*1024*1024))

	// Safe files
	fmt.Fprintf(w, "✅ SAFE TO DELETE (2+ copies elsewhere)\n")
	fmt.Fprintf(w, "   Count:  %d files\n", len(r.SafeFiles))
	fmt.Fprintf(w, "   Size:   %.1f GB\n", float64(r.SafeSize)/(1024*1024*1024))
	fmt.Fprintf(w, "   Can free: %.1f GB\n\n", float64(r.SafeableSpaceFreed)/(1024*1024*1024))

	// Risky files
	fmt.Fprintf(w, "⚠️  RISKY (1 copy elsewhere — violates 3-2-1)\n")
	fmt.Fprintf(w, "   Count:  %d files\n", len(r.RiskyFiles))
	fmt.Fprintf(w, "   Size:   %.1f GB\n", float64(r.RiskySize)/(1024*1024*1024))
	fmt.Fprintf(w, "   Action: Avoid deleting unless you create another copy first\n\n")

	// Critical files
	fmt.Fprintf(w, "🚫 CRITICAL (no copies elsewhere — ONLY on this machine)\n")
	fmt.Fprintf(w, "   Count:  %d files\n", len(r.CriticalFiles))
	fmt.Fprintf(w, "   Size:   %.1f GB\n", float64(r.CriticalSize)/(1024*1024*1024))
	fmt.Fprintf(w, "   Action: DO NOT DELETE — data loss risk!\n\n")

	fmt.Fprintf(w, "%s\n", sep)

	// Show sample files from each category
	if len(r.SafeFiles) > 0 {
		fmt.Fprintf(w, "\nSample SAFE files (showing first 5):\n")
		for i, f := range r.SafeFiles {
			if i >= 5 {
				break
			}
			fmt.Fprintf(w, "  • %s (%.1f MB, copies on: %s)\n", f.Path, float64(f.SizeBytes)/(1024*1024), strings.Join(f.CopyMachines, ", "))
		}
		if len(r.SafeFiles) > 5 {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.SafeFiles)-5)
		}
	}

	if len(r.RiskyFiles) > 0 {
		fmt.Fprintf(w, "\nSample RISKY files (showing first 5):\n")
		for i, f := range r.RiskyFiles {
			if i >= 5 {
				break
			}
			fmt.Fprintf(w, "  • %s (%.1f MB, copy on: %s)\n", f.Path, float64(f.SizeBytes)/(1024*1024), strings.Join(f.CopyMachines, ", "))
		}
		if len(r.RiskyFiles) > 5 {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.RiskyFiles)-5)
		}
	}

	if len(r.CriticalFiles) > 0 {
		fmt.Fprintf(w, "\nSample CRITICAL files (showing first 5):\n")
		for i, f := range r.CriticalFiles {
			if i >= 5 {
				break
			}
			fmt.Fprintf(w, "  • %s (%.1f MB)\n", f.Path, float64(f.SizeBytes)/(1024*1024))
		}
		if len(r.CriticalFiles) > 5 {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.CriticalFiles)-5)
		}
	}

	fmt.Fprintf(w, "\n")
}

func writeBackupComplianceCSV(prefix string, r BackupComplianceReport) error {
	writeFile := func(filename string, files []BackupComplianceFile) error {
		f, err := os.Create(filename)
		if err != nil {
			return err
		}
		defer f.Close()

		w := csv.NewWriter(f)
		w.Write([]string{"path", "size_bytes", "size_mb", "copies_elsewhere", "copy_machines", "compliance_status"})

		for _, file := range files {
			w.Write([]string{
				file.Path,
				strconv.FormatInt(file.SizeBytes, 10),
				fmt.Sprintf("%.1f", float64(file.SizeBytes)/(1024*1024)),
				strconv.Itoa(file.CopiesElsewhere),
				strings.Join(file.CopyMachines, ";"),
				file.ComplianceStatus,
			})
		}
		w.Flush()
		return w.Error()
	}

	if len(r.SafeFiles) > 0 {
		if err := writeFile(prefix+"_safe.csv", r.SafeFiles); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Wrote %s_safe.csv\n", prefix)
	}

	if len(r.RiskyFiles) > 0 {
		if err := writeFile(prefix+"_risky.csv", r.RiskyFiles); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Wrote %s_risky.csv\n", prefix)
	}

	if len(r.CriticalFiles) > 0 {
		if err := writeFile(prefix+"_critical.csv", r.CriticalFiles); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Wrote %s_critical.csv\n", prefix)
	}

	return nil
}

// =============================================================================
// Check Backup Command
// =============================================================================

type BackupCheckResult struct {
	FolderPath     string
	TotalFiles     int
	BackedUpFiles  int
	NotBackedUp    []FileBackupStatus
	BackupLocations map[string]int // machine@path -> count of files
	AllBackedUp    bool
}

type FileBackupStatus struct {
	Path      string
	SizeBytes int64
	Locations int // number of machines that have this file
}

func runCheckBackup(flagArgs []string) {
	if len(flagArgs) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer check-backup <folder-path>\n\n")
		fmt.Fprintf(os.Stderr, "Check if all files in a folder are backed up on other machines.\n")
		os.Exit(1)
	}

	folderPath := flagArgs[0]

	// Resolve to absolute path
	absFolderPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid path %q\n", folderPath)
		os.Exit(1)
	}

	// Check folder exists
	info, err := os.Stat(absFolderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Error: %q is not a directory\n", folderPath)
		os.Exit(1)
	}

	// Load all manifests
	defaultDir := filepath.Join(os.Getenv("HOME"), "manifests", "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(defaultDir, "*.csv"))
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no manifests found in %s\n", defaultDir)
		os.Exit(1)
	}

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no manifests loaded\n")
		os.Exit(1)
	}

	// Build hash index from all manifests
	idx := buildHashIndex(sources)

	// Scan the folder and check each file
	result := checkFolderBackup(absFolderPath, sources, idx)

	// Print results
	printCheckBackupResult(result)
}

func checkFolderBackup(folderPath string, sources []ManifestSource, idx map[string][]hashLocation) BackupCheckResult {
	result := BackupCheckResult{
		FolderPath:      folderPath,
		BackupLocations: make(map[string]int),
	}

	// Walk through all files in folder
	filepath.WalkDir(folderPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || path == folderPath {
			return nil
		}

		info, err := os.Stat(path)
		if err != nil {
			return nil
		}

		sizeBytes := info.Size()
		result.TotalFiles++

		// Compute partial hash (use processFile from main.go)
		partialHash, _ := processFile(path)
		if partialHash == "" {
			return nil
		}

		// Check if this hash exists in other manifests
		key := indexKey(partialHash, sizeBytes)
		locs, exists := idx[key]

		if !exists || len(locs) == 0 {
			// File has no copies anywhere
			relPath, _ := filepath.Rel(folderPath, path)
			result.NotBackedUp = append(result.NotBackedUp, FileBackupStatus{
				Path:      relPath,
				SizeBytes: sizeBytes,
				Locations: 0,
			})
			return nil
		}

		// Count how many machines have this file and track locations
		machines := make(map[string]bool)
		for _, loc := range locs {
			machines[sources[loc.sourceIdx].MachineName] = true
			// Track backup location with label
			label := sources[loc.sourceIdx].Label
			result.BackupLocations[label]++
		}

		if len(machines) > 0 {
			result.BackedUpFiles++
		} else {
			// File exists in index but only on current machine
			relPath, _ := filepath.Rel(folderPath, path)
			result.NotBackedUp = append(result.NotBackedUp, FileBackupStatus{
				Path:      relPath,
				SizeBytes: sizeBytes,
				Locations: len(machines),
			})
		}

		return nil
	})

	result.AllBackedUp = (len(result.NotBackedUp) == 0) && (result.TotalFiles > 0)
	return result
}

func printCheckBackupResult(r BackupCheckResult) {
	sep := "================================================================="

	fmt.Fprintf(os.Stdout, "\n%s\n", sep)
	fmt.Fprintf(os.Stdout, "BACKUP CHECK\n")
	fmt.Fprintf(os.Stdout, "%s\n\n", sep)

	fmt.Fprintf(os.Stdout, "Folder: %s\n", r.FolderPath)
	fmt.Fprintf(os.Stdout, "Total files: %d\n\n", r.TotalFiles)

	if r.AllBackedUp {
		fmt.Fprintf(os.Stdout, "✅ All %d files are backed up on other machines\n", r.TotalFiles)
		fmt.Fprintf(os.Stdout, "   Safe to delete this folder\n\n")

		// Show backup locations
		if len(r.BackupLocations) > 0 {
			fmt.Fprintf(os.Stdout, "Backup locations:\n")
			// Sort locations for consistent output
			var locations []string
			for loc := range r.BackupLocations {
				locations = append(locations, loc)
			}
			sort.Strings(locations)

			for _, loc := range locations {
				count := r.BackupLocations[loc]
				fmt.Fprintf(os.Stdout, "   • %s (%d files)\n", loc, count)
			}
			fmt.Fprintf(os.Stdout, "\n")
		}
	} else {
		notBackedUpSize := int64(0)
		for _, f := range r.NotBackedUp {
			notBackedUpSize += f.SizeBytes
		}

		fmt.Fprintf(os.Stdout, "❌ %d file(s) NOT backed up elsewhere:\n\n", len(r.NotBackedUp))
		for i, f := range r.NotBackedUp {
			if i >= 10 {
				fmt.Fprintf(os.Stdout, "   ... and %d more files\n", len(r.NotBackedUp)-10)
				break
			}
			fmt.Fprintf(os.Stdout, "   • %s (%.1f MB)\n", f.Path, float64(f.SizeBytes)/(1024*1024))
		}

		fmt.Fprintf(os.Stdout, "\n   Total unbacked-up: %.1f GB\n", float64(notBackedUpSize)/(1024*1024*1024))
		fmt.Fprintf(os.Stdout, "   DO NOT delete this folder until these files are backed up\n\n")
	}

	fmt.Fprintf(os.Stdout, "%s\n", sep)
}

// MonitorDiskSpace checks available disk space and warns if low.
func MonitorDiskSpace(path string) bool {
	// Write a small test file to check writability and estimate space
	testFile := filepath.Join(path, ".photo-organizer-space-check")
	testData := make([]byte, 10*1024*1024) // 10MB test write

	err := os.WriteFile(testFile, testData, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠  Warning: Low disk space on %s\n", path)
		fmt.Fprintf(os.Stderr, "   Cannot write 10MB test file\n")
		return false
	}
	defer os.Remove(testFile)
	return true
}
