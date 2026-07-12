package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// machineInfo holds metadata about a discovered machine
type machineInfo struct {
	name      string
	scanPaths map[string]bool
	lastScan  string
	fileCount int
	totalSize int64
}

func runMachines(args []string) {
	// Parse flags
	writeConf := false
	for _, arg := range args {
		if arg == "--write-conf" || arg == "--generate-conf" {
			writeConf = true
		}
	}

	// Load all manifests from the default manifest directory.
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")

	allCSVs, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))
	if len(allCSVs) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		os.Exit(1)
	}

	// Collect all machines and their metadata.
	machines := make(map[string]*machineInfo)

	for _, csvPath := range allCSVs {
		src, err := readManifest(csvPath)
		if err != nil || src.MachineName == "" {
			continue
		}

		if machines[src.MachineName] == nil {
			machines[src.MachineName] = &machineInfo{
				name:      src.MachineName,
				scanPaths: make(map[string]bool),
			}
		}
		m := machines[src.MachineName]

		if src.ScanPath != "" {
			m.scanPaths[src.ScanPath] = true
		}
		m.fileCount += len(src.Rows)
		if src.LastScanned > m.lastScan {
			m.lastScan = src.LastScanned
		}
		for _, row := range src.Rows {
			m.totalSize += row.SizeBytes
		}
	}

	// Load machine config for SSH targets.
	cfg := loadMachinesConfig()

	// Sort machines by name and display.
	var names []string
	for name := range machines {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Println("No machines found in manifests.")
		return
	}

	// If --write-conf flag, generate and write machines.conf
	if writeConf {
		confPath := machinesConfFile()
		confContent := generateMachinesConfWithPaths(names, machines, cfg)

		// Check if file exists
		if _, err := os.Stat(confPath); err == nil {
			fmt.Fprintf(os.Stderr, "⚠  %s already exists\n", confPath)
			fmt.Fprintf(os.Stderr, "Backup: cp %s %s.backup\n", confPath, confPath)
			fmt.Fprintf(os.Stderr, "Then overwrite with:\n")
			fmt.Fprintf(os.Stderr, "  cat > %s << 'EOF'\n%sEOF\n", confPath, confContent)
			return
		}

		if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", confPath, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "✓ Generated machines.conf at %s\n", confPath)
		fmt.Fprintf(os.Stderr, "\nEdit the file to add SSH targets for remote machines:\n")
		fmt.Fprintf(os.Stderr, "  nano %s\n", confPath)
		fmt.Fprintf(os.Stderr, "\nFormat: machine-name=user@host:/path\n")
		fmt.Fprintf(os.Stderr, "Example: nas-backup=admin@192.168.1.100:/mnt/backup\n")
		fmt.Fprintf(os.Stderr, "\nNote: Removable media (marked with [removable]) don't need SSH targets.\n")
		return
	}

	// Get current machine ID to mark it in the list
	currentMachine := machineID()

	fmt.Println("Machines in manifests:")
	fmt.Println()
	for _, name := range names {
		m := machines[name]
		sshTarget := cfg[name]
		if sshTarget == "" {
			sshTarget = "—"
		}

		// Collect and sort scan paths.
		var paths []string
		for p := range m.scanPaths {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		// Mark current machine
		marker := ""
		if name == currentMachine {
			marker = " (this machine)"
		}

		fmt.Printf("  %s%s\n", name, marker)
		fmt.Printf("    SSH: %s\n", sshTarget)
		fmt.Printf("    Last scanned: %s\n", m.lastScan)
		fmt.Printf("    Files: %s  (%s)\n", formatCount(m.fileCount), formatSize(m.totalSize))
		if len(paths) > 0 {
			fmt.Printf("    Scan paths:\n")
			for _, p := range paths {
				fmt.Printf("      %s\n", p)
			}
		}
		fmt.Println()
	}

	fmt.Printf("Use: photo-organizer dups to find duplicate files across machines\n")
	fmt.Printf("     photo-organizer dup-folders to find duplicate folders for cleanup review\n")
}

// =============================================================================
// Machines Config (~/manifests/machines.conf)
// =============================================================================

// isRemovableMedia asks the OS whether the filesystem at path is removable.
// On macOS: uses diskutil info to check "Removable Media: Removable" or "Ejectable: Yes".
// isRemovableSource reports whether a manifest source lives on removable media
// (SD card, USB drive) rather than a durable device.
//
// The machines.conf entry is authoritative when present: a "[removable]" tag
// means removable, and any other entry (an SSH target or "[local]") means
// durable. Machines with no config entry fall back to the scan-path heuristic.
//
// This is the analysis-side predicate for classifying manifests from any
// machine. It is distinct from isRemovableMedia below, which queries the OS
// about a locally mounted path at scan time.
func isRemovableSource(machineName, scanPath string, cfg map[string]string) bool {
	if entry := cfg[machineName]; entry != "" {
		return strings.Contains(entry, "[removable]")
	}
	return isRemovablePath(scanPath)
}

// On Linux: resolves the mount device via /proc/mounts, then checks /sys/block/<dev>/removable.
// Falls back to false (no warning) if detection fails — better a missed warning than a false one.
func isRemovableMedia(path string) bool {
	switch runtime.GOOS {
	case "darwin":
		return isMacOSRemovable(path)
	case "linux":
		return isLinuxRemovable(path)
	}
	return false
}

func isMacOSRemovable(path string) bool {
	out, err := exec.Command("diskutil", "info", path).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(line, ":", 2)
		if len(fields) != 2 {
			continue
		}
		key := strings.TrimSpace(fields[0])
		val := strings.TrimSpace(fields[1])
		// "Removable Media: Removable" (not "Fixed")
		if key == "Removable Media" && val == "Removable" {
			return true
		}
		// "Ejectable: Yes"
		if key == "Ejectable" && val == "Yes" {
			return true
		}
	}
	return false
}

func isLinuxRemovable(path string) bool {
	// Find the mount device for the given path via /proc/mounts.
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}

	dev := mountDeviceForPath(data, path)
	if dev == "" {
		return false
	}
	devName := blockDeviceName(dev)

	// Check /sys/block/<dev>/removable: "1" means removable.
	removable, err := os.ReadFile("/sys/block/" + devName + "/removable")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(removable)) == "1"
}

// mountDeviceForPath returns the device backing the longest mount point (in
// /proc/mounts-formatted data) that is a prefix of path, or "" if none match.
func mountDeviceForPath(mountsData []byte, path string) string {
	bestMount, bestDev := "", ""
	for _, line := range strings.Split(string(mountsData), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev, mount := fields[0], fields[1]
		if strings.HasPrefix(path, mount) && len(mount) > len(bestMount) {
			bestMount = mount
			bestDev = dev
		}
	}
	return bestDev
}

// blockDeviceName reduces a device path to its parent block-device name by
// dropping the directory and any trailing partition number (/dev/sdb1 → sdb).
func blockDeviceName(dev string) string {
	name := filepath.Base(dev)
	for len(name) > 0 && name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
		name = name[:len(name)-1]
	}
	return name
}

// generateMachinesConfWithPaths creates machines.conf from discovered machines and their paths
func generateMachinesConfWithPaths(machineNames []string, machineInfo map[string]*machineInfo, existing map[string]string) string {
	var buf strings.Builder

	buf.WriteString("# Machine SSH Configuration\n")
	buf.WriteString("# Format: machine-name=user@host:/path\n")
	buf.WriteString("#\n")
	buf.WriteString("# Machines marked [removable] are external drives/SD cards (no SSH needed)\n")
	buf.WriteString("# Machines marked [local] are on this computer (no SSH needed)\n")
	buf.WriteString("# For other machines, add SSH target in format: user@host:/path\n")
	buf.WriteString("#\n")
	buf.WriteString("# Examples:\n")
	buf.WriteString("# nas-backup=admin@nas.local:/mnt/backup\n")
	buf.WriteString("# cloud-storage=user@cloud.example.com:/home/user/backup\n")
	buf.WriteString("#\n\n")

	for _, name := range machineNames {
		existing_target := existing[name]
		paths := machineInfo[name].scanPaths

		// Determine if this is removable media
		isRemovable := false
		var scanPath string
		for p := range paths {
			scanPath = p
			if isRemovableMedia(p) {
				isRemovable = true
				break
			}
		}

		// Build comment with scan path
		var comment string
		if isRemovable {
			comment = fmt.Sprintf(" [removable] scanned from: %s", scanPath)
		} else if scanPath != "" {
			comment = fmt.Sprintf(" [local] scanned from: %s", scanPath)
		}

		if existing_target != "" {
			// Preserve existing SSH target
			buf.WriteString(fmt.Sprintf("%s=%s  #%s\n", name, existing_target, comment))
		} else {
			// Add placeholder for new machines
			if isRemovable {
				// Don't need SSH for removable media
				buf.WriteString(fmt.Sprintf("# %s=%s\n", name, comment))
			} else {
				buf.WriteString(fmt.Sprintf("# %s=%s\n", name, comment))
			}
		}
	}

	return buf.String()
}

// loadMachinesConfig reads ~/manifests/machines.conf and returns a map of
// machine_id → ssh_target. File format:
//
//	# comment
//	ubuntu-max-acb605 = ubuntu@192.168.1.100
//	nas-main          = admin@nas.local
func loadMachinesConfig() map[string]string {
	cfg := make(map[string]string)
	newPath := machinesConfFile()
	oldPath := filepath.Join(userHomeDir(), ".photo-organizer-machines")

	// Migrate old dotfile to new location if needed.
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if data, err := os.ReadFile(oldPath); err == nil {
			os.MkdirAll(filepath.Dir(newPath), 0755)
			if os.WriteFile(newPath, data, 0644) == nil {
				os.Remove(oldPath)
			}
		}
	}

	data, err := os.ReadFile(newPath)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if id != "" && target != "" {
			cfg[id] = target
		}
	}
	return cfg
}

// sshTargetFor returns the SSH target for a machine ID. Falls back to the
// machine ID itself if no entry exists (works if ~/.ssh/config has a match).
func sshTargetFor(machineID string, cfg map[string]string) string {
	if target, ok := cfg[machineID]; ok {
		return target
	}
	return machineID
}

// saveMachinesConfig writes the machines config file, creating it if needed.
func saveMachinesConfig(cfg map[string]string) error {
	path := machinesConfFile()
	os.MkdirAll(filepath.Dir(path), 0755)
	var sb strings.Builder
	sb.WriteString("# photo-organizer machines configuration\n")
	sb.WriteString("# Format: machine_id = user@host\n")
	sb.WriteString("# SSH connection details (port, key, etc.) go in ~/.ssh/config\n\n")
	ids := make([]string, 0, len(cfg))
	for id := range cfg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(&sb, "%-30s = %s\n", id, cfg[id])
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}
