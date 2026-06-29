package main

import "path/filepath"

func defaultManifestRoot() string {
	return filepath.Join(userHomeDir(), "manifests")
}

func machineIDFile() string {
	return filepath.Join(defaultManifestRoot(), "machine-id")
}

func machinesConfFile() string {
	return filepath.Join(defaultManifestRoot(), "machines.conf")
}
