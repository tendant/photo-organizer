package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Collect (pull manifests from remote machines)
// =============================================================================

func runCollect(args []string) {
	// Use pure function to parse arguments
	parsed := parseCollectArgs(args)
	fromMachines := parsed.FromMachines
	remaining := parsed.Remaining

	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	rootFlag := fs.String("root", "", "local manifest directory (default: ~/manifests)")
	addFlag := fs.String("add", "", "register a new machine: --add machine_id=user@host")
	listFlag := fs.Bool("list", false, "list configured machines")
	syncDeleteFlag := fs.Bool("sync-delete", false, "delete local manifests from machine if they don't exist on remote")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect [--from/-f <machine> [--from <machine> ...]]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls manifests from remote machines into ~/manifests/_Manifest/.\n")
		fmt.Fprintf(os.Stderr, "If --from/-f is omitted, collects from all configured machines.\n")
		fmt.Fprintf(os.Stderr, "SSH targets are looked up from ~/manifests/machines.conf.\n\n")
		fmt.Fprintf(os.Stderr, "Collect from all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect\n\n")
		fmt.Fprintf(os.Stderr, "Collect from specific machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -f ubuntu-max -f nas\n\n")
		fmt.Fprintf(os.Stderr, "Sync deletions (only when --from is specified):\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -f ubuntu-max --sync-delete\n\n")
		fmt.Fprintf(os.Stderr, "Register a machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -a ubuntu-max-acb605=ubuntu@192.168.1.100\n\n")
		fmt.Fprintf(os.Stderr, "List configured machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect -l\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(remaining)

	cfg := loadMachinesConfig()

	// --add: register a new machine.
	if *addFlag != "" {
		parts := strings.SplitN(*addFlag, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "collect: --add format is machine_id=user@host")
			os.Exit(1)
		}
		id, target := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		cfg[id] = target
		if err := saveMachinesConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "collect: could not save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Registered: %-30s → %s\n", id, target)
		fmt.Printf("Config saved to %s\n", machinesConfFile())
		return
	}

	// --list: show all configured machines.
	if *listFlag {
		if len(cfg) == 0 {
			fmt.Fprintf(os.Stderr, "No machines configured. Add one with:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer collect --add machine_id=user@host\n")
			return
		}
		fmt.Println("Configured machines:")
		ids := make([]string, 0, len(cfg))
		for id := range cfg {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("  %-30s → %s\n", id, cfg[id])
		}
		return
	}

	// If no --from specified, collect from all configured machines.
	if len(fromMachines) == 0 {
		if len(cfg) == 0 {
			fmt.Fprintln(os.Stderr, "collect: no machines configured. Add one with:")
			fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
			os.Exit(1)
		}
		// Collect from all machines.
		for id := range cfg {
			fromMachines = append(fromMachines, id)
		}
		sort.Strings(fromMachines)
	}

	manifestRoot := *rootFlag
	if manifestRoot == "" {
		manifestRoot = defaultManifestRoot()
	}
	localDir := filepath.Join(manifestRoot, "_Manifest") + "/"
	if err := os.MkdirAll(localDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "collect: cannot create local manifest dir: %v\n", err)
		os.Exit(1)
	}

	// Get local machine ID to avoid collecting from ourselves
	localMachineID := ""
	machineIDPath := filepath.Join(manifestRoot, "machine-id")
	if data, err := os.ReadFile(machineIDPath); err == nil {
		localMachineID = strings.TrimSpace(string(data))
	}

	for _, machine := range fromMachines {
		// Skip collecting from ourselves
		if localMachineID != "" && machine == localMachineID {
			fmt.Fprintf(os.Stderr, "⊘  Skipping %s (local machine, don't collect from self)\n", machine)
			continue
		}

		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			fmt.Fprintf(os.Stderr, "   Add with: photo-organizer collect --add %s=user@host\n", machine)
			continue
		}

		// Skip removable devices (mounted locally, not remote)
		if strings.Contains(target, "[removable]") {
			fmt.Fprintf(os.Stderr, "⊘  Skipping %s (removable device, mounted locally)\n", machine)
			continue
		}

		remoteDir := target + ":~/manifests/_Manifest/"
		fmt.Printf("Collecting from %s (%s)...\n", machine, target)

		var output bytes.Buffer
		cmd := exec.Command("rsync", "-av", "--itemize-changes", remoteDir, localDir)
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Run(); err != nil {
			errMsg := strings.ToLower(output.String() + err.Error())
			fmt.Fprintf(os.Stderr, "⚠  Collect from %s failed\n", machine)

			switch {
			case strings.Contains(errMsg, "connection refused"):
				fmt.Fprintf(os.Stderr, "   Error: Cannot connect to %s (host unreachable or SSH not running)\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ping %s  or  ssh %s echo ok\n", target, target)
			case strings.Contains(errMsg, "permission denied"):
				fmt.Fprintf(os.Stderr, "   Error: Permission denied when connecting to %s\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ssh-copy-id %s  or check SSH keys\n", target)
			case strings.Contains(errMsg, "no such file"):
				fmt.Fprintf(os.Stderr, "   Error: Remote path ~/manifests/_Manifest/ not found on %s\n", target)
				fmt.Fprintf(os.Stderr, "   Try: ssh %s ls -la ~/manifests/\n", target)
			case strings.Contains(errMsg, "timeout"):
				fmt.Fprintf(os.Stderr, "   Error: Connection timeout to %s (network too slow)\n", target)
				fmt.Fprintf(os.Stderr, "   Try: Check network or wait and retry\n")
			default:
				fmt.Fprintf(os.Stderr, "   %s\n", err)
			}
		} else {
			fmt.Printf("Done: %s\n", machine)

			// Report overwritten files (remote data replaced local data)
			// rsync --itemize-changes outputs: ">f+++++++++ filename" (> = receiving, f = file)
			// Lines starting with > indicate files transferred from remote
			rsyncOutput := output.String()
			var overwrites []string
			for _, line := range strings.Split(rsyncOutput, "\n") {
				if len(line) > 11 && strings.HasPrefix(line, ">") {
					// Extract filename (skip the 11-char itemize codes and space)
					filename := strings.TrimSpace(line[11:])
					if filename != "" && !strings.HasSuffix(filename, "/") {
						overwrites = append(overwrites, filename)
					}
				}
			}

			if len(overwrites) > 0 {
				fmt.Fprintf(os.Stderr, "\n⚠️  %d files overwritten from remote:\n", len(overwrites))
				for i, f := range overwrites {
					if i >= 20 {
						fmt.Fprintf(os.Stderr, "   ... and %d more\n", len(overwrites)-20)
						break
					}
					fmt.Fprintf(os.Stderr, "   %s\n", f)
				}
				fmt.Fprintf(os.Stderr, "\n")
			} else {
				fmt.Printf("(already in sync)\n")
			}

			// Handle --sync-delete: delete local manifests from this machine if they don't exist on remote
			if *syncDeleteFlag && len(fromMachines) > 0 {
				// Only sync deletions when collecting from specific machine(s), not from all
				collectFromAll := false
				if len(fromMachines) == 1 {
					// Check if this is the only machine or if collecting from all
					allMachines := []string{}
					for id := range cfg {
						if !strings.Contains(cfg[id], "[removable]") {
							allMachines = append(allMachines, id)
						}
					}
					if len(fromMachines) < len(allMachines) {
						collectFromAll = false
					} else {
						collectFromAll = true
					}
				} else {
					collectFromAll = false
				}

				if !collectFromAll {
					deleteStaleManiests(machine, target, localDir)
				}
			}
		}
	}
}

func deleteStaleManiests(machine, target, localDir string) {
	// Get list of remote manifests for this machine via SSH
	// Use full machine ID in grep pattern to avoid matching other machines
	remoteManifestCmd := fmt.Sprintf("ls -1 ~/manifests/_Manifest/ 2>/dev/null | grep -F -- %s || true", shellQuote("photo_manifest_"+machine+"_"))
	cmd := exec.Command("ssh", target, remoteManifestCmd)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Silently skip if SSH fails
		return
	}

	remoteFiles := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			remoteFiles[line] = true
		}
	}

	// Find all local manifests for this machine and delete ones not on remote
	matches, _ := filepath.Glob(filepath.Join(localDir, fmt.Sprintf("photo_manifest_%s_*", machine)))
	for _, localPath := range matches {
		name := filepath.Base(localPath)
		if !remoteFiles[name] {
			if err := os.Remove(localPath); err == nil {
				fmt.Fprintf(os.Stderr, "  ✓ Deleted stale manifest: %s\n", name)
			}
		}
	}
}

// mergeMachinesConfig merges remote config into local config, preserving remote-only entries.
// Returns the merged config, counts of added/updated/preserved entries, and list of conflicts.
func mergeMachinesConfig(local, remote map[string]string) (map[string]string, int, int, int) {
	merged := make(map[string]string)
	var added, updated, preserved int

	// Copy all remote entries initially
	for k, v := range remote {
		merged[k] = v
	}

	// Local entries override remote entries
	for k, v := range local {
		if _, exists := remote[k]; exists && remote[k] != v {
			updated++
		} else if !exists {
			added++
		}
		merged[k] = v
	}

	// Count preserved remote-only entries
	for k := range remote {
		if _, inLocal := local[k]; !inLocal {
			preserved++
		}
	}

	return merged, added, updated, preserved
}

func runCollectConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-f" || arg == "--from") && i+1 < len(args) {
			processedArgs = append(processedArgs, "--from", args[i+1])
			i++
		} else if after, ok := strings.CutPrefix(arg, "-f="); ok {
			processedArgs = append(processedArgs, "--from="+after)
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("collect-config", flag.ExitOnError)
	fromFlag := fs.String("from", "", "collect from specific machine (default: all machines)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer collect-config [-f/--from <machine>]\n\n")
		fmt.Fprintf(os.Stderr, "Pulls machines.conf from remote machines and merges locally.\n")
		fmt.Fprintf(os.Stderr, "Remote-only entries are preserved; local entries override.\n\n")
		fmt.Fprintf(os.Stderr, "Collect from all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect-config\n\n")
		fmt.Fprintf(os.Stderr, "Collect from specific machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer collect-config -f ubuntu-max\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	cfg := loadMachinesConfig()
	if len(cfg) == 0 {
		fmt.Fprintln(os.Stderr, "No machines configured. Add one with:")
		fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
		return
	}

	fromMachines := []string{}
	if *fromFlag != "" {
		fromMachines = append(fromMachines, *fromFlag)
	} else {
		for id := range cfg {
			fromMachines = append(fromMachines, id)
		}
		sort.Strings(fromMachines)
	}

	merged := make(map[string]string)
	// Start with current local config
	for k, v := range cfg {
		merged[k] = v
	}

	for _, machine := range fromMachines {
		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			continue
		}

		fmt.Printf("Collecting config from %s (%s)...\n", machine, target)

		// SSH to remote and get machines.conf (silently returns empty if missing)
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd := exec.Command("ssh", target, "cat ~/manifests/machines.conf 2>/dev/null || echo ''")
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not fetch config from %s: %v\n", machine, err)
			continue
		}

		// Parse remote config
		remoteConfig := make(map[string]string)
		for _, line := range strings.Split(out.String(), "\n") {
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
				remoteConfig[id] = target
			}
		}

		// Merge: mergeMachinesConfig(remote, local) to count remote-originated changes
		// "added" = new entries from remote, "updated" = conflicting entries
		_, found, conflicts, _ := mergeMachinesConfig(remoteConfig, merged)

		// Now merge remote into merged with local precedence
		for k, v := range remoteConfig {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}

		fmt.Printf("  Found: %d new, Conflicts: %d (local kept)\n", found, conflicts)
	}

	// Save merged config
	if err := saveMachinesConfig(merged); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving merged config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nMerged config saved to %s\n", machinesConfFile())
}

func runPushConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "-t" || arg == "--to") && i+1 < len(args) {
			processedArgs = append(processedArgs, "--to", args[i+1])
			i++
		} else if after, ok := strings.CutPrefix(arg, "-t="); ok {
			processedArgs = append(processedArgs, "--to="+after)
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("push-config", flag.ExitOnError)
	toFlag := fs.String("to", "", "push to specific machine (default: all machines)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer push-config [-t/--to <machine>]\n\n")
		fmt.Fprintf(os.Stderr, "Pushes local machines.conf to remote machines with merge strategy.\n")
		fmt.Fprintf(os.Stderr, "Local entries override; remote-only entries are preserved.\n\n")
		fmt.Fprintf(os.Stderr, "Push to all machines:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer push-config\n\n")
		fmt.Fprintf(os.Stderr, "Push to specific machine:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer push-config -t ubuntu-max\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	cfg := loadMachinesConfig()
	if len(cfg) == 0 {
		fmt.Fprintln(os.Stderr, "No machines configured. Add one with:")
		fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
		return
	}

	toMachines := []string{}
	if *toFlag != "" {
		toMachines = append(toMachines, *toFlag)
	} else {
		for id := range cfg {
			toMachines = append(toMachines, id)
		}
		sort.Strings(toMachines)
	}

	for _, machine := range toMachines {
		target := sshTargetFor(machine, cfg)
		if target == machine {
			fmt.Fprintf(os.Stderr, "⚠  Machine %q not configured in machines.conf\n", machine)
			continue
		}

		fmt.Printf("Pushing config to %s (%s)...\n", machine, target)

		// SSH to remote and get their current machines.conf
		var out bytes.Buffer
		var stderr bytes.Buffer
		cmd := exec.Command("ssh", target, "cat ~/manifests/machines.conf 2>/dev/null || echo ''")
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not fetch remote config from %s: %v\n", machine, err)
			continue
		}

		// Parse remote config
		remoteConfig := make(map[string]string)
		for _, line := range strings.Split(out.String(), "\n") {
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
				remoteConfig[id] = target
			}
		}

		// Merge: local overrides, remote-only preserved
		merged, added, updated, preserved := mergeMachinesConfig(cfg, remoteConfig)

		// Build merged config content
		var sb strings.Builder
		sb.WriteString("# photo-organizer machines configuration\n")
		sb.WriteString("# Format: machine_id = user@host\n")
		sb.WriteString("# SSH connection details (port, key, etc.) go in ~/.ssh/config\n\n")
		ids := make([]string, 0, len(merged))
		for id := range merged {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&sb, "%-30s = %s\n", id, merged[id])
		}

		// Push merged config to remote via SSH
		var pushStderr bytes.Buffer
		pushCmd := exec.Command("ssh", target, "mkdir -p ~/manifests && cat > ~/manifests/machines.conf")
		pushCmd.Stdin = strings.NewReader(sb.String())
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = &pushStderr
		if err := pushCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠  Could not push config to %s: %v\n", machine, err)
			continue
		}

		fmt.Printf("  Added: %d, Updated: %d, Preserved remote-only: %d\n", added, updated, preserved)
		fmt.Printf("Done: %s\n\n", machine)
	}
}

func runSyncConfig(args []string) {
	// Convert short flags to long form for consistency
	var processedArgs []string
	for _, arg := range args {
		if arg == "-d" || arg == "--dry-run" {
			processedArgs = append(processedArgs, "--dry-run")
		} else {
			processedArgs = append(processedArgs, arg)
		}
	}

	fs := flag.NewFlagSet("sync-config", flag.ExitOnError)
	dryRunFlag := fs.Bool("dry-run", false, "show what would be added without modifying machines.conf")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: photo-organizer sync-config [-d/--dry-run]\n\n")
		fmt.Fprintf(os.Stderr, "Auto-register machines found in manifests to machines.conf.\n")
		fmt.Fprintf(os.Stderr, "Reads all manifest files and adds any machines not yet in machines.conf.\n")
		fmt.Fprintf(os.Stderr, "Machines are tagged as [local], [removable], or plain SSH targets.\n\n")
		fmt.Fprintf(os.Stderr, "Dry-run preview:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer sync-config -d\n\n")
		fmt.Fprintf(os.Stderr, "Actually sync:\n")
		fmt.Fprintf(os.Stderr, "  photo-organizer sync-config\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(processedArgs)

	// Load all manifests
	manifestRoot := defaultManifestRoot()
	manifestDir := filepath.Join(manifestRoot, "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No manifests found in %s\n", manifestDir)
		return
	}

	// Read all manifests and extract unique machines
	var sources []ManifestSource
	machineMap := make(map[string]ManifestSource) // machine name -> best source

	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil || len(src.Rows) == 0 {
			continue
		}
		// Keep the first (or best) source for each machine
		if _, exists := machineMap[src.MachineName]; !exists {
			sources = append(sources, src)
			machineMap[src.MachineName] = src
		}
	}

	if len(sources) == 0 {
		fmt.Fprintf(os.Stderr, "No valid manifests with data found\n")
		return
	}

	// Load current machines.conf
	cfg := loadMachinesConfig()

	// Determine which machines need to be added
	type NewMachine struct {
		ID     string
		Target string
		Kind   string // "local", "removable", or "unknown"
	}
	var newMachines []NewMachine

	for _, src := range sources {
		if _, exists := cfg[src.MachineName]; exists {
			// Already configured
			continue
		}

		kind := "unknown"
		target := ""

		if isRemovablePath(src.ScanPath) {
			// Removable media (USB, SD card, etc.)
			kind = "removable"
			target = "[removable] scanned from: " + src.ScanPath
		} else if src.IsLocal {
			// Local machine
			kind = "local"
			target = "[local] scanned from: " + src.ScanPath
		}
		// For remote machines, we can't auto-configure (need SSH target)

		if kind != "unknown" {
			newMachines = append(newMachines, NewMachine{
				ID:     src.MachineName,
				Target: target,
				Kind:   kind,
			})
		}
	}

	if len(newMachines) == 0 {
		fmt.Printf("All machines in manifests are already configured in machines.conf\n")
		return
	}

	// Display what will be added
	fmt.Printf("Found %d new machines to add to machines.conf:\n\n", len(newMachines))
	for _, m := range newMachines {
		fmt.Printf("  %-30s → %s (%s)\n", m.ID, m.Target, m.Kind)
	}

	if *dryRunFlag {
		fmt.Printf("\nDry-run mode: no changes made\n")
		return
	}

	// Add to machines.conf
	for _, m := range newMachines {
		cfg[m.ID] = m.Target
	}

	if err := saveMachinesConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving machines.conf: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Added %d machines to %s\n", len(newMachines), machinesConfFile())
}
