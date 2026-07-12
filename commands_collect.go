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

func runCollect(args []string) error {
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
			return errFailed
		}
		id, target := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		cfg[id] = target
		if err := saveMachinesConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "collect: could not save config: %v\n", err)
			return errFailed
		}
		fmt.Printf("Registered: %-30s → %s\n", id, target)
		fmt.Printf("Config saved to %s\n", machinesConfFile())
		return nil
	}

	// --list: show all configured machines.
	if *listFlag {
		if len(cfg) == 0 {
			fmt.Fprintf(os.Stderr, "No machines configured. Add one with:\n")
			fmt.Fprintf(os.Stderr, "  photo-organizer collect --add machine_id=user@host\n")
			return nil
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
		return nil
	}

	// If no --from specified, collect from all configured machines.
	if len(fromMachines) == 0 {
		if len(cfg) == 0 {
			fmt.Fprintln(os.Stderr, "collect: no machines configured. Add one with:")
			fmt.Fprintln(os.Stderr, "  photo-organizer collect --add machine_id=user@host")
			return errFailed
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
		return errFailed
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
					deleteStaleManifests(machine, target, localDir)
				}
			}
		}
	}
	return nil
}

func deleteStaleManifests(machine, target, localDir string) {
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
