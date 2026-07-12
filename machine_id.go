package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveMachineID returns the machine ID with priority:
// 1. Explicit flag value (if provided and non-empty)
// 2. ./machine-id file in current working directory (if exists)
// 3. ~/manifests/machine-id (via machineID())
func resolveMachineID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}

	// Check for machine-id file in current directory
	cwd, err := os.Getwd()
	if err == nil {
		localPath := filepath.Join(cwd, "machine-id")
		if data, err := os.ReadFile(localPath); err == nil {
			if id := strings.TrimSpace(string(data)); id != "" {
				return id
			}
		}
	}

	// Fall back to global machine ID
	return machineID()
}

// machineID returns a stable, unique machine identifier of the form
// "Ls-MBP-a3f7c2". It is computed once and cached in ~/manifests/machine-id
// so it never changes even if the hostname is later renamed.
func machineID() string {
	newPath := machineIDFile()
	oldPath := filepath.Join(userHomeDir(), ".photo-organizer-id")

	// Migrate old dotfile to new location if needed.
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if data, err := os.ReadFile(oldPath); err == nil {
			os.MkdirAll(filepath.Dir(newPath), 0755)
			if os.WriteFile(newPath, data, 0644) == nil {
				os.Remove(oldPath)
			}
		}
	}

	// Return cached ID if it exists.
	if data, err := os.ReadFile(newPath); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}

	// Build a new ID: short hostname + 6-char hardware UUID suffix.
	id := buildMachineID()

	// Persist it so it never changes.
	os.MkdirAll(filepath.Dir(newPath), 0755)
	os.WriteFile(newPath, []byte(id+"\n"), 0644)
	return id
}

func buildMachineID() string {
	name := localHostName()
	suffix := hardwareUUIDSuffix()
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// localHostName returns the user-set computer name, falling back to the
// short hostname (first label only, stripping domain/Tailscale suffixes).
func localHostName() string {
	// macOS: LocalHostName is stable and not network-dependent.
	if out, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}
	if h, err := os.Hostname(); err == nil {
		return strings.SplitN(h, ".", 2)[0]
	}
	return "unknown"
}

// hardwareUUIDSuffix returns the first 6 hex chars of the hardware UUID.
// On macOS this comes from ioreg; on Linux from /etc/machine-id.
func hardwareUUIDSuffix() string {
	// macOS: line format is:  "IOPlatformUUID" = "XXXXXXXX-XXXX-..."
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, "IOPlatformUUID") {
				continue
			}
			// Extract the value between the last pair of quotes.
			if i := strings.LastIndex(line, "\""); i > 0 {
				if j := strings.LastIndex(line[:i], "\""); j >= 0 {
					uuid := line[j+1 : i]
					raw := strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
					if len(raw) >= 6 {
						return raw[:6]
					}
				}
			}
		}
	}
	// Linux
	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(data)); len(id) >= 6 {
			return id[:6]
		}
	}
	return ""
}
