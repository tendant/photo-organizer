package main

import (
	"fmt"
	"os"
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

func hasIndependentBackup(sources []ManifestSource, idx map[string][]hashLocation, machineName, partialHash string, sizeBytes int64) bool {
	locs := idx[indexKey(partialHash, sizeBytes)]
	for _, loc := range locs {
		src := sources[loc.sourceIdx]
		if src.MachineName == machineName {
			continue
		}
		if isRemovablePath(src.ScanPath) {
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
