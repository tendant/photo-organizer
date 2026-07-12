package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func runManifests(args []string) error {
	// Parse flags
	showStalled := false
	doCleanup := false
	removeFolder := ""

	for i := 0; i < len(args); i++ {
		if args[i] == "--stalled" {
			showStalled = true
		} else if args[i] == "--cleanup" {
			doCleanup = true
		} else if args[i] == "--remove" && i+1 < len(args) {
			removeFolder = args[i+1]
			i++
		}
	}

	// Handle --remove flag
	if removeFolder != "" {
		absPath, _ := filepath.Abs(removeFolder)
		manifestRoot := defaultManifestRoot()
		manifestDir := filepath.Join(manifestRoot, "_Manifest")
		matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

		removed := 0
		for _, path := range matches {
			src, err := readManifest(path)
			if err == nil && src.ScanPath == absPath {
				if err := os.Remove(path); err == nil {
					fmt.Printf("✓ Removed manifest: %s\n", filepath.Base(path))
					removed++
				}
			}
		}

		if removed == 0 {
			fmt.Fprintf(os.Stderr, "No manifest found for: %s\n", absPath)
		} else {
			fmt.Printf("\n✓ Removed %d manifest(s)\n", removed)
		}
		return nil
	}

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		return nil
	}

	var sources []ManifestSource
	currentMachineID := machineID()

	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		// Mark as local or remote
		markManifestOrigin(&src, currentMachineID)
		// Check if stalled (source path no longer exists)
		if _, err := os.Stat(src.ScanPath); err != nil {
			src.IsStale = true
		}
		sources = append(sources, src)
	}

	// Handle --stalled flag
	if showStalled {
		var stalledSources []ManifestSource
		for _, src := range sources {
			if src.IsStale {
				stalledSources = append(stalledSources, src)
			}
		}

		if len(stalledSources) == 0 {
			fmt.Fprintf(os.Stderr, "✅ No stalled manifests found\n")
			return nil
		}

		fmt.Fprintf(os.Stderr, "📋 STALLED MANIFESTS (source paths no longer exist)\n\n")
		for _, src := range stalledSources {
			fmt.Printf("  %s\n", filepath.Base(src.FilePath))
			fmt.Printf("    Scan path: %s (missing)\n", src.ScanPath)
			fmt.Printf("    Machine:   %s\n", src.MachineName)
			fmt.Printf("    Files:     %d\n\n", len(src.Rows))
		}
		return nil
	}

	// Handle --cleanup flag
	if doCleanup {
		var stalledSources []ManifestSource
		var stalledPaths []string
		for i, src := range sources {
			if src.IsStale {
				stalledSources = append(stalledSources, src)
				stalledPaths = append(stalledPaths, matches[i])
			}
		}

		if len(stalledSources) == 0 {
			fmt.Fprintf(os.Stderr, "✅ No stalled manifests to clean up\n")
			return nil
		}

		fmt.Fprintf(os.Stderr, "Found %d stalled manifest(s):\n\n", len(stalledSources))
		for _, src := range stalledSources {
			fmt.Fprintf(os.Stderr, "  %s (%s - missing)\n", filepath.Base(src.FilePath), src.ScanPath)
		}
		fmt.Fprintf(os.Stderr, "\n")

		if !confirmPrompt("Remove stalled manifests?") {
			fmt.Fprintf(os.Stderr, "Cancelled.\n")
			return nil
		}

		removed := 0
		for _, path := range stalledPaths {
			if err := os.Remove(path); err == nil {
				fmt.Printf("✓ Removed: %s\n", filepath.Base(path))
				removed++
			}
		}
		fmt.Printf("\n✓ Removed %d stalled manifest(s)\n", removed)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Listing all manifests and their origin...\n\n")

	// Sort by origin (local first), then by machine name, then scan path
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].IsLocal != sources[j].IsLocal {
			return sources[i].IsLocal // local first
		}
		if sources[i].MachineName != sources[j].MachineName {
			return sources[i].MachineName < sources[j].MachineName
		}
		return sources[i].ScanPath < sources[j].ScanPath
	})

	// Separate into local, removable, and remote (excluding empty)
	machinesCfg := loadMachinesConfig()
	var localSources, removableSources, remoteSources, emptySources []ManifestSource
	for _, src := range sources {
		if len(src.Rows) == 0 {
			emptySources = append(emptySources, src)
		} else if isRemovableSource(src.MachineName, src.ScanPath, machinesCfg) {
			// Check removable path first (USB, SD cards, external drives)
			removableSources = append(removableSources, src)
		} else if src.IsLocal {
			// Local permanent storage
			localSources = append(localSources, src)
		} else {
			// Remote SSH machines
			remoteSources = append(remoteSources, src)
		}
	}

	// Display local manifests
	if len(localSources) > 0 {
		fmt.Fprintf(os.Stderr, "LOCAL MANIFESTS\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(localSources, "💻 lcl")
	}

	// Display removable manifests
	if len(removableSources) > 0 {
		if len(localSources) > 0 {
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "REMOVABLE MEDIA (USB, SD cards, etc.)\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(removableSources, "💾 rem")
	}

	// Display remote manifests
	if len(remoteSources) > 0 {
		if len(localSources) > 0 || len(removableSources) > 0 {
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "REMOTE MACHINES\n")
		fmt.Fprintf(os.Stderr, "Origin  Machine              Scan Path                      Files  Status              Last Scanned\n")
		fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────────────────────────────────────────────────\n")
		displayManifestTable(remoteSources, "📦 rmt")
	}

	// Display empty manifests separately
	if len(emptySources) > 0 {
		fmt.Fprintf(os.Stderr, "\n═══════════════════════════════════════════════════════════════════\n")
		fmt.Fprintf(os.Stderr, "EMPTY MANIFESTS (0 files - need investigation)\n")
		fmt.Fprintf(os.Stderr, "═══════════════════════════════════════════════════════════════════\n\n")

		for i, src := range emptySources {
			// Extract path from filename
			scanPath := src.ScanPath
			if scanPath == "" {
				filename := filepath.Base(src.FilePath)
				pathEncoded := strings.TrimSuffix(filename, ".csv")
				pathEncoded = strings.TrimPrefix(pathEncoded, "photo_manifest_")
				if idx := strings.Index(pathEncoded, src.MachineName); idx == 0 && len(pathEncoded) > len(src.MachineName) {
					pathEncoded = pathEncoded[len(src.MachineName)+1:]
					scanPath = "/" + strings.ReplaceAll(pathEncoded, "_", "/")
				}
			}

			originMark := "📦 rmt"
			if src.IsLocal {
				originMark = "💻 lcl"
			}
			if isRemovableSource(src.MachineName, src.ScanPath, machinesCfg) {
				originMark = "💾 rem"
			}

			fmt.Fprintf(os.Stderr, "%d. %s %s @ %s\n", i+1, originMark, src.MachineName, scanPath)
			fmt.Fprintf(os.Stderr, "   File: %s\n\n", src.FilePath)
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	localCount := 0
	removableCount := 0
	remoteCount := 0
	emptyCount := 0
	for _, src := range sources {
		if len(src.Rows) == 0 {
			emptyCount++
		} else if isRemovableSource(src.MachineName, src.ScanPath, machinesCfg) {
			removableCount++
		} else if src.IsLocal {
			localCount++
		} else {
			remoteCount++
		}
	}
	fmt.Fprintf(os.Stderr, "Summary: %d local, %d removable, %d remote, %d empty\n", localCount, removableCount, remoteCount, emptyCount)
	fmt.Fprintf(os.Stderr, "💻 = Local machine       💾 = Removable media       📦 = Remote machines\n")
	return nil
}

// Helper function to display manifest table
func displayManifestTable(sources []ManifestSource, originMark string) {
	for _, src := range sources {

		// Truncate long paths
		scanPath := src.ScanPath
		if len(scanPath) > 28 {
			scanPath = "..." + scanPath[len(scanPath)-25:]
		}

		// Calculate freshness status
		lastScanDate, _ := time.Parse("2006-01-02 15:04:05", src.LastScanned)
		daysSince := time.Since(lastScanDate).Hours() / 24

		var freshStatus string
		if daysSince < 1 {
			freshStatus = "✓ Fresh"
		} else if daysSince < 7 {
			freshStatus = fmt.Sprintf("⚡ %d day%s", int(daysSince), map[bool]string{true: "s", false: ""}[daysSince != 1])
		} else if daysSince < 30 {
			freshStatus = fmt.Sprintf("⚠  %d wks", int(daysSince/7))
		} else {
			freshStatus = fmt.Sprintf("🔴 %d mo", int(daysSince/30))
		}

		fmt.Fprintf(os.Stderr, "%s  %-20s %-28s %6d  %-17s %s\n",
			originMark,
			src.MachineName,
			scanPath,
			len(src.Rows),
			freshStatus,
			src.LastScanned,
		)
	}
}
