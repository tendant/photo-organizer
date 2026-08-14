package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func parseRequiredDestFlag(args []string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == "--dest" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func resolveExistingFolder(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

func loadManifestSources(currentMachineID string) []ManifestSource {
	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	matches, _ := filepath.Glob(filepath.Join(manifestDir, "*.csv"))

	var sources []ManifestSource
	for _, path := range matches {
		src, err := readManifest(path)
		if err != nil {
			continue
		}
		if currentMachineID != "" {
			markManifestOrigin(&src, currentMachineID)
		}
		sources = append(sources, src)
	}
	return sources
}

func findManifestForScanPath(absPath string) (ManifestSource, bool) {
	manifestDir := filepath.Join(userHomeDir(), "manifests", "_Manifest")
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return ManifestSource{}, false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".csv") {
			continue
		}
		fullPath := filepath.Join(manifestDir, entry.Name())
		src, err := readManifest(fullPath)
		if err == nil && src.ScanPath == absPath {
			return src, true
		}
	}
	return ManifestSource{}, false
}

func hasIndependentBackup(sources []ManifestSource, idx map[string][]hashLocation, machinesCfg map[string]string, machineName, partialHash string, sizeBytes int64) bool {
	locs := idx[indexKey(partialHash, sizeBytes)]
	for _, loc := range locs {
		src := sources[loc.sourceIdx]
		if src.MachineName == machineName {
			continue
		}
		if isRemovableSource(src.MachineName, src.ScanPath, machinesCfg) {
			continue
		}
		return true
	}
	return false
}

func resolveBackupDestination(destLocation string, machines map[string]string) (remoteUserHost, remoteMachineID, remotePath string, err error) {
	parts := strings.SplitN(destLocation, ":", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid destination format. Use: machine-id:/path or user@host:/path")
	}

	destIdentifier := parts[0]
	remotePath = parts[1]

	if sshHost, exists := machines[destIdentifier]; exists {
		return sshHost, destIdentifier, remotePath, nil
	}

	if strings.Contains(destIdentifier, "@") {
		remoteUserHost = destIdentifier
		for machID, sshHost := range machines {
			if sshHost == remoteUserHost {
				return remoteUserHost, machID, remotePath, nil
			}
		}
		return remoteUserHost, "", remotePath, nil
	}

	for machID, sshHost := range machines {
		if sshHost == destIdentifier {
			return sshHost, machID, remotePath, nil
		}
	}

	return "", "", "", fmt.Errorf("%q not found in machines.conf", destIdentifier)
}

// isRemoteDestination reports whether dest names an SSH target
// (machine-id:/path or user@host:/path) rather than a local directory.
// A local path never has a colon before its first slash.
func isRemoteDestination(dest string) bool {
	colon := strings.Index(dest, ":")
	if colon <= 0 || strings.HasPrefix(dest, ".") || strings.HasPrefix(dest, "~") {
		return false
	}
	return !strings.Contains(dest[:colon], "/")
}

func printBackupDestinationOptions(machines map[string]string) {
	fmt.Fprintf(os.Stderr, "Available options:\n")
	for machID, sshHost := range machines {
		if !strings.HasPrefix(sshHost, "[removable]") {
			fmt.Fprintf(os.Stderr, "  - %s (machine-id: %s)\n", sshHost, machID)
		}
	}
}

// rsyncRelPaths copies the given srcRoot-relative paths to remoteUserHost:remotePath,
// creating the destination tree as needed.
func rsyncRelPaths(srcRoot string, relPaths []string, remoteUserHost, remotePath string) error {
	tmpFile, err := os.CreateTemp("", "photo-organizer-files-*.txt")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	for _, rel := range relPaths {
		fmt.Fprintln(tmpFile, rel)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("cannot write file list: %w", err)
	}

	cmd := exec.Command("rsync", "-az", "--files-from="+tmpFile.Name(), srcRoot+"/", remoteUserHost+":"+remotePath+"/")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// refreshRemoteManifest rescans remotePath on the remote machine and pulls the
// updated manifest back, so files copied there count as a backup locally.
func refreshRemoteManifest(remoteUserHost, remoteMachineID, remotePath string) error {
	scanCmd := fmt.Sprintf("cd %s && for path in photo-organizer ~/bin/photo-organizer /usr/local/bin/photo-organizer; do if command -v $path &>/dev/null || [ -f $path ]; then $path scan . --machine %s >/dev/null 2>&1; exit $?; fi; done; exit 1", shellQuote(remotePath), shellQuote(remoteMachineID))
	if err := exec.Command("ssh", remoteUserHost, scanCmd).Run(); err != nil {
		return fmt.Errorf("remote scan failed after copying files. photo-organizer must be installed on remote machine")
	}

	collectCmd := exec.Command("photo-organizer", "collect", "--from", remoteMachineID)
	collectCmd.Stdout = os.Stderr
	collectCmd.Stderr = os.Stderr
	if err := collectCmd.Run(); err != nil {
		return fmt.Errorf("files were copied, but manifest collection failed: %v", err)
	}
	return nil
}
