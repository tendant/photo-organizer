package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func runStorageStatus(args []string) error {
	// Load all manifests
	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Loading manifests...\n\n")

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No valid manifests found\n")
		return nil
	}

	// Build hash index for deduplication analysis
	idx := buildHashIndex(sources)
	duplicates := findDuplicates(sources, idx)
	uniqueByMachine := findUnique(sources, idx)

	// Group by machine, then by device/mount point
	type DeviceStats struct {
		MountPoint  string
		ScanPaths   []string
		TotalFiles  int
		TotalBytes  int64
		UniqueFiles int
		UniqueBytes int64
		DupFiles    int
		DupBytes    int64
		BackedFiles int
		BackedBytes int64
	}

	type MachineStats struct {
		Machine string
		Devices map[string]*DeviceStats
	}

	machineStats := make(map[string]*MachineStats)

	// Process each source
	for i, src := range sources {
		if _, ok := machineStats[src.MachineName]; !ok {
			machineStats[src.MachineName] = &MachineStats{
				Machine: src.MachineName,
				Devices: make(map[string]*DeviceStats),
			}
		}

		// Auto-detect mount point from scan path
		mountPoint := detectMountPoint(src.ScanPath)
		if _, ok := machineStats[src.MachineName].Devices[mountPoint]; !ok {
			machineStats[src.MachineName].Devices[mountPoint] = &DeviceStats{
				MountPoint: mountPoint,
				ScanPaths:  []string{},
			}
		}

		device := machineStats[src.MachineName].Devices[mountPoint]
		device.ScanPaths = append(device.ScanPaths, src.ScanPath)

		// Calculate stats for this source
		totalFiles := len(src.Rows)
		var totalBytes int64
		for _, row := range src.Rows {
			totalBytes += row.SizeBytes
		}

		device.TotalFiles += totalFiles
		device.TotalBytes += totalBytes

		// Count unique files for this machine
		uniqueRows := uniqueByMachine[src.MachineName]
		uniqueCount := 0
		var uniqueBytes int64
		for _, row := range uniqueRows {
			if rowBelongsToSource(row, src, i) {
				uniqueCount++
				uniqueBytes += row.SizeBytes
			}
		}

		device.UniqueFiles += uniqueCount
		device.UniqueBytes += uniqueBytes

		// Count duplicated files
		device.DupFiles = device.TotalFiles - device.UniqueFiles
		device.DupBytes = device.TotalBytes - device.UniqueBytes
	}

	// Print report
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("STORAGE STATUS REPORT\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")

	var totalGlobalFiles int
	var totalGlobalBytes int64

	// Sort machines for consistent output
	var machines []string
	for m := range machineStats {
		machines = append(machines, m)
	}
	sort.Strings(machines)

	// Load machines config for enhanced labels
	machinesConfig := loadMachinesConfig()

	for _, machine := range machines {
		stats := machineStats[machine]
		config := machinesConfig[machine]

		// Any scan path of this machine is representative for the removable check.
		var machineScanPath string
		for _, dev := range stats.Devices {
			if len(dev.ScanPaths) > 0 {
				machineScanPath = dev.ScanPaths[0]
				break
			}
		}

		// Print machine with type info
		if isRemovableSource(machine, machineScanPath, machinesConfig) {
			deviceInfo := extractDeviceInfo(config)
			fmt.Printf("📷 REMOVABLE: %s\n", machine)
			if deviceInfo != "" {
				fmt.Printf("   (%s)\n", deviceInfo)
			}
		} else if config != "" && !strings.Contains(config, "[") {
			fmt.Printf("🌐 REMOTE: %s (%s)\n", machine, config)
		} else {
			fmt.Printf("💻 LOCAL: %s\n", machine)
			// Show disk space for local machine
			if diskUsage := getDiskUsageLocal(); diskUsage != "" {
				fmt.Printf("   %s\n", diskUsage)
			}
		}

		// Sort devices
		var devices []string
		for d := range stats.Devices {
			devices = append(devices, d)
		}
		sort.Strings(devices)

		var machineTotal int64
		var machineUnique int64

		for _, device := range devices {
			dev := stats.Devices[device]
			machineTotal += dev.TotalBytes
			machineUnique += dev.UniqueBytes

			coverage := 0.0
			if dev.TotalFiles > 0 {
				coverage = float64(dev.TotalFiles-dev.UniqueFiles) / float64(dev.TotalFiles) * 100
			}

			fmt.Printf("  Device: %s\n", dev.MountPoint)
			fmt.Printf("    Paths:        %v\n", dev.ScanPaths)
			fmt.Printf("    Total:        %s files (%s)\n", formatCount(dev.TotalFiles), formatSize(dev.TotalBytes))
			fmt.Printf("    Unique:       %s files (%s) - only on this machine\n", formatCount(dev.UniqueFiles), formatSize(dev.UniqueBytes))
			fmt.Printf("    Duplicated:   %s files (%s) - %.1f%% backed elsewhere\n", formatCount(dev.DupFiles), formatSize(dev.DupBytes), coverage)
			fmt.Printf("\n")
		}

		totalGlobalFiles += len(stats.Devices)
		totalGlobalBytes += machineTotal

		fmt.Printf("\n")
	}

	// Print summary
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("SUMMARY\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")
	fmt.Printf("Machines scanned: %d\n", len(machines))
	fmt.Printf("Total files:      %s (%s)\n", formatCount(len(idx)), formatSize(totalGlobalBytes))
	fmt.Printf("Duplicated files: %s\n", formatCount(len(duplicates)))
	fmt.Printf("\n")
	return nil
}

// machineDisplayLabel prefixes a machine name with its device-type icon:
// 📷 removable media, 🌐 remote (SSH target), 💻 local.
func machineDisplayLabel(machine, scanPath string, cfg map[string]string) string {
	entry := cfg[machine]
	switch {
	case isRemovableSource(machine, scanPath, cfg):
		return "📷 " + machine
	case entry != "" && !strings.Contains(entry, "["):
		return "🌐 " + machine
	default:
		return "💻 " + machine
	}
}

// runStoragePlan recommends backup priorities and storage planning
func runStoragePlan(args []string) error {
	// Load all manifests
	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return nil
	}

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No valid manifests found\n")
		return nil
	}

	// Build hash index for deduplication analysis
	idx := buildHashIndex(sources)
	uniqueByMachine := findUnique(sources, idx)

	// Calculate unique files per machine
	type MachineInfo struct {
		Machine     string
		UniqueFiles int
		UniqueBytes int64
		TotalFiles  int
		TotalBytes  int64
	}

	machineMap := make(map[string]*MachineInfo)
	machineScanPath := make(map[string]string) // representative path for the removable check
	for _, src := range sources {
		if _, ok := machineMap[src.MachineName]; !ok {
			machineMap[src.MachineName] = &MachineInfo{Machine: src.MachineName}
			machineScanPath[src.MachineName] = src.ScanPath
		}

		m := machineMap[src.MachineName]
		for _, row := range src.Rows {
			m.TotalFiles++
			m.TotalBytes += row.SizeBytes
		}
	}

	// Count unique files per machine
	for machine, rows := range uniqueByMachine {
		if m, ok := machineMap[machine]; ok {
			for _, row := range rows {
				m.UniqueFiles++
				m.UniqueBytes += row.SizeBytes
			}
		}
	}

	// Sort machines by unique bytes (highest first)
	var machines []*MachineInfo
	for _, m := range machineMap {
		machines = append(machines, m)
	}
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].UniqueBytes > machines[j].UniqueBytes
	})

	// Load machines config
	machinesConfig := loadMachinesConfig()

	// Print report
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("STORAGE PLANNING REPORT\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")

	// 1. Priority backups - at-risk files (unique to single machine)
	fmt.Printf("1️⃣  PRIORITY BACKUPS (At-Risk Files)\n")
	fmt.Printf("   Back up these FIRST - they exist on only one machine:\n\n")

	totalAtRisk := int64(0)
	for _, m := range machines {
		if m.UniqueBytes > 0 {
			coverage := 100.0 - (float64(m.UniqueBytes) / float64(m.TotalBytes) * 100)
			machineLabel := machineDisplayLabel(m.Machine, machineScanPath[m.Machine], machinesConfig)

			fmt.Printf("   ⚠️  %s\n", machineLabel)
			fmt.Printf("       Unique:  %s (%s)\n", formatCount(m.UniqueFiles), formatSize(m.UniqueBytes))
			fmt.Printf("       Backed:  %.1f%% (on other machines)\n\n", coverage)
			totalAtRisk += m.UniqueBytes
		}
	}

	if totalAtRisk == 0 {
		fmt.Printf("   ✓ Everything is backed up!\n\n")
	} else {
		fmt.Printf("   Total at-risk: %s\n\n", formatSize(totalAtRisk))
	}

	// 2. Storage gaps - show capacity differences
	fmt.Printf("2️⃣  STORAGE GAPS (Backup Capacity)\n")
	fmt.Printf("   Which machines can back up which:\n\n")

	// Calculate available space on backup targets (assume duplicated files can be deleted)
	for _, m := range machines {
		if m.UniqueFiles == 0 {
			continue
		}
		duplicatedBytes := m.TotalBytes - m.UniqueBytes
		fmt.Printf("   📤 %s has %s unique files\n", m.Machine, formatSize(m.UniqueBytes))
		fmt.Printf("      Needs backup target with ≥ %s free space\n", formatSize(m.UniqueBytes))
		fmt.Printf("      (can free up %s by deleting duplicates locally)\n\n", formatSize(duplicatedBytes))
	}

	// 3. Backup targets calculation
	fmt.Printf("3️⃣  BACKUP TARGETS\n")
	fmt.Printf("   Recommended backup order:\n\n")

	for i, m := range machines {
		if m.UniqueBytes == 0 {
			continue
		}
		fmt.Printf("   %d. Back up %s to a machine with ≥ %s free space\n",
			i+1, m.Machine, formatSize(m.UniqueBytes))
		fmt.Printf("      %s files, %.1f%% unprotected\n",
			formatCount(m.UniqueFiles),
			float64(m.UniqueBytes)/float64(m.TotalBytes)*100)
		fmt.Printf("\n")
	}

	// 4. Cleanup opportunities
	fmt.Printf("4️⃣  CLEANUP OPPORTUNITIES\n")
	fmt.Printf("   Free up space by deleting backed-up duplicates:\n\n")

	totalCleanup := int64(0)
	for _, m := range machines {
		duplicatedBytes := m.TotalBytes - m.UniqueBytes
		if duplicatedBytes > 0 {
			machineLabel := machineDisplayLabel(m.Machine, machineScanPath[m.Machine], machinesConfig)

			coverage := float64(m.TotalFiles-m.UniqueFiles) / float64(m.TotalFiles) * 100
			fmt.Printf("   🧹 %s\n", machineLabel)
			fmt.Printf("       Can delete: %s (%s backed up elsewhere)\n", formatSize(duplicatedBytes), formatCount(m.TotalFiles-m.UniqueFiles))
			fmt.Printf("       Coverage:   %.1f%% backed elsewhere\n\n", coverage)
			totalCleanup += duplicatedBytes
		}
	}

	if totalCleanup > 0 {
		fmt.Printf("   Total freeable space: %s\n\n", formatSize(totalCleanup))
	}

	// Summary
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("SUMMARY\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")
	fmt.Printf("Files at-risk:     %s\n", formatSize(totalAtRisk))
	fmt.Printf("Freeable space:    %s (duplicates)\n", formatSize(totalCleanup))

	// Calculate total bytes
	var totalCapacity int64
	for _, m := range machineMap {
		totalCapacity += m.TotalBytes
	}
	fmt.Printf("Total capacity:    %s across %d machines\n", formatSize(totalCapacity), len(machines))
	fmt.Printf("\n")
	return nil
}

// runCheckBackupStatus checks if a folder is backed up and shows per-machine coverage
func runCheckBackupStatus(args []string) error {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "\n✓ CHECK BACKUP STATUS\n\n")
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer check-backup <folder>\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer check-backup ~/Photos\n\n")
		fmt.Fprintf(os.Stderr, "Shows backup coverage per machine and at-risk files.\n")
		return errFailed
	}

	folderPath := args[0]

	// Resolve to absolute path
	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot resolve folder path: %v\n", err)
		return errFailed
	}

	// Load all manifests
	manifestRoot := filepath.Join(userHomeDir(), "manifests")
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found\n")
		return nil
	}

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		sources = append(sources, src)
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No valid manifests found\n")
		return nil
	}

	// Find local manifest for this folder and build hash index
	var localRows []ManifestRow
	hashIndex := make(map[string][]ManifestRow) // hash|size -> rows across all machines

	for _, src := range sources {
		if src.ScanPath == absPath {
			// Exact match
			localRows = src.Rows
		} else if strings.HasPrefix(absPath, src.ScanPath+string(filepath.Separator)) {
			// Check if absPath is a subfolder of src.ScanPath
			// Filter rows to only include files in this subfolder
			for _, row := range src.Rows {
				filePath := filepath.Join(src.ScanPath, row.RelativePath)
				if strings.HasPrefix(filePath, absPath+string(filepath.Separator)) || filePath == absPath {
					localRows = append(localRows, row)
				}
			}
		}
		// Build hash index for all files across all machines
		for _, row := range src.Rows {
			if row.PartialHash == "" {
				continue
			}
			key := indexKey(row.PartialHash, row.SizeBytes)
			hashIndex[key] = append(hashIndex[key], row)
		}
	}

	// Print header
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n")
	fmt.Printf("BACKUP COVERAGE: %s\n", absPath)
	fmt.Printf("═══════════════════════════════════════════════════════════════════\n\n")

	if len(localRows) == 0 {
		fmt.Printf("❌ NOT SCANNED\n")
		fmt.Printf("   This folder has not been scanned yet.\n")
		fmt.Printf("   Run: photo-organizer scan %s\n", folderPath)
		return nil
	}

	// Get current machine to exclude it from backup count
	currentMachine := resolveMachineID("")

	// Count files per machine (separate backed up vs removable vs local)
	machinesCfg := loadMachinesConfig()
	machineCount := make(map[string]int)     // remote machines with proper backups
	machineBytes := make(map[string]int64)   // remote machines with proper backups
	removableCount := make(map[string]int)   // removable media
	removableBytes := make(map[string]int64) // removable media

	// Track folder-level stats
	type FolderStats struct {
		Path          string
		BackedUpFiles int
		AtRiskFiles   int
		TotalFiles    int
	}
	folderStats := make(map[string]*FolderStats)

	atRiskFiles := 0
	atRiskBytes := int64(0)

	for _, localRow := range localRows {
		// Always use PartialHash for key consistency (even if FullHash exists)
		if localRow.PartialHash == "" {
			continue
		}

		// Get folder from path
		folderPath := filepath.Dir(localRow.RelativePath)
		if folderPath == "." {
			folderPath = "/"
		}
		if _, ok := folderStats[folderPath]; !ok {
			folderStats[folderPath] = &FolderStats{Path: folderPath}
		}
		folderStats[folderPath].TotalFiles++

		// Find which machines have this file (excluding current machine and removable)
		machinesHaveFile := make(map[string]bool)
		removableHaveFile := make(map[string]bool)
		key := indexKey(localRow.PartialHash, localRow.SizeBytes)
		if len(hashIndex[key]) > 0 {
			for _, remoteRow := range hashIndex[key] {
				if isRemovableSource(remoteRow.MachineName, remoteRow.ScanPath, machinesCfg) {
					removableHaveFile[remoteRow.MachineName] = true
				} else if remoteRow.MachineName != currentMachine {
					// Only count OTHER machines as backups (not current machine)
					machinesHaveFile[remoteRow.MachineName] = true
				}
			}
		}

		// Count coverage (only remote non-removable machines count as "backed up")
		if len(machinesHaveFile) == 0 {
			// File not found on remote backup machines
			atRiskFiles++
			atRiskBytes += localRow.SizeBytes
			folderStats[folderPath].AtRiskFiles++
		} else {
			folderStats[folderPath].BackedUpFiles++
			for machine := range machinesHaveFile {
				machineCount[machine]++
				machineBytes[machine] += localRow.SizeBytes
			}
		}

		// Track removable media separately
		for machine := range removableHaveFile {
			removableCount[machine]++
			removableBytes[machine] += localRow.SizeBytes
		}
	}

	totalFiles := len(localRows)
	totalBytes := int64(0)
	for _, row := range localRows {
		totalBytes += row.SizeBytes
	}

	fmt.Printf("Total:   %s (%s)\n\n", formatCount(totalFiles), formatSize(totalBytes))

	// Sort machines by name for consistent output
	var machines []string
	for m := range machineCount {
		machines = append(machines, m)
	}
	sort.Strings(machines)

	// Show per-machine coverage (only proper backups)
	if len(machines) > 0 {
		fmt.Printf("BACKED UP on reliable machines:\n\n")
		for _, machine := range machines {
			count := machineCount[machine]
			bytes := machineBytes[machine]
			coverage := float64(count) / float64(totalFiles) * 100

			cfg := machinesCfg[machine]
			machineLabel := machine
			if cfg != "" && !strings.Contains(cfg, "[") {
				machineLabel = "🌐 " + machine
			} else {
				machineLabel = "💻 " + machine
			}

			fmt.Printf("  %s\n", machineLabel)
			fmt.Printf("    %s files (%.1f%%) | %s\n", formatCount(count), coverage, formatSize(bytes))
		}
		fmt.Printf("\n")
	}

	// Show removable media separately (doesn't count toward backup status)
	var removableMedia []string
	for m := range removableCount {
		removableMedia = append(removableMedia, m)
	}
	sort.Strings(removableMedia)

	if len(removableMedia) > 0 {
		fmt.Printf("Also found on removable media (not counted as backup):\n\n")
		for _, machine := range removableMedia {
			count := removableCount[machine]
			bytes := removableBytes[machine]
			coverage := float64(count) / float64(totalFiles) * 100

			fmt.Printf("  📷 %s\n", machine)
			fmt.Printf("    %s files (%.1f%%) | %s\n", formatCount(count), coverage, formatSize(bytes))
		}
		fmt.Printf("\n")
	}

	// Show at-risk files
	if atRiskFiles > 0 {
		atRiskCoverage := float64(atRiskFiles) / float64(totalFiles) * 100
		fmt.Printf("⚠️  NOT ON PRIMARY BACKUPS: %s files (%.1f%%) | %s\n", formatCount(atRiskFiles), atRiskCoverage, formatSize(atRiskBytes))
		if len(machines) > 0 {
			machineList := strings.Join(machines, ", ")
			fmt.Printf("   Not found on configured backup machines (%s)\n", machineList)
		} else {
			fmt.Printf("   Not found on any configured backup machines\n")
		}
		fmt.Printf("   May exist on other machines or need explicit backup\n")
		fmt.Printf("   Run: photo-organizer backup %s --dest <target>\n", folderPath)
	} else {
		fmt.Printf("✓ ALL FILES BACKED UP\n")
		fmt.Printf("   Every file has copies on reliable machines\n")
	}
	fmt.Printf("\n")

	// Show top 3 folders by at-risk files or by size
	type FolderRank struct {
		Path     string
		BackedUp int
		AtRisk   int
		Total    int
		Coverage float64
	}

	var folderRanks []FolderRank
	for _, stats := range folderStats {
		coverage := 0.0
		if stats.TotalFiles > 0 {
			coverage = float64(stats.BackedUpFiles) / float64(stats.TotalFiles) * 100
		}
		folderRanks = append(folderRanks, FolderRank{
			Path:     stats.Path,
			BackedUp: stats.BackedUpFiles,
			AtRisk:   stats.AtRiskFiles,
			Total:    stats.TotalFiles,
			Coverage: coverage,
		})
	}

	// Sort by at-risk files (descending)
	sort.Slice(folderRanks, func(i, j int) bool {
		return folderRanks[i].AtRisk > folderRanks[j].AtRisk
	})

	// Show top 3 folders with at-risk files
	topAtRisk := folderRanks
	if len(topAtRisk) > 3 {
		topAtRisk = topAtRisk[:3]
	}

	if len(topAtRisk) > 0 && topAtRisk[0].AtRisk > 0 {
		fmt.Printf("TOP FOLDERS TO BACK UP:\n\n")
		for i, f := range topAtRisk {
			if f.AtRisk > 0 {
				fmt.Printf("  %d. %s\n", i+1, f.Path)
				fmt.Printf("     At-risk: %s files | Coverage: %.1f%%\n", formatCount(f.AtRisk), f.Coverage)
			}
		}
		fmt.Printf("\n")
	}

	return nil
}

// detectMountPoint returns the likely mount point for a given path
// by walking up the directory tree
func detectMountPoint(path string) string {
	// For now, use a simple heuristic: the first component after root
	// This works for most cases like /data, /tankm1, /mnt, /Volumes
	path = filepath.Clean(path)
	parts := strings.Split(path, string(filepath.Separator))

	if len(parts) <= 2 {
		return path
	}

	// Return root + first component: /data, /tankm1, /mnt, etc.
	if parts[0] == "" {
		// Unix-style absolute path
		return string(filepath.Separator) + parts[1]
	}

	// Windows-style path
	return parts[0]
}

// rowBelongsToSource checks if a manifest row belongs to a specific source
func rowBelongsToSource(row ManifestRow, src ManifestSource, srcIdx int) bool {
	return row.ScanPath == src.ScanPath && row.MachineName == src.MachineName
}

// getDiskUsageLocal returns disk usage info for the local machine
func getDiskUsageLocal() string {
	// Use 'df' to get disk usage for the home directory
	homeDir := userHomeDir()
	cmd := exec.Command("df", "-h", homeDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		return ""
	}

	// Parse df output: Filesystem Size Used Avail Use% Mounted on
	fields := strings.Fields(lines[1])
	if len(fields) >= 4 {
		available := fields[3]
		total := fields[1]
		used := fields[2]
		return fmt.Sprintf("Disk: %s used / %s total (%s available)", used, total, available)
	}
	return ""
}
