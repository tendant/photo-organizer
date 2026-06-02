package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// Data Structures
// =============================================================================

type ManifestRow struct {
	Filename     string
	RelativePath string
	SizeBytes    int64
	FileHash     string
	Extension    string
	ScanPath     string
	MachineName  string // empty on old 12-column manifests
	HashMode     string // "partial" or "full"; empty = partial (old manifests)
}

// ManifestSource is one scan run: one CSV file = one (machine, folder) pair.
type ManifestSource struct {
	FilePath    string
	MachineName string
	ScanPath    string
	Label       string // "machineName @ scanPath"
	Rows        []ManifestRow
}

type DuplicateGroup struct {
	Hash      string
	SizeBytes int64
	Locations []string // "label: relative_path"
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

	col := func(row []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}

	var rows []ManifestRow
	for _, row := range records[1:] {
		if len(row) < 2 {
			continue
		}
		size, _ := strconv.ParseInt(col(row, "file_size_bytes"), 10, 64)
		hashMode := col(row, "hash_mode")
		if hashMode == "" {
			hashMode = "partial"
		}
		rows = append(rows, ManifestRow{
			Filename:     col(row, "filename"),
			RelativePath: col(row, "relative_path"),
			SizeBytes:    size,
			FileHash:     col(row, "file_hash"),
			Extension:    col(row, "extension"),
			ScanPath:     col(row, "scan_path"),
			MachineName:  col(row, "machine_name"),
			HashMode:     hashMode,
		})
	}

	// Derive machine name: from rows, or fall back to CSV filename stem.
	machine := ""
	scanPath := ""
	for _, row := range rows {
		if row.MachineName != "" {
			machine = row.MachineName
		}
		if row.ScanPath != "" {
			scanPath = row.ScanPath
		}
		if machine != "" && scanPath != "" {
			break
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

	return ManifestSource{
		FilePath:    csvPath,
		MachineName: machine,
		ScanPath:    scanPath,
		Label:       label,
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

// buildHashIndex returns hash → list of (sourceIndex, rowIndex) pairs.
// sourceIndex indexes into sources; rowIndex into sources[i].Rows.
type hashLocation struct {
	sourceIdx int
	rowIdx    int
}

func buildHashIndex(sources []ManifestSource) map[string][]hashLocation {
	idx := make(map[string][]hashLocation)
	for si, src := range sources {
		for ri, row := range src.Rows {
			if row.FileHash == "" {
				continue
			}
			idx[row.FileHash] = append(idx[row.FileHash], hashLocation{si, ri})
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

func findDuplicates(sources []ManifestSource, idx map[string][]hashLocation) []DuplicateGroup {
	var groups []DuplicateGroup
	for hash, locs := range idx {
		machines := distinctMachines(locs, sources)
		if len(machines) < 2 {
			continue
		}
		var locations []string
		var sizeBytes int64
		for _, loc := range locs {
			row := sources[loc.sourceIdx].Rows[loc.rowIdx]
			locations = append(locations, sources[loc.sourceIdx].Label+": "+row.RelativePath)
			sizeBytes = row.SizeBytes
		}
		sort.Strings(locations)
		groups = append(groups, DuplicateGroup{
			Hash:      hash,
			SizeBytes: sizeBytes,
			Locations: locations,
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
	MachineName   string
	Hash          string
	SizeBytes     int64
	Locations     []string // "label: relative_path"
	HashMode      string   // "partial" or "full"
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
			hashMode := "full"
			for _, loc := range mLocs {
				row := sources[loc.sourceIdx].Rows[loc.rowIdx]
				abs := absFilePath(sources[loc.sourceIdx], row)
				if seenAbs[abs] {
					continue
				}
				seenAbs[abs] = true
				locations = append(locations, sources[loc.sourceIdx].Label+": "+row.RelativePath)
				sizeBytes = row.SizeBytes
				if row.HashMode == "partial" {
					hashMode = "partial"
				}
			}
			if len(locations) < 2 {
				continue
			}
			sort.Strings(locations)
			result = append(result, intraDupGroup{
				MachineName: machine,
				Hash:        hash,
				SizeBytes:   sizeBytes,
				Locations:   locations,
				HashMode:    hashMode,
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

			locs := idx[row.FileHash]
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
		fmt.Fprintf(w, "  [%8s]  %s\n", formatSize(g.SizeBytes), g.Hash[:12]+"...")
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

		var confirmed, unconfirmed []intraDupGroup
		for _, g := range intraDups {
			if g.HashMode == "full" {
				confirmed = append(confirmed, g)
			} else {
				unconfirmed = append(unconfirmed, g)
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

		printIntraDupGroups(confirmed, "CONFIRMED duplicates (full hash)")
		if len(unconfirmed) > 0 {
			printIntraDupGroups(unconfirmed, "UNCONFIRMED — partial hash only (may be false positives for videos)")
			fmt.Fprintf(w, "\n  ⚠  Re-scan with --full-hash to confirm or dismiss these %s groups.\n",
				formatCount(len(unconfirmed)))
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
	fmt.Fprintf(w, "  Hash mode:  MD5 first 64KB (fast; sufficient for dedup)\n")
	fmt.Fprintf(w, "  Total files across all sources:   %s  (%s)\n", formatCount(totalFiles), formatSize(totalBytes))
	fmt.Fprintf(w, "  Duplicated on 2+ machines:        %s (%.1f%%)\n",
		formatCount(len(duplicates)), pct(len(duplicates), totalFiles))
	fmt.Fprintf(w, "  Unique to one machine:            %s (%.1f%%)\n",
		formatCount(totalUnique), pct(totalUnique, totalFiles))
	fmt.Fprintf(w, "  Fully-redundant folders (100%%):   %s\n", formatCount(fullRedundant))
	fmt.Fprintf(w, "  Nearly-redundant folders (>%.0f%%): %s\n", threshold*100, formatCount(highRedundant))
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
	w.Write([]string{"hash", "size_bytes", "location"})
	for _, g := range groups {
		for _, loc := range g.Locations {
			w.Write([]string{g.Hash, strconv.FormatInt(g.SizeBytes, 10), loc})
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
	w.Write([]string{"machine_name", "relative_path", "size_bytes", "extension", "file_hash"})
	machines := make([]string, 0, len(uniqueByMachine))
	for m := range uniqueByMachine {
		machines = append(machines, m)
	}
	sort.Strings(machines)
	for _, m := range machines {
		rows := uniqueByMachine[m]
		sort.Slice(rows, func(i, j int) bool { return rows[i].RelativePath < rows[j].RelativePath })
		for _, row := range rows {
			w.Write([]string{m, row.RelativePath, strconv.FormatInt(row.SizeBytes, 10), row.Extension, row.FileHash})
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

func runPlan(args []string) {
	// Pre-separate flags from positional args.
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if name == "keep" || name == "out" {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	keepFlag := fs.String("keep", "", "machine name whose copies are the authoritative backup (required)")
	outFlag := fs.String("out", "", "write shell script to this file instead of stdout")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer plan --keep <machine> [manifest...]\n\n")
		fmt.Fprintf(os.Stderr, "Generates a shell script of rm commands for files safely backed up\n")
		fmt.Fprintf(os.Stderr, "on the --keep machine. Review the script before running it.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	if *keepFlag == "" {
		fmt.Fprintln(os.Stderr, "plan: --keep <machine> is required")
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

	// Verify keep machine exists in manifests.
	keepFound := false
	for _, src := range sources {
		if src.MachineName == *keepFlag {
			keepFound = true
			break
		}
	}
	if !keepFound {
		fmt.Fprintf(os.Stderr, "plan: machine %q not found in manifests\n", *keepFlag)
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

	candidates := buildDeletePlan(sources, *keepFlag)

	// Verify backup files exist on disk where accessible.
	// Paths that can't be stat'd are marked unverified in the script.
	verified, unverified := 0, 0
	for i := range candidates {
		for j := range candidates[i].Backups {
			if _, err := os.Stat(candidates[i].Backups[j].AbsPath); err == nil {
				verified++
			} else {
				candidates[i].Backups[j].Label += "  ⚠ NOT VERIFIED ON DISK"
				unverified++
			}
		}
	}
	if unverified > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠  %d backup path(s) could not be verified on disk.\n", unverified)
		fmt.Fprintf(os.Stderr, "   Re-scan the keep machine and verify paths are mounted before running the script.\n")
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
	fmt.Fprintf(out, "# Keeping authoritative copy on: %s\n", *keepFlag)
	fmt.Fprintf(out, "# Files to remove: %s  (%s reclaimable)\n", formatCount(len(candidates)), formatSize(totalSize))
	fmt.Fprintf(out, "# Backup verified on disk: %d  |  unverified: %d\n", verified, unverified)
	fmt.Fprintf(out, "#\n")
	fmt.Fprintf(out, "# REVIEW CAREFULLY before running.\n")
	fmt.Fprintf(out, "# All rm commands are commented out. Uncomment lines you want to execute,\n")
	fmt.Fprintf(out, "# or run:  bash <(grep -v '^#' this_script.sh)\n")
	if unverified > 0 {
		fmt.Fprintf(out, "#\n# WARNING: Some backups marked ⚠ could not be verified on disk.\n")
		fmt.Fprintf(out, "# Do NOT delete files with unverified backups.\n")
	}
	fmt.Fprintf(out, "#\n")

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
			fmt.Fprintf(out, "#rm %s\n", shellQuote(abs))
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

// =============================================================================
// Formatting Helpers
// =============================================================================

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
