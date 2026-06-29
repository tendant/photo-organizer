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
	"time"
)

type hashLocation struct {
	sourceIdx int
	rowIdx    int
}

func indexKey(partialHash string, sizeBytes int64) string {
	return fmt.Sprintf("%s|%d", partialHash, sizeBytes)
}

// detectStaleManifests checks which manifests reference paths that no longer exist (locally)
func detectStaleManifests(sources []ManifestSource) StaleManifestReport {
	report := StaleManifestReport{
		Details: []string{},
	}

	// Get current machine ID to identify local vs remote manifests
	localMachineID := resolveMachineID("")

	for i := range sources {
		// Ensure origin is marked before checking
		if sources[i].Origin == "" {
			markManifestOrigin(&sources[i], localMachineID)
		}

		// Only report stale LOCAL manifests (we can verify them with os.Stat)
		// Remote manifests are unreachable from here, so skip them
		if sources[i].IsLocal {
			if _, err := os.Stat(sources[i].ScanPath); os.IsNotExist(err) {
				sources[i].IsStale = true
				report.StaleCount++
				manifestFile := filepath.Base(sources[i].FilePath)
				detail := fmt.Sprintf(
					"⚠  Stale manifest: %q @ %q (path no longer exists)\n"+
						"   File: %s\n"+
						"   Last scanned: %s\n"+
						"   Contains: %d file(s)\n"+
						"   This likely means the folder was moved, archived, or deleted.",
					sources[i].MachineName, sources[i].ScanPath, manifestFile, sources[i].LastScanned, len(sources[i].Rows),
				)
				report.Details = append(report.Details, detail)
			}
		}
	}

	return report
}

// printStaleManifestReport shows stale manifest info to user if found
func printStaleManifestReport(report StaleManifestReport) {
	if report.StaleCount == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "STALE MANIFESTS DETECTED\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

	for _, detail := range report.Details {
		fmt.Fprintf(os.Stderr, "%s\n\n", detail)
	}

	fmt.Fprintf(os.Stderr, "⚠️  Found %d stale manifest(s). Files from these manifests are still included\n", report.StaleCount)
	fmt.Fprintf(os.Stderr, "in the analysis, but may reference archived or deleted folders.\n\n")
	fmt.Fprintf(os.Stderr, "To clean up stale manifests, see:\n")
	fmt.Fprintf(os.Stderr, "   photo-organizer cleanup-manifests\n\n")
}

// DeduplicationReport tracks files excluded due to overlapping scans
type DeduplicationReport struct {
	OverlapGroups map[[2]int]int // pair → count of files excluded from broader manifest
	TotalExcluded int
	Details       []string // Human-readable details
}

// reportOverlapDeduplication analyzes what would be deduplicated and returns details
func reportOverlapDeduplication(sources []ManifestSource) DeduplicationReport {
	report := DeduplicationReport{
		OverlapGroups: make(map[[2]int]int),
		Details:       []string{},
	}

	if len(sources) < 2 {
		return report
	}

	overlaps := overlappingPairs(sources)
	if len(overlaps) == 0 {
		return report
	}

	// Track which pairs we've already reported (using canonical form)
	reportedPairs := make(map[[2]int]bool)

	// For each overlapping pair, count how many files would be excluded
	for pair := range overlaps {
		// Normalize pair so smaller index is first
		canonical := pair
		if pair[0] > pair[1] {
			canonical = [2]int{pair[1], pair[0]}
		}

		// Skip if we've already reported this canonical pair
		if reportedPairs[canonical] {
			continue
		}
		reportedPairs[canonical] = true

		// Use the normalized canonical pair for all lookups below
		pair = canonical

		broaderIdx := canonical[0]
		if strings.HasPrefix(
			filepath.Clean(sources[canonical[0]].ScanPath),
			filepath.Clean(sources[canonical[1]].ScanPath)+string(filepath.Separator),
		) {
			broaderIdx = canonical[1]
		}
		specificIdx := canonical[1]
		if broaderIdx == canonical[1] {
			specificIdx = canonical[0]
		}

		broaderSrc := sources[broaderIdx]
		specificSrc := sources[specificIdx]
		excludedCount := 0

		// Build set of absolute paths in specific scan for fast lookup
		specificPaths := make(map[string]bool)
		for _, row := range specificSrc.Rows {
			absPath := absFilePath(specificSrc, row)
			specificPaths[filepath.Clean(absPath)] = true
		}

		// Count files that would be excluded from the broader manifest
		for _, row := range broaderSrc.Rows {
			absPath := filepath.Clean(absFilePath(broaderSrc, row))
			if specificPaths[absPath] {
				excludedCount++
			}
		}

		if excludedCount > 0 {
			report.OverlapGroups[canonical] = excludedCount
			report.TotalExcluded += excludedCount
			detail := fmt.Sprintf(
				"⚠  Overlapping scans: %q and %q (same machine %q)\n"+
					"   Excluding %d file(s) from broader scan (using more specific scan instead)",
				broaderSrc.ScanPath, specificSrc.ScanPath, broaderSrc.MachineName, excludedCount,
			)
			report.Details = append(report.Details, detail)
		}
	}

	return report
}

// printDeduplicationReportFiltered shows overlaps, optionally filtering to local machine only
func printDeduplicationReportFiltered(report DeduplicationReport, localMachineID string, sources []ManifestSource) {
	if report.TotalExcluded == 0 {
		return
	}

	// Build filtered details (always try to filter to local machine)
	filteredDetails := []string{}
	if localMachineID != "" && sources != nil {
		// Explicit filtering: show only local machine overlaps
		for pair, count := range report.OverlapGroups {
			// Check if both manifests are from the local machine
			if sources[pair[0]].MachineName == localMachineID && sources[pair[1]].MachineName == localMachineID {
				broaderIdx := pair[0]
				if strings.HasPrefix(
					filepath.Clean(sources[pair[0]].ScanPath),
					filepath.Clean(sources[pair[1]].ScanPath)+string(filepath.Separator),
				) {
					broaderIdx = pair[1]
				}
				specificIdx := pair[1]
				if broaderIdx == pair[1] {
					specificIdx = pair[0]
				}
				broaderSrc := sources[broaderIdx]
				specificSrc := sources[specificIdx]
				detail := fmt.Sprintf(
					"⚠  Overlapping scans: %q and %q (same machine %q)\n"+
						"   Excluding %d file(s) from broader scan (using more specific scan instead)",
					broaderSrc.ScanPath, specificSrc.ScanPath, broaderSrc.MachineName, count,
				)
				filteredDetails = append(filteredDetails, detail)
			}
		}
	} else {
		// No filtering requested, use all details (this shouldn't normally happen)
		filteredDetails = report.Details
	}

	if len(filteredDetails) == 0 {
		// No overlaps to report (all were remote)
		return
	}

	fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "MANIFEST DEDUPLICATION\n")
	fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

	for _, detail := range filteredDetails {
		fmt.Fprintf(os.Stderr, "%s\n", detail)
	}

	fmt.Fprintf(os.Stderr, "\nTotal files excluded from duplicate scans: %d\n", report.TotalExcluded)
	fmt.Fprintf(os.Stderr, "This ensures files are counted only once in analysis.\n\n")
}

func buildHashIndex(sources []ManifestSource) map[string][]hashLocation {
	idx := make(map[string][]hashLocation)

	// Detect overlapping manifests (same machine, nested paths)
	overlaps := overlappingPairs(sources)

	for si, src := range sources {
		for ri, row := range src.Rows {
			if row.PartialHash == "" {
				continue
			}

			// Check if this entry should be skipped due to overlap
			// Skip if there's a more specific (child) manifest with the same file
			skip := false
			absPath := absFilePath(src, row)

			for pair := range overlaps {
				// pair[0] is broader, pair[1] is more specific
				if pair[0] == si {
					// This is the broader manifest, check if child has this file
					childSrc := sources[pair[1]]
					childAbsPath := filepath.Join(childSrc.ScanPath, filepath.FromSlash(row.RelativePath))
					if filepath.Clean(childAbsPath) == filepath.Clean(absPath) {
						// File exists in both, skip from broader manifest
						skip = true
						break
					}
				}
			}

			if skip {
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
				MachineName: machine,
				Hash:        hash,
				SizeBytes:   sizeBytes,
				Locations:   locations,
				FullHashed:  allFullHashed,
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
	MachineName string
	Sources     []string // labels of all scan sources for this machine
	TotalFiles  int
	TotalBytes  int64
	UniqueFiles int
	DupedFiles  int
	ByType      map[string]int // "photo"/"video"/"audio"/"sidecar"/"other"
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

func printReport(sources []ManifestSource, threshold float64, topN int, w io.Writer) {
	overlaps := overlappingPairs(sources)
	idx := buildHashIndex(sources)
	duplicates := findDuplicates(sources, idx)
	uniqueByMachine := findUnique(sources, idx)
	intraDups := findIntraMachine(sources, idx)
	folderStats := computeFolderRedundancy(sources, idx)
	summaries := computeSummaries(sources, idx)

	// Filter to top N most duplicated folders if requested
	if topN > 0 && len(folderStats) > topN {
		// Sort by coverage (descending) to get most duplicated first
		sort.Slice(folderStats, func(i, j int) bool {
			return folderStats[i].Coverage > folderStats[j].Coverage
		})
		folderStats = folderStats[:topN]
	}

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
		fmt.Fprintln(w, "  Run 'photo-organizer scan <folder> --prune' on these machines to refresh.")
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
				if name == "csv" || name == "threshold" || name == "top" {
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
	topN := fs.Int("top", 0, "show only top N most duplicated folders (0 = all)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer dups [--csv prefix] [--threshold 0.9] [--top N] [manifest1.csv ...]\n")
		fs.PrintDefaults()
	}
	fs.Parse(flagArgs)

	manifestPaths := posArgs
	if len(manifestPaths) == 0 {
		defaultDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
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
	fmt.Fprintf(os.Stderr, "Tip: run 'photo-organizer dup-folders --top 20' before cleanup, then verify with 'photo-organizer check-backup <folder>'.\n\n")

	printReport(sources, *threshold, *topN, os.Stdout)

	if *csvPrefix != "" {
		fmt.Fprintln(os.Stdout)
		if err := writeAnalysisCSV(sources, *csvPrefix); err != nil {
			fmt.Fprintln(os.Stderr, "Error writing CSV:", err)
			os.Exit(1)
		}
	}
}

// =============================================================================
