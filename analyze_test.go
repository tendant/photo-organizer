package main

import (
	"os"
	"strings"
	"testing"
)

// =============================================================================
// Helpers
// =============================================================================

func makeSource(machine, scanPath string, files []struct{ rel, hash string; size int64 }) ManifestSource {
	var rows []ManifestRow
	for _, f := range files {
		rows = append(rows, ManifestRow{
			Filename:     f.rel,
			RelativePath: f.rel,
			SizeBytes:    f.size,
			PartialHash:  f.hash,
			FullHash:     f.hash, // tests use same value for both; real files differ
			Extension:    ".jpg",
			ScanPath:     scanPath,
			MachineName:  machine,
		})
	}
	return ManifestSource{
		MachineName: machine,
		ScanPath:    scanPath,
		Label:       machine + " @ " + scanPath,
		Rows:        rows,
	}
}

// =============================================================================
// topLevelFolder
// =============================================================================

func TestTopLevelFolder(t *testing.T) {
	tests := []struct{ rel, want string }{
		{"IMG_001.jpg", "(root)"},
		{"Vacation/IMG_001.jpg", "Vacation"},
		{"Vacation/2023/IMG_001.jpg", "Vacation"},
		{"", "(root)"},
	}
	for _, tt := range tests {
		if got := topLevelFolder(tt.rel); got != tt.want {
			t.Errorf("topLevelFolder(%q) = %q, want %q", tt.rel, got, tt.want)
		}
	}
}

// =============================================================================
// absFilePath
// =============================================================================

func TestAbsFilePath(t *testing.T) {
	src := ManifestSource{ScanPath: "/Photos"}
	row := ManifestRow{RelativePath: "Vacation/IMG_001.jpg"}
	got := absFilePath(src, row)
	if got != "/Photos/Vacation/IMG_001.jpg" {
		t.Errorf("got %q, want /Photos/Vacation/IMG_001.jpg", got)
	}
}

// =============================================================================
// overlappingPairs
// =============================================================================

func TestOverlappingPairs(t *testing.T) {
	parent := makeSource("mac", "/Photos", nil)
	child := makeSource("mac", "/Photos/Vacation", nil)
	other := makeSource("nas", "/volume1/photos", nil)
	unrelated := makeSource("mac", "/Videos", nil)

	sources := []ManifestSource{parent, child, other, unrelated}
	pairs := overlappingPairs(sources)

	// parent (0) and child (1) overlap — both directions
	if !pairs[[2]int{0, 1}] {
		t.Error("expected parent→child to be overlapping")
	}
	if !pairs[[2]int{1, 0}] {
		t.Error("expected child→parent to be overlapping")
	}
	// different machine — not overlapping
	if pairs[[2]int{0, 2}] {
		t.Error("different machines should not be overlapping")
	}
	// same machine, unrelated paths — not overlapping
	if pairs[[2]int{0, 3}] {
		t.Error("unrelated paths on same machine should not be overlapping")
	}
	// same scan path counts as overlapping
	same1 := makeSource("mac", "/Photos", nil)
	same2 := makeSource("mac", "/Photos", nil)
	p2 := overlappingPairs([]ManifestSource{same1, same2})
	if !p2[[2]int{0, 1}] {
		t.Error("identical scan paths should be overlapping")
	}
}

// =============================================================================
// findDuplicates
// =============================================================================

func TestFindDuplicates(t *testing.T) {
	mac := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100},
		{"IMG_002.jpg", "bbb", 200},
	})
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // dup of mac IMG_001
		{"IMG_003.jpg", "ccc", 300}, // unique to nas
	})

	sources := []ManifestSource{mac, nas}
	idx := buildHashIndex(sources)
	dups := findDuplicates(sources, idx)

	if len(dups) != 1 {
		t.Fatalf("expected 1 duplicate group, got %d", len(dups))
	}
	if dups[0].PartialHash != "aaa" {
		t.Errorf("expected hash aaa, got %s", dups[0].PartialHash)
	}
	if len(dups[0].Locations) != 2 {
		t.Errorf("expected 2 locations, got %d", len(dups[0].Locations))
	}
}

func TestFindDuplicatesSameHashDifferentSize(t *testing.T) {
	// Same partial hash, different size — must NOT be reported as duplicate.
	mac := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100},
	})
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 999}, // same hash, different size — hash collision
	})
	idx := buildHashIndex([]ManifestSource{mac, nas})
	dups := findDuplicates([]ManifestSource{mac, nas}, idx)
	if len(dups) != 0 {
		t.Errorf("same hash but different size should not be a duplicate, got %d groups", len(dups))
	}
}

func TestFindDuplicatesNone(t *testing.T) {
	mac := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100},
	})
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"IMG_002.jpg", "bbb", 200},
	})
	idx := buildHashIndex([]ManifestSource{mac, nas})
	dups := findDuplicates([]ManifestSource{mac, nas}, idx)
	if len(dups) != 0 {
		t.Errorf("expected no duplicates, got %d", len(dups))
	}
}

// =============================================================================
// findUnique
// =============================================================================

func TestFindUnique(t *testing.T) {
	mac := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // on both machines
		{"IMG_002.jpg", "bbb", 200}, // only on mac
	})
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // on both machines
		{"IMG_003.jpg", "ccc", 300}, // only on nas
	})

	sources := []ManifestSource{mac, nas}
	idx := buildHashIndex(sources)
	unique := findUnique(sources, idx)

	if len(unique["mac"]) != 1 || unique["mac"][0].PartialHash != "bbb" {
		t.Errorf("mac unique: expected [bbb], got %v", unique["mac"])
	}
	if len(unique["nas"]) != 1 || unique["nas"][0].PartialHash != "ccc" {
		t.Errorf("nas unique: expected [ccc], got %v", unique["nas"])
	}
}

func TestFindUniqueDeduplicatesOverlappingScans(t *testing.T) {
	// Same physical file appears in both parent and child scan on same machine.
	parent := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"Vacation/IMG_001.jpg", "aaa", 100},
	})
	child := makeSource("mac", "/Photos/Vacation", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // same file, different relative path
	})

	sources := []ManifestSource{parent, child}
	idx := buildHashIndex(sources)
	unique := findUnique(sources, idx)

	// Should appear only once — same physical file
	if len(unique["mac"]) != 1 {
		t.Errorf("expected 1 unique file (deduplicated), got %d", len(unique["mac"]))
	}
}

// =============================================================================
// findIntraMachine
// =============================================================================

func TestFindIntraMachine(t *testing.T) {
	// Two genuinely different copies of the same file on the same machine.
	src1 := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"Backup/IMG_001.jpg", "aaa", 100},
	})
	src2 := makeSource("mac", "/Archive", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // different absolute path — real duplicate
	})

	sources := []ManifestSource{src1, src2}
	idx := buildHashIndex(sources)
	dups := findIntraMachine(sources, idx)

	if len(dups) != 1 {
		t.Errorf("expected 1 intra-machine duplicate, got %d", len(dups))
	}
}

func TestFindIntraMachineSkipsOverlappingScans(t *testing.T) {
	// Parent and child scan of the same directory — NOT a real duplicate.
	parent := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"Vacation/IMG_001.jpg", "aaa", 100},
	})
	child := makeSource("mac", "/Photos/Vacation", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // same physical file
	})

	sources := []ManifestSource{parent, child}
	idx := buildHashIndex(sources)
	dups := findIntraMachine(sources, idx)

	if len(dups) != 0 {
		t.Errorf("overlapping scans should not produce intra-machine duplicates, got %d", len(dups))
	}
}

// =============================================================================
// buildDeletePlan
// =============================================================================

func TestBuildDeletePlanBasic(t *testing.T) {
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100},
		{"IMG_002.jpg", "bbb", 200},
	})
	laptop := makeSource("laptop", "/home/photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100}, // backed up on nas
		{"IMG_003.jpg", "ccc", 300}, // unique to laptop — must NOT appear in plan
	})

	plan := buildDeletePlan([]ManifestSource{nas, laptop}, "nas")

	if len(plan) != 1 {
		t.Fatalf("expected 1 delete candidate, got %d", len(plan))
	}
	if plan[0].Machine != "laptop" {
		t.Errorf("expected laptop candidate, got %s", plan[0].Machine)
	}
	if plan[0].RelPath != "IMG_001.jpg" {
		t.Errorf("expected IMG_001.jpg, got %s", plan[0].RelPath)
	}
}

func TestBuildDeletePlanKeepMachineNotInManifests(t *testing.T) {
	laptop := makeSource("laptop", "/home/photos", []struct{ rel, hash string; size int64 }{
		{"IMG_001.jpg", "aaa", 100},
	})
	// Keep machine "nas" doesn't have the file — should produce no candidates.
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"OTHER.jpg", "bbb", 200},
	})

	plan := buildDeletePlan([]ManifestSource{nas, laptop}, "nas")
	if len(plan) != 0 {
		t.Errorf("keep machine doesn't have the file — expected 0 candidates, got %d", len(plan))
	}
}

func TestBuildIntraPlanThreeCopies(t *testing.T) {
	// Same file exists in 3 folders on the same machine.
	// Default (no --keep-under): keep first, delete 2. Both 2 deletes should
	// list the one kept copy as backup.
	src := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"Originals/file.DNG", "aaa", 100},
		{"Exports/file.DNG", "aaa", 100},
		{"Archive/file.DNG", "aaa", 100},
	})
	plan := buildIntraPlan([]ManifestSource{src}, "mac", "")
	if len(plan) != 2 {
		t.Fatalf("expected 2 delete candidates for 3 copies, got %d", len(plan))
	}
	for _, c := range plan {
		if len(c.Backups) != 1 {
			t.Errorf("expected 1 backup copy listed, got %d", len(c.Backups))
		}
	}
}

func TestBuildIntraPlanKeepUnderMultipleKept(t *testing.T) {
	// File exists in 3 folders; 2 are under keep-under prefix.
	// Should delete the 1 outside the prefix, show both kept paths as backups.
	src := makeSource("mac", "/Photos", []struct{ rel, hash string; size int64 }{
		{"Originals/2025/file.DNG", "aaa", 100},
		{"Originals/archive/file.DNG", "aaa", 100},
		{"Exports/file.DNG", "aaa", 100}, // outside keep-under
	})
	plan := buildIntraPlan([]ManifestSource{src}, "mac", "/Photos/Originals")
	if len(plan) != 1 {
		t.Fatalf("expected 1 delete candidate (Exports copy), got %d", len(plan))
	}
	if len(plan[0].Backups) != 2 {
		t.Errorf("expected 2 backup copies (both Originals paths), got %d", len(plan[0].Backups))
	}
	if plan[0].RelPath != "Exports/file.DNG" {
		t.Errorf("expected Exports copy to be deleted, got %s", plan[0].RelPath)
	}
}

func TestBuildDeletePlanNeverDeletesUniqueFiles(t *testing.T) {
	nas := makeSource("nas", "/volume1", []struct{ rel, hash string; size int64 }{
		{"shared.jpg", "aaa", 100},
	})
	laptop := makeSource("laptop", "/home", []struct{ rel, hash string; size int64 }{
		{"shared.jpg", "aaa", 100}, // dup → eligible
		{"unique.jpg", "zzz", 999}, // unique → must NEVER appear
	})

	plan := buildDeletePlan([]ManifestSource{nas, laptop}, "nas")
	for _, c := range plan {
		if c.RelPath == "unique.jpg" {
			t.Error("unique.jpg should never appear in delete plan")
		}
	}
}

// =============================================================================
// shellQuote
// =============================================================================

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/Photos/IMG 001.jpg", "'/Photos/IMG 001.jpg'"},
		{"/Photos/it's/file.jpg", "'/Photos/it'\\''s/file.jpg'"},
		{"/simple/path.jpg", "'/simple/path.jpg'"},
	}
	for _, tt := range tests {
		if got := shellQuote(tt.in); got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// =============================================================================
// overlapWarnings
// =============================================================================

func TestOverlapWarnings(t *testing.T) {
	parent := makeSource("mac", "/Photos", nil)
	child := makeSource("mac", "/Photos/Vacation", nil)
	sources := []ManifestSource{parent, child}
	pairs := overlappingPairs(sources)
	warnings := overlapWarnings(sources, pairs)

	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if !strings.Contains(warnings[0], "contains") {
		t.Errorf("warning should mention 'contains': %s", warnings[0])
	}
}

// =============================================================================
// readManifest: Empty, Header Only, Corrupt, Old Format
// =============================================================================

func TestReadManifestEmpty(t *testing.T) {
	dir := t.TempDir()
	manifestFile := dir + "/empty.csv"
	// Write a 0-byte file.
	if err := os.WriteFile(manifestFile, []byte{}, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	_, err := readManifest(manifestFile)
	if err == nil {
		t.Errorf("readManifest should return an error for empty file, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty', got %q", err.Error())
	}
}

func TestReadManifestHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	manifestFile := dir + "/header-only.csv"
	// Write just the header row.
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,partial_hash,full_hash,scan_date,scan_path,machine_name`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	source, _ := readManifest(manifestFile)
	if len(source.Rows) != 0 {
		t.Errorf("header-only manifest should have 0 rows, got %d", len(source.Rows))
	}
	// Check no crash on second read.
	if _, err := readManifest(manifestFile); err != nil {
		t.Fatalf("second read failed: %v", err)
	}
}

func TestReadManifestCorrupt(t *testing.T) {
	dir := t.TempDir()
	manifestFile := dir + "/corrupt.csv"
	// Write valid header + corrupt mid-row (unmatched quote).
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,partial_hash,full_hash,scan_date,scan_path,machine_name
test.jpg,"test.jpg,1000,0,2023-01-01,2023-01-01,,,abc123,,2023-01-01,/scan,m1`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	_, err := readManifest(manifestFile)
	if err == nil {
		t.Errorf("readManifest should return an error for corrupt CSV, got nil")
	}
}

func TestReadManifestOldFormat(t *testing.T) {
	dir := t.TempDir()
	manifestFile := dir + "/old-format.csv"
	// Old format uses file_hash instead of partial_hash.
	content := `filename,relative_path,file_size_bytes,file_size_mb,file_modified,capture_date,camera_make,camera_model,file_hash,full_hash,scan_date,scan_path,machine_name
test.jpg,test.jpg,1000,0,2023-01-01,2023-01-01,,,oldhash123,,2023-01-01,/scan,m1`
	if err := os.WriteFile(manifestFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	source, _ := readManifest(manifestFile)
	if len(source.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(source.Rows))
	}
	// Old file_hash should be read into PartialHash.
	if source.Rows[0].PartialHash != "oldhash123" {
		t.Errorf("got PartialHash %q, want %q", source.Rows[0].PartialHash, "oldhash123")
	}
}

// =============================================================================
// sshVerifyPaths: Empty Paths, Command Failure
// =============================================================================

func TestSSHVerifyPathsEmptyPaths(t *testing.T) {
	// Call with empty paths; should return empty map without invoking SSH.
	result := sshVerifyPaths("unused-host", []string{})
	if len(result) != 0 {
		t.Errorf("expected empty map for empty paths, got %d entries", len(result))
	}
}

func TestSSHVerifyPathsCommandFailure(t *testing.T) {
	// Pass an invalid/unreachable host; SSH should fail quickly (with timeout from Fix 3).
	// The function should return an empty map (all unverified) without panicking.
	result := sshVerifyPaths("127.0.0.1:65535", []string{"/nonexistent/path"})
	if len(result) != 0 {
		t.Errorf("command failure should return empty map (all unverified), got %d entries", len(result))
	}
}
