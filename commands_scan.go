package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func runScan(args []string) error {
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
				if name == "root" || name == "machine" || name == "media-id" || name == "score-threshold" { // value-taking flags only
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "where to write the manifest (default: ~/manifests)")
	machineFlag := fs.String("machine", "", "machine label embedded in manifest (default: stable machine ID)")
	mediaIDFlag := fs.String("media-id", "", "stable identifier for removable media (same across different machines)")
	noWriteMediaIDFlag := fs.Bool("no-write-media-id", false, "skip writing machine-id file to card")
	noCacheFlag := fs.Bool("no-cache", false, "recompute all hashes, ignoring cached values (use after hash algorithm change)")
	pruneFlag := fs.Bool("prune", true, "remove manifest entries for files no longer on disk (default; kept for old scripts)")
	noPruneFlag := fs.Bool("no-prune", false, "keep manifest entries for files that are no longer on disk")
	forcePruneFlag := fs.Bool("force-prune", false, "prune even when that removes more than half of the manifest (after moving or deleting most of a folder on purpose)")
	separateFlag := fs.Bool("separate", false, "give a subfolder of an already-scanned folder its own manifest instead of updating the parent's")
	holdingFlag := fs.Bool("holding", false, "keep this folder's manifest in _Manifest/_holding/ so dups and check-backup do not count it as a copy (for folders you are about to delete)")
	autoIdentifyFlag := fs.Bool("auto-identify-folders", false, "sample subdirectories and only scan those matching score threshold")
	scoreThresholdFlag := fs.Int("score-threshold", 30, "minimum folder score (0-100) for --auto-identify-folders (default: 30)")
	detectOnlyFlag := fs.Bool("detect-only", false, "with --auto-identify-folders: show detection results and exit without scanning")
	noReportFlag := fs.Bool("no-report", false, "skip file type coverage report (usually shown before scanning)")
	fs.Usage = printUsage
	defer func() { forcePrune = false }()
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // help requested and printed; not a failure
		}
		return errFailed // flag package already printed the error and usage
	}
	forcePrune = *forcePruneFlag

	// Resolve machine name (priority: flag > ./machine-id > ~/manifests/machine-id)
	machineName := resolveMachineID(*machineFlag)

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
			return errFailed
		}
	}

	// Resolve scanDir to an absolute path for stable manifest naming.
	absScanDir, err := filepath.Abs(scanDir)
	if err != nil {
		absScanDir = scanDir
	}

	// Auto-read media-id from card if not provided via flag
	if *mediaIDFlag == "" {
		cardIDPath := filepath.Join(absScanDir, "machine-id")
		if data, err := os.ReadFile(cardIDPath); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				*mediaIDFlag = id
			}
		}
	}

	// Apply media-id to machine name after auto-read
	if *mediaIDFlag != "" {
		machineName = *mediaIDFlag
	}

	// Determine where manifest is written
	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}

	// Exit if scanning removable media without media-id (before pre-flight)
	if *mediaIDFlag == "" && isRemovableMedia(absScanDir) {
		fmt.Fprintf(os.Stderr, "⚠  Removable media detected: %s\n", absScanDir)
		fmt.Fprintf(os.Stderr, "   This is removable media and requires --media-id to scan.\n")
		fmt.Fprintf(os.Stderr, "   Without it, the card will be recorded under this machine (%q)\n", machineName)
		fmt.Fprintf(os.Stderr, "   and appear as different machines when scanned on other systems.\n\n")
		fmt.Fprintf(os.Stderr, "   Please provide a stable identifier:\n")
		fmt.Fprintf(os.Stderr, "     photo-organizer scan %s --media-id \"<label-on-card>\"\n\n", absScanDir)
		return errFailed
	}

	// Write machine-id to card on first use (unless --no-write-media-id)
	if *mediaIDFlag != "" && !*noWriteMediaIDFlag {
		cardIDPath := filepath.Join(absScanDir, "machine-id")
		if _, err := os.Stat(cardIDPath); os.IsNotExist(err) {
			if err := os.WriteFile(cardIDPath, []byte(*mediaIDFlag+"\n"), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "⚠  Could not write machine-id to card: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✓  Media ID %q written to %s\n", *mediaIDFlag, cardIDPath)
			}
		}
	}

	// Ensure the manifest directory exists — on a fresh machine nothing has
	// created it yet, and the writability preflight below would fail otherwise.
	// If MkdirAll fails, the preflight reports the directory as unwritable.
	_ = os.MkdirAll(filepath.Join(manifestRoot, "_Manifest"), 0755)

	// Pre-flight checks
	fmt.Fprintf(os.Stderr, "Pre-flight checks:\n")
	checks := []PreflightCheck{
		CheckDirReadable(absScanDir, "Source directory readable"),
		CheckDirWritable(filepath.Join(manifestRoot, "_Manifest"), "Manifest directory writable"),
	}
	if !RunPreflightChecks(checks) {
		return errFailed
	}

	// Check if folder was already scanned
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		if src.ScanPath == absScanDir {
			fmt.Fprintf(os.Stderr, "\n⚠️  This folder was already scanned:\n")
			fmt.Fprintf(os.Stderr, "   Machine: %s\n", src.MachineName)
			fmt.Fprintf(os.Stderr, "   Last scanned: %s\n", src.LastScanned)
			fmt.Fprintf(os.Stderr, "   Files: %d\n", len(src.Rows))
			fmt.Fprintf(os.Stderr, "   Tip: run scan again with --prune to refresh this manifest.\n\n")
			break
		}
	}

	// Show file type coverage report by default (unless --no-report is set)
	if !*noReportFlag {
		analyzeDirectoryTypes(absScanDir)
		fmt.Fprintf(os.Stderr, "Proceeding with scan...\n\n")
	}

	// If auto-identify-folders is set, sample subdirectories and scan only those matching score threshold.
	if *autoIdentifyFlag {
		qualifying, skipped, err := identifyPhotoFolders(absScanDir, *scoreThresholdFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error identifying photo folders: %v\n", err)
			return errFailed
		}

		if len(qualifying) == 0 {
			fmt.Fprintf(os.Stderr, "No photo folders found.\n")
			return errFailed
		}

		fmt.Printf("Auto-identifying photo folders in %s...\n\n", scanDir)
		for _, scored := range qualifying {
			ratio := 0.0
			if scored.Sample.TotalCount > 0 {
				ratio = float64(scored.Sample.MediaCount) / float64(scored.Sample.TotalCount) * 100
			}
			reasonsStr := strings.Join(scored.Reasons, ", ")
			fmt.Printf("  [%3d] ✓ %-40s %3d%% media, %s files  (%s)\n",
				scored.Score, filepath.Base(scored.Path), int(ratio), formatCount(scored.Sample.TotalCount), reasonsStr)
		}
		fmt.Printf("  ──── threshold: %d ────────────────────────────────────────────\n", *scoreThresholdFlag)
		for _, scored := range skipped {
			ratio := 0.0
			if scored.Sample.TotalCount > 0 {
				ratio = float64(scored.Sample.MediaCount) / float64(scored.Sample.TotalCount) * 100
			}
			reasonsStr := strings.Join(scored.Reasons, ", ")
			fmt.Printf("  [%3d] ✗ %-40s %3d%% media, %s files  (%s)\n",
				scored.Score, filepath.Base(scored.Path), int(ratio), formatCount(scored.Sample.TotalCount), reasonsStr)
		}
		if *detectOnlyFlag {
			fmt.Printf("\nDetection complete. Use --auto-identify-folders (without --detect-only) to scan these folders.\n")
			return nil
		}

		fmt.Printf("\nScanning %d photo folder(s)...\n\n", len(qualifying))

		// Scan each qualifying folder separately.
		for i, scored := range qualifying {
			qualifyingAbsDir, err := filepath.Abs(scored.Path)
			if err != nil {
				qualifyingAbsDir = scored.Path
			}

			manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, qualifyingAbsDir))
			manifestDir := filepath.Dir(manifestFile)
			if err := os.MkdirAll(manifestDir, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "Error: cannot create manifest directory %s: %v\n", manifestDir, err)
				return errFailed
			}

			fmt.Printf("[%d/%d] Scanning: %s\n", i+1, len(qualifying), scored.Path)
			fmt.Printf("        Manifest: %s\n\n", manifestFile)

			cache := loadCache(manifestFile)
			if *noCacheFlag {
				cache = make(map[string]CacheEntry)
			}
			photoIgnore := newPhotoIgnore(qualifyingAbsDir)
			fmt.Fprintf(os.Stderr, "Scanning directory tree...\n")
			files, scanStats, err := scanDirectory(qualifyingAbsDir, cache, photoIgnore)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error scanning %s: %v\n", scored.Path, err)
				continue
			}

			fmt.Fprintf(os.Stderr, "✓ Scan complete: %d files found\n", len(files))
			fmt.Fprintf(os.Stderr, "Writing manifest file...\n")
			manifestStats, err := updateManifest(qualifyingAbsDir, files, manifestFile, machineName, pruneAllowed(*pruneFlag && !*noPruneFlag, scanStats))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
				continue
			}

			printScanSummary(scanStats, manifestStats)
		}
	} else {
		// Original single-folder scan.
		manifestFile := filepath.Join(manifestRoot, "_Manifest", manifestFilename(machineName, absScanDir))
		if held := filepath.Join(manifestRoot, "_Manifest", heldDirName, manifestFilename(machineName, absScanDir)); *holdingFlag || fileExists(held) {
			// A holding folder keeps its manifest apart; --holding also moves an existing one there.
			if _, err := os.Stat(held); err != nil && fileExists(manifestFile) {
				if err := os.MkdirAll(filepath.Dir(held), 0755); err == nil {
					os.Rename(manifestFile, held)
				}
			}
			manifestFile = held
		}

		// A folder inside one that already has a manifest updates that manifest (only its part of it),
		// so a huge tree never needs a full rescan for a change in one corner.
		if !fileExists(manifestFile) && !*separateFlag {
			if root, parent := findParentManifest(manifestRoot, machineName, absScanDir); parent != "" {
				return partialScan(absScanDir, root, parent, machineName, *noCacheFlag, *pruneFlag && !*noPruneFlag, *forcePruneFlag)
			}
		}

		// Check write access to manifest directory before spending time scanning.
		manifestDir := filepath.Dir(manifestFile)
		if err := os.MkdirAll(manifestDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot create manifest directory %s: %v\n", manifestDir, err)
			return errFailed
		}

		fmt.Printf("Scanning:  %s\n", scanDir)
		fmt.Printf("Manifest:  %s\n", manifestFile)
		fmt.Printf("Machine:   %s\n\n", machineName)

		cache := loadCache(manifestFile)
		if *noCacheFlag {
			cache = make(map[string]CacheEntry) // discard cache — force full recompute
		}
		photoIgnore := newPhotoIgnore(absScanDir)

		// Show .photoignore hint if it doesn't exist
		photoignorePath := filepath.Join(absScanDir, ".photoignore")
		if _, err := os.Stat(photoignorePath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "💡 Tip: Create a .photoignore file to exclude folders/files from backup\n")
			fmt.Fprintf(os.Stderr, "   Example: echo '.claude/\\n_Manifest/' > %s\n\n", photoignorePath)
		}

		fmt.Fprintf(os.Stderr, "\nScanning directory tree...\n")
		files, scanStats, err := scanDirectory(absScanDir, cache, photoIgnore)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return errFailed
		}

		fmt.Fprintf(os.Stderr, "\n✓ Scan complete: %d files found\n", len(files))
		fmt.Fprintf(os.Stderr, "Writing manifest file...\n")
		manifestStats, err := updateManifest(absScanDir, files, manifestFile, machineName, pruneAllowed(*pruneFlag && !*noPruneFlag, scanStats))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error writing manifest:", err)
			return errFailed
		}

		printScanSummary(scanStats, manifestStats)
	}
	return nil
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
	if m.Pruned > 0 {
		fmt.Printf("  Pruned (deleted):  %s  (no longer on disk)\n", formatCount(m.Pruned))
	}
	fmt.Printf("  Total size:        %s\n", formatSize(s.TotalBytes))
	fmt.Println("─────────────────────────────────────────────────────")
}

// pruneAllowed decides whether this scan may remove manifest rows for files it
// did not find. A scan that could not read some folders did not see everything,
// so it must not treat the unseen files as deleted.
func pruneAllowed(want bool, st ScanStats) bool {
	if want && st.Unreadable > 0 {
		fmt.Fprintf(os.Stderr, "⚠  Not pruning: %s path(s) could not be read (first: %s), so files under them look missing.\n"+
			"   Fix the permissions or the disk and scan again; nothing was removed from the manifest.\n",
			formatCount(st.Unreadable), st.FirstBad)
		return false
	}
	return want
}

// manifestHead reads the scan_path and machine of a manifest from its first data row, without
// loading the whole file (some have a million rows).
func manifestHead(path string) (scanPath, machine string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return "", ""
	}
	row, err := r.Read()
	if err != nil {
		return "", ""
	}
	for i, h := range header {
		switch h {
		case "scan_path":
			if i < len(row) {
				scanPath = row[i]
			}
		case "machine_name":
			if i < len(row) {
				machine = row[i]
			}
		}
	}
	return
}

// findParentManifest returns the nearest already-scanned folder that contains dir on this machine,
// and the manifest file that covers it.
func findParentManifest(manifestRoot, machine, dir string) (root, file string) {
	base := filepath.Join(manifestRoot, "_Manifest")
	var cands []string
	for _, pat := range []string{filepath.Join(base, "*.csv"), filepath.Join(base, heldDirName, "*.csv")} {
		m, _ := filepath.Glob(pat)
		cands = append(cands, m...)
	}
	for _, c := range cands {
		sp, mach := manifestHead(c)
		if sp == "" || (mach != "" && mach != machine) || sp == dir {
			continue
		}
		if rel, err := filepath.Rel(sp, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if len(sp) > len(root) {
			root, file = sp, c
		}
	}
	return
}

// partialScan rescans dir (inside root) and merges the result into root's manifest.
func partialScan(dir, root, manifestFile, machine string, noCache, prune, force bool) error {
	sub, _ := filepath.Rel(root, dir)
	fmt.Printf("Scanning:  %s\n", dir)
	fmt.Printf("Part of:   %s (manifest %s)\n", root, filepath.Base(manifestFile))
	fmt.Printf("Machine:   %s\n\n", machine)
	cache := loadCache(manifestFile)
	if noCache {
		cache = make(map[string]CacheEntry)
	}
	fmt.Fprintf(os.Stderr, "\nScanning directory tree...\n")
	files, st, err := scanDirectoryRel(dir, root, cache, newPhotoIgnore(root))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return errFailed
	}
	fmt.Fprintf(os.Stderr, "\n✓ Scan complete: %d files found\n", len(files))
	forcePrune = force
	defer func() { forcePrune = false }()
	ms, err := updateManifestScoped(root, sub, files, manifestFile, machine, pruneAllowed(prune, st))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error writing manifest:", err)
		return errFailed
	}
	printScanSummary(st, ms)
	return nil
}
