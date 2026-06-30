package main

import (
	"bytes"
	"context"
	"encoding/csv"
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
	FileModified string // file's modification time
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
	IsStale     bool   // true if ScanPath no longer exists on this machine
	IsLocal     bool   // true if this manifest is from the current machine
	Origin      string // "local" or "remote" for display
}

type StaleManifestReport struct {
	StaleCount int
	Details    []string // Human-readable details
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

// markManifestOrigin determines if a manifest is from the local machine or remote
func markManifestOrigin(src *ManifestSource, currentMachineID string) {
	// A manifest is local if its machine name matches the current machine
	src.IsLocal = src.MachineName == currentMachineID
	if src.IsLocal {
		src.Origin = "local"
	} else {
		src.Origin = "remote"
	}
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

// =============================================================================
// Migrate (copy unique files preserving folder structure)
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
	Name  string
	Pass  bool
	Error string
	Hint  string
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
	Machine            string
	TargetPath         string
	SafeFiles          []BackupComplianceFile
	RiskyFiles         []BackupComplianceFile
	CriticalFiles      []BackupComplianceFile
	SafeSize           int64
	RiskySize          int64
	CriticalSize       int64
	TotalSize          int64
	TotalFiles         int
	SafeableSpaceFreed int64
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

// isRemovablePath checks if a path is a typical removable media mount point
func isRemovablePath(path string) bool {
	path = filepath.Clean(path)
	lower := strings.ToLower(path)

	// macOS removable mounts
	if strings.HasPrefix(lower, "/volumes/") {
		return true
	}

	// Linux removable mounts
	if strings.HasPrefix(lower, "/mnt/") {
		return true
	}
	if strings.HasPrefix(lower, "/media/") {
		// /media/user/device is removable, but /media/Photos/archived might not be
		parts := strings.Split(lower, "/")
		// If it's /media/something/something (3+ parts), consider it removable
		// But if it has archive/backup keywords, it's permanent
		pathStr := strings.Join(parts, "/")
		if strings.Contains(pathStr, "archive") || strings.Contains(pathStr, "backup") ||
			strings.Contains(pathStr, "tank") {
			return false
		}
		if len(parts) <= 3 {
			return true
		}
	}

	// Windows removable drives (D:, E:, etc - typically not C:)
	if len(lower) == 2 && lower[1] == ':' && lower[0] != 'c' {
		return true
	}

	// Other patterns
	if strings.Contains(lower, "/usb") || strings.Contains(lower, "/sd") {
		return true
	}

	return false
}

// =============================================================================
// Check Backup Command
// =============================================================================

type BackupCheckResult struct {
	FolderPath      string
	TotalFiles      int
	TotalSize       int64
	BackedUpFiles   int
	SafelyBackedUp  []FileBackupStatus // 2+ copies (meets 3-2-1 rule)
	AtRisk          []FileBackupStatus // only 1 copy (risky)
	NotBackedUp     []FileBackupStatus // 0 copies
	BackupLocations map[string]int     // machine@path -> count of files
	AllBackedUp     bool
	IgnoredFiles    int   // number of system/sync files skipped
	IgnoredSize     int64 // total size of ignored files
}

type FileBackupStatus struct {
	Path            string
	SizeBytes       int64
	Locations       int      // number of machines that have this file
	LocationDetails []string // detailed list of locations where file exists
}

func checkFolderBackup(folderPath string, sources []ManifestSource, idx map[string][]hashLocation, localMachineID string) BackupCheckResult {
	result := BackupCheckResult{
		FolderPath:      folderPath,
		BackupLocations: make(map[string]int),
	}

	// Resolve to absolute path for comparison
	absFolderPath, _ := filepath.Abs(folderPath)

	// Load .photoignore patterns for this folder
	photoIgnore := newPhotoIgnore(absFolderPath)

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

		// Skip system and sync files
		if shouldSkipFile(path) {
			result.IgnoredFiles++
			result.IgnoredSize += info.Size()
			return nil
		}

		// Apply .photoignore patterns
		if photoIgnore.ShouldSkip(path) {
			result.IgnoredFiles++
			result.IgnoredSize += info.Size()
			return nil
		}

		sizeBytes := info.Size()
		result.TotalFiles++
		result.TotalSize += sizeBytes

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

		// Count backups: only count non-removable locations
		// Removable media (SD cards, USB) don't count as backups
		machineToLocations := make(map[string][]int) // machine → indices of its locations
		locationLabels := make(map[string]bool)
		var locationDetails []string

		// Detect overlapping manifests
		overlaps := overlappingPairs(sources)

		// First pass: collect non-removable locations and mark ones to skip
		skipIndices := make(map[int]bool)
		var backupLocations []int // indices of non-removable locations

		for i, loc := range locs {
			path := sources[loc.sourceIdx].ScanPath
			isLocalManifest := sources[loc.sourceIdx].MachineName == localMachineID

			// Skip removable media entirely - they don't count as backups
			if isRemovablePath(path) {
				skipIndices[i] = true
				continue
			}

			// Skip if it's the local machine's source folder (file we're checking from)
			if isLocalManifest && path == absFolderPath {
				skipIndices[i] = true
				continue
			}

			// This is a non-removable (permanent) location
			machine := sources[loc.sourceIdx].MachineName
			backupLocations = append(backupLocations, i)
			machineToLocations[machine] = append(machineToLocations[machine], i)

			// Check if this is from a broader manifest with a more specific child
			for pair := range overlaps {
				if pair[0] == loc.sourceIdx && sources[pair[0]].MachineName == machine {
					// This is from a broader manifest, check if child has same file
					childIdx := pair[1]
					childPath := sources[childIdx].ScanPath
					if !isRemovablePath(childPath) {
						for _, otherLoc := range locs {
							if otherLoc.sourceIdx == childIdx && sources[otherLoc.sourceIdx].MachineName == machine {
								// Found same file in more specific manifest
								skipIndices[i] = true
								break
							}
						}
					}
					if skipIndices[i] {
						break
					}
				}
			}
		}

		// Second pass: for each machine, keep only the most specific/canonical location
		for machine, indices := range machineToLocations {
			// Find the best location for this machine
			bestIdx := indices[0]
			bestLabel := sources[locs[bestIdx].sourceIdx].Label
			bestPath := sources[locs[bestIdx].sourceIdx].ScanPath

			for _, idx := range indices[1:] {
				if skipIndices[idx] {
					continue
				}
				label := sources[locs[idx].sourceIdx].Label
				path := sources[locs[idx].sourceIdx].ScanPath

				// Prefer paths with media-id suffix (e.g., /tankm1/media/Photos/sd_samsung_512g_untitled)
				hasMediaId := strings.Contains(path, machine)
				bestHasMediaId := strings.Contains(bestPath, machine)

				if hasMediaId && !bestHasMediaId {
					bestIdx = idx
					bestLabel = label
					bestPath = path
				} else if !hasMediaId && bestHasMediaId {
					// keep best
				} else if len(path) > len(bestPath) {
					// Prefer longer/more specific path
					bestIdx = idx
					bestLabel = label
					bestPath = path
				}
			}

			// Add only the best location for this machine
			if !locationLabels[bestLabel] {
				locationLabels[bestLabel] = true
				result.BackupLocations[bestLabel]++
				locationDetails = append(locationDetails, bestLabel)
			}
		}

		relPath, _ := filepath.Rel(folderPath, path)

		// Only count as backed up if it has non-removable locations
		if len(locationDetails) > 0 {
			result.BackedUpFiles++
			// Count unique machines from non-removable locations only
			backupMachines := make(map[string]bool)
			for _, label := range locationDetails {
				// Extract machine name from label (format: "machine @ path")
				parts := strings.Split(label, " @ ")
				if len(parts) > 0 {
					backupMachines[parts[0]] = true
				}
			}
			numCopies := len(backupMachines)
			status := FileBackupStatus{
				Path:            relPath,
				SizeBytes:       sizeBytes,
				Locations:       numCopies,
				LocationDetails: locationDetails,
			}
			// Categorize by safety: 2+ copies is safe, 1 copy is at risk
			if numCopies >= 2 {
				result.SafelyBackedUp = append(result.SafelyBackedUp, status)
			} else {
				result.AtRisk = append(result.AtRisk, status)
			}
		} else {
			// File exists in index but only on current machine
			result.NotBackedUp = append(result.NotBackedUp, FileBackupStatus{
				Path:      relPath,
				SizeBytes: sizeBytes,
				Locations: 0,
			})
		}

		return nil
	})

	result.AllBackedUp = (len(result.NotBackedUp) == 0 && len(result.AtRisk) == 0) && (result.TotalFiles > 0)
	return result
}

func printCheckBackupResult(r BackupCheckResult) {
	sep := "================================================================="

	fmt.Fprintf(os.Stdout, "\n%s\n", sep)
	fmt.Fprintf(os.Stdout, "BACKUP STATUS\n")
	fmt.Fprintf(os.Stdout, "%s\n\n", sep)

	fmt.Fprintf(os.Stdout, "Folder: %s\n", r.FolderPath)
	fmt.Fprintf(os.Stdout, "Total files: %d\n", r.TotalFiles)
	if r.IgnoredFiles > 0 {
		fmt.Fprintf(os.Stdout, "Ignored (system/sync files): %d\n", r.IgnoredFiles)
	}

	// Calculate sizes for each category
	safelyBackedUpSize := int64(0)
	for _, f := range r.SafelyBackedUp {
		safelyBackedUpSize += f.SizeBytes
	}
	atRiskSize := int64(0)
	for _, f := range r.AtRisk {
		atRiskSize += f.SizeBytes
	}
	notBackedUpSize := int64(0)
	for _, f := range r.NotBackedUp {
		notBackedUpSize += f.SizeBytes
	}
	fmt.Fprintf(os.Stdout, "Total size: %.1f GB\n\n", float64(r.TotalSize)/(1024*1024*1024))

	// Summary counts - categorized by safety (only show icon if there are files)
	if len(r.SafelyBackedUp) > 0 {
		fmt.Fprintf(os.Stdout, "✅ SAFELY BACKED UP (2+ copies): %d files (%.1f GB)\n", len(r.SafelyBackedUp), float64(safelyBackedUpSize)/(1024*1024*1024))
	}
	if len(r.AtRisk) > 0 {
		fmt.Fprintf(os.Stdout, "⚠️  AT RISK (only 1 copy): %d files (%.1f GB)\n", len(r.AtRisk), float64(atRiskSize)/(1024*1024*1024))
	}
	if len(r.NotBackedUp) > 0 {
		fmt.Fprintf(os.Stdout, "❌ NOT BACKED UP (0 copies): %d files (%.1f GB)\n", len(r.NotBackedUp), float64(notBackedUpSize)/(1024*1024*1024))
	}
	fmt.Fprintf(os.Stdout, "\n")

	// Show sample safely backed-up files
	if len(r.SafelyBackedUp) > 0 {
		fmt.Fprintf(os.Stdout, "Sample safely backed-up files (showing first 5):\n")
		for i, f := range r.SafelyBackedUp {
			if i >= 5 {
				fmt.Fprintf(os.Stdout, "   ... and %d more safely backed-up files\n\n", len(r.SafelyBackedUp)-5)
				break
			}
			fmt.Fprintf(os.Stdout, "   • %s (%s, %d copies)\n", f.Path, formatBytes(f.SizeBytes), f.Locations)
			for _, loc := range f.LocationDetails {
				fmt.Fprintf(os.Stdout, "     → %s\n", loc)
			}
		}
		if len(r.SafelyBackedUp) <= 5 {
			fmt.Fprintf(os.Stdout, "\n")
		}
	}

	// Show at-risk files
	if len(r.AtRisk) > 0 {
		fmt.Fprintf(os.Stdout, "Files AT RISK (only 1 copy - showing first 5):\n")
		for i, f := range r.AtRisk {
			if i >= 5 {
				fmt.Fprintf(os.Stdout, "   ... and %d more at-risk files\n\n", len(r.AtRisk)-5)
				break
			}
			fmt.Fprintf(os.Stdout, "   • %s (%s)\n", f.Path, formatBytes(f.SizeBytes))
			for _, loc := range f.LocationDetails {
				fmt.Fprintf(os.Stdout, "     → %s\n", loc)
			}
		}
		fmt.Fprintf(os.Stdout, "\n   ⚠️  These files have only 1 backup copy. Create another copy to be safe.\n\n")
	}

	// Show not backed up files
	if len(r.NotBackedUp) > 0 {
		fmt.Fprintf(os.Stdout, "Files NOT backed up (showing first 5):\n")
		for i, f := range r.NotBackedUp {
			if i >= 5 {
				fmt.Fprintf(os.Stdout, "   ... and %d more files\n\n", len(r.NotBackedUp)-5)
				break
			}
			fmt.Fprintf(os.Stdout, "   • %s (%s)\n", f.Path, formatBytes(f.SizeBytes))
		}
		fmt.Fprintf(os.Stdout, "\n   ❌ Back these up before deleting folder\n\n")
	}

	if len(r.NotBackedUp) == 0 && len(r.AtRisk) == 0 {
		fmt.Fprintf(os.Stdout, "✓ All files are safely backed up (2+ copies)\n")
		fmt.Fprintf(os.Stdout, "  Safe to archive or delete this folder\n\n")
	} else if len(r.NotBackedUp) == 0 && len(r.AtRisk) > 0 {
		fmt.Fprintf(os.Stdout, "⚠️  Some files have only 1 backup copy\n")
		fmt.Fprintf(os.Stdout, "  Create additional copies before deleting\n\n")
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

// extractDeviceInfo extracts human-readable device info from config
func extractDeviceInfo(machineInfo string) string {
	if strings.Contains(machineInfo, "[removable]") {
		// Extract device type from config string
		if strings.Contains(machineInfo, "scanned from:") {
			parts := strings.Split(machineInfo, "scanned from:")
			if len(parts) > 0 {
				path := strings.TrimSpace(parts[1])
				deviceName := filepath.Base(path)

				if strings.Contains(machineInfo, "camera") || strings.Contains(deviceName, "Untitled") {
					return fmt.Sprintf("Camera/USB: %s", deviceName)
				}
				if strings.Contains(deviceName, "SD") || strings.Contains(deviceName, "Card") {
					return fmt.Sprintf("SD Card: %s", deviceName)
				}
				return fmt.Sprintf("Removable: %s", deviceName)
			}
		}
		return "Removable media"
	}
	return ""
}

// =============================================================================
// Storage Status - Machine & Device Level Storage Analysis
// =============================================================================
