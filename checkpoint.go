package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScanCheckpoint tracks scan progress for resumption.
type ScanCheckpoint struct {
	ManifestPath  string    `json:"manifest_path"`
	ScanPath      string    `json:"scan_path"`
	ProcessedDir  int       `json:"processed_dirs"`
	ProcessedFile int       `json:"processed_files"`
	LastFile      string    `json:"last_file"`
	StartTime     time.Time `json:"start_time"`
	LastUpdate    time.Time `json:"last_update"`
}

// CheckpointDir returns the directory for storing scan checkpoints.
func CheckpointDir() string {
	if dir := strings.TrimSpace(os.Getenv("PHOTO_ORGANIZER_CHECKPOINT_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(userHomeDir(), "manifests", "_checkpoints")
}

// CheckpointPath returns the checkpoint file path for a manifest.
func CheckpointPath(manifestPath string) string {
	name := filepath.Base(manifestPath)
	return filepath.Join(CheckpointDir(), name+".checkpoint")
}

// LoadCheckpoint loads a previous scan checkpoint if it exists.
func LoadCheckpoint(manifestPath string) (*ScanCheckpoint, error) {
	path := CheckpointPath(manifestPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // File doesn't exist or can't read
	}

	var cp ScanCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// SaveCheckpoint saves scan progress for potential resumption.
func SaveCheckpoint(cp *ScanCheckpoint) error {
	dir := CheckpointDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}

	path := CheckpointPath(cp.ManifestPath)
	return os.WriteFile(path, data, 0600)
}

// ClearCheckpoint removes a checkpoint after successful completion.
func ClearCheckpoint(manifestPath string) error {
	path := CheckpointPath(manifestPath)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil // Already gone, that's fine
	}
	return err
}

// DetectInterruptedOperations checks for incomplete operations and provides recovery hints.
func DetectInterruptedOperations() {
	dir := CheckpointDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // Checkpoint dir doesn't exist yet
	}

	var found []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".checkpoint") {
			found = append(found, entry.Name())
		}
	}

	if len(found) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  Found %d incomplete scan(s). Run scan again to refresh:\n", len(found))
		for _, f := range found {
			name := strings.TrimSuffix(f, ".checkpoint")
			fmt.Fprintf(os.Stderr, "   photo-organizer scan <folder>  (checkpoint: %s)\n", name)
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}
