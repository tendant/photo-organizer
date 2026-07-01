package main

import (
	"path/filepath"
)

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
