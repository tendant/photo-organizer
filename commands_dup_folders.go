package main

import (
	"crypto/md5"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FolderInfo describes one physical copy of a folder found in a manifest.
type FolderInfo struct {
	Path        string
	MachineName string
	FileCount   int
	TotalSize   int64
	FolderHash  string
}

// distinctDevices counts the distinct non-removable machines a folder's copies
// live on. Removable media (SD cards, USB) don't count as a durable device, so
// a folder is only safely archivable when this is >= 2.
func distinctDevices(folders []*FolderInfo, machinesCfg map[string]string) int {
	set := make(map[string]bool)
	for _, f := range folders {
		if f.MachineName == "" {
			continue
		}
		if isRemovableSource(f.MachineName, f.Path, machinesCfg) {
			continue
		}
		set[f.MachineName] = true
	}
	return len(set)
}

func runFindDuplicateFolders(args []string) {
	// Convert short flags to long form
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-m" && i+1 < len(args) {
			processedArgs = append(processedArgs, "--machine", args[i+1])
			i++
		} else if arg == "-s" || arg == "--summary" {
			processedArgs = append(processedArgs, "--summary")
		} else if arg == "-c" || arg == "--by-count" {
			processedArgs = append(processedArgs, "--by-count")
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("dup-folders", flag.ExitOnError)
	machineFlag := fs.String("machine", "", "find duplicates in manifest from specific machine")
	summaryFlag := fs.Bool("summary", false, "show summary only")
	byCountFlag := fs.Bool("by-count", false, "sort by number of copies (most duplicated first)")
	topFlag := fs.Int("top", 0, "show only top N duplicate folders (0 = all)")
	minCopiesFlag := fs.Int("min-copies", 0, "only show folders with at least N copies (filter)")
	minDevicesFlag := fs.Int("min-devices", 0, "only show folders present on at least N distinct non-removable devices (archivable filter)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer dup-folders [options]\n\n")
		fmt.Fprintf(os.Stderr, "Find duplicate folders (entire directories with identical contents).\n")
		fmt.Fprintf(os.Stderr, "Much simpler than individual file duplicates.\n\n")
		fmt.Fprintf(os.Stderr, "Find all duplicate folders:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders\n\n")
		fmt.Fprintf(os.Stderr, "Top 20 folders by wasted space:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders --top 20 -s\n\n")
		fmt.Fprintf(os.Stderr, "Top 20 by size, only folders with 3+ copies:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders --top 20 -s --min-copies 3\n\n")
		fmt.Fprintf(os.Stderr, "Only folders safely archivable (on 2+ durable devices):\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders -s --min-devices 2\n\n")
		fmt.Fprintf(os.Stderr, "Most duplicated folders (sorted by copy count):\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders --by-count -s\n\n")
		fmt.Fprintf(os.Stderr, "Find duplicate folders on specific machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders -m ubuntu-max\n\n")
		fmt.Fprintf(os.Stderr, "Summary view:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders -s\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return
	}

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		if *machineFlag != "" && src.MachineName != *machineFlag {
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return
	}

	fmt.Fprintf(os.Stderr, "Using manifest data from %d manifest(s)...\n\n", len(sources))

	// Config lets us tell durable machines from removable media (SD/USB).
	machinesConfig := loadMachinesConfig()

	// Group files by folder, calculate folder hash
	folderHashes := make(map[string]*FolderInfo) // folder path -> info
	filesByFolder := make(map[string][]string)   // folder path -> list of file hashes

	for _, src := range sources {
		for _, row := range src.Rows {
			// Use partial hash (always computed, fast) for grouping
			// Full hash is optional and only needed for verification
			hash := row.PartialHash
			if hash == "" {
				continue
			}

			folderPath := filepath.Join(row.ScanPath, filepath.Dir(row.RelativePath))

			if _, exists := folderHashes[folderPath]; !exists {
				folderHashes[folderPath] = &FolderInfo{
					Path:        folderPath,
					MachineName: src.MachineName,
				}
			}

			folderHashes[folderPath].FileCount++
			folderHashes[folderPath].TotalSize += row.SizeBytes
			filesByFolder[folderPath] = append(filesByFolder[folderPath], hash)
		}
	}

	// Calculate folder hash from sorted file hashes
	for folderPath, hashes := range filesByFolder {
		// Sort hashes for deterministic folder hash
		sort.Strings(hashes)
		// Create folder hash from all file hashes
		h := md5.New()
		for _, hash := range hashes {
			h.Write([]byte(hash))
		}
		folderHashes[folderPath].FolderHash = fmt.Sprintf("%x", h.Sum(nil))
	}

	// Find duplicate folders (same folder hash)
	type DupFolderGroup struct {
		FolderHash  string
		Folders     []*FolderInfo
		TotalWasted int64
	}

	dupMap := make(map[string][]*FolderInfo)
	for _, folderInfo := range folderHashes {
		dupMap[folderInfo.FolderHash] = append(dupMap[folderInfo.FolderHash], folderInfo)
	}

	var duplicates []DupFolderGroup
	for hash, folders := range dupMap {
		if len(folders) > 1 {
			// Calculate wasted space: only count copies that are safely deletable
			// Count copies per machine
			copiesByMachine := make(map[string]int)
			for _, folder := range folders {
				copiesByMachine[folder.MachineName]++
			}

			// Wasted = extra copies (keep 1 copy per machine, delete the rest)
			var wasted int64
			for _, count := range copiesByMachine {
				if count > 1 {
					// Keep 1 copy, delete (count - 1) copies
					wasted += folders[0].TotalSize * int64(count-1)
				}
			}

			group := DupFolderGroup{
				FolderHash:  hash,
				Folders:     folders,
				TotalWasted: wasted,
			}
			duplicates = append(duplicates, group)
		}
	}

	if len(duplicates) == 0 {
		fmt.Printf("✓ No duplicate folders found\n")
		return
	}

	// Sort by wasted space or copy count
	if *byCountFlag {
		// Sort by number of copies (most duplicated first)
		sort.Slice(duplicates, func(i, j int) bool {
			if len(duplicates[i].Folders) != len(duplicates[j].Folders) {
				return len(duplicates[i].Folders) > len(duplicates[j].Folders)
			}
			// Tiebreaker: by wasted space
			return duplicates[i].TotalWasted > duplicates[j].TotalWasted
		})
	} else {
		// Sort by wasted space (default)
		sort.Slice(duplicates, func(i, j int) bool {
			return duplicates[i].TotalWasted > duplicates[j].TotalWasted
		})
	}

	// Filter by minimum copies and/or minimum distinct devices if specified.
	filteredDuplicates := duplicates
	if *minCopiesFlag > 0 || *minDevicesFlag > 0 {
		var filtered []DupFolderGroup
		for _, dup := range duplicates {
			if *minCopiesFlag > 0 && len(dup.Folders) < *minCopiesFlag {
				continue
			}
			if *minDevicesFlag > 0 && distinctDevices(dup.Folders, machinesConfig) < *minDevicesFlag {
				continue
			}
			filtered = append(filtered, dup)
		}
		filteredDuplicates = filtered
	}

	// Limit results if --top is specified
	displayDuplicates := filteredDuplicates
	if *topFlag > 0 && *topFlag < len(filteredDuplicates) {
		displayDuplicates = filteredDuplicates[:*topFlag]
	}

	if *summaryFlag {
		totalWasted := int64(0)
		for _, dup := range filteredDuplicates {
			totalWasted += dup.TotalWasted
		}
		fmt.Printf("Found %d duplicate folders", len(duplicates))
		if *minCopiesFlag > 0 {
			fmt.Printf(" (%d with %d+ copies)", len(filteredDuplicates), *minCopiesFlag)
		}
		if *topFlag > 0 {
			fmt.Printf(" (showing top %d)\n", *topFlag)
		} else {
			fmt.Printf("\n")
		}
		fmt.Printf("Total wasted space: %s\n\n", formatBytes(totalWasted))
		fmt.Printf("%-4s %-7s %-8s %-12s %-12s %s\n", "Rank", "Copies", "Devices", "Size/copy", "Wasted", "Archive?")
		fmt.Printf("%-4s %-7s %-8s %-12s %-12s %s\n", "----", "------", "-------", "---------", "-------", "--------")
		for i, dup := range displayDuplicates {
			devices := distinctDevices(dup.Folders, machinesConfig)
			archive := "no (1 device)"
			if devices >= 2 {
				archive = "yes"
			}
			fmt.Printf("%-4d %-7d %-8d %-12s %-12s %s\n",
				i+1, len(dup.Folders), devices, formatBytes(dup.Folders[0].TotalSize),
				formatBytes(dup.TotalWasted), archive)
		}
		fmt.Printf("\n\"Devices\" counts distinct non-removable machines; archive only when >= 2.\n")
		return
	}

	// Full output
	totalWasted := int64(0)
	for _, dup := range filteredDuplicates {
		totalWasted += dup.TotalWasted
	}
	fmt.Printf("Found %d duplicate folders", len(duplicates))
	if *minCopiesFlag > 0 {
		fmt.Printf(" (%d with %d+ copies)", len(filteredDuplicates), *minCopiesFlag)
	}
	if *topFlag > 0 {
		fmt.Printf(" (showing top %d)\n", *topFlag)
	} else {
		fmt.Printf("\n")
	}
	fmt.Printf("Total wasted space: %s\n\n", formatBytes(totalWasted))

	for i, dup := range displayDuplicates {
		devices := distinctDevices(dup.Folders, machinesConfig)
		verdict := "KEEP — only on 1 durable device, archiving would lose redundancy"
		if devices >= 2 {
			verdict = fmt.Sprintf("ARCHIVABLE — on %d durable devices, one copy can be archived", devices)
		}
		fmt.Printf("Duplicate Folder Group %d: %d copies (%s each, %s wasted) — %s\n",
			i+1, len(dup.Folders), formatBytes(dup.Folders[0].TotalSize),
			formatBytes(dup.TotalWasted), verdict)
		// Sort folders by machine first, then by path for consistent output
		sortedFolders := make([]*FolderInfo, len(dup.Folders))
		copy(sortedFolders, dup.Folders)
		sort.Slice(sortedFolders, func(a, b int) bool {
			// Sort by machine name first
			if sortedFolders[a].MachineName != sortedFolders[b].MachineName {
				return sortedFolders[a].MachineName < sortedFolders[b].MachineName
			}
			// Then by path within same machine
			return sortedFolders[a].Path < sortedFolders[b].Path
		})

		// Group folders by machine for display
		foldersByMachine := make(map[string][]*FolderInfo)
		machineOrder := []string{}
		for _, folder := range sortedFolders {
			machine := folder.MachineName
			if machine == "" {
				machine = "unknown"
			}
			if _, exists := foldersByMachine[machine]; !exists {
				machineOrder = append(machineOrder, machine)
			}
			foldersByMachine[machine] = append(foldersByMachine[machine], folder)
		}

		// Print folders grouped by machine
		for _, machine := range machineOrder {
			folders := foldersByMachine[machine]
			fmt.Printf("  [%s] - %d copies\n", machine, len(folders))
			for _, folder := range folders {
				fmt.Printf("    - %s (%d files)\n", folder.Path, folder.FileCount)
			}
			fmt.Printf("\n")
		}
	}
}
