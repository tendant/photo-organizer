package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCLIMainHelper(t *testing.T) {
	if os.Getenv("PHOTO_ORGANIZER_TEST_MAIN") != "1" {
		return
	}
	args := []string{"photo-organizer"}
	for i, arg := range os.Args {
		if arg == "--" {
			args = append(args, os.Args[i+1:]...)
			break
		}
	}
	os.Args = args
	main()
}

func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runCLIWithHomeInputEnv(t, t.TempDir(), "", nil, args...)
}

func runCLIWithHome(t *testing.T, homeDir string, args ...string) (string, int) {
	t.Helper()
	return runCLIWithHomeInputEnv(t, homeDir, "", nil, args...)
}

func runCLIWithHomeInput(t *testing.T, homeDir, stdin string, args ...string) (string, int) {
	t.Helper()
	return runCLIWithHomeInputEnv(t, homeDir, stdin, nil, args...)
}

func runCLIWithHomeInputEnv(t *testing.T, homeDir, stdin string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestCLIMainHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Dir = "."
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Env = append(append(os.Environ(),
		"HOME="+homeDir,
		"PHOTO_ORGANIZER_TEST_MAIN=1",
		"PHOTO_ORGANIZER_CHECKPOINT_DIR="+filepath.Join(homeDir, "checkpoints"),
	), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("run cli %v: %v\n%s", args, err, out)
	return "", -1
}

func readAllManifestSources(t *testing.T, homeDir string) []ManifestSource {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(homeDir, "manifests", "_Manifest", "*.csv"))
	if err != nil {
		t.Fatalf("glob manifests: %v", err)
	}
	var sources []ManifestSource
	for _, path := range paths {
		src, err := readManifest(path)
		if err != nil {
			t.Fatalf("read manifest %s: %v", path, err)
		}
		sources = append(sources, src)
	}
	return sources
}

func singleDirEntry(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("%s entries = %d, want 1 directory", dir, len(entries))
	}
	return filepath.Join(dir, entries[0].Name())
}

func prependPathEnv(pathDir string) string {
	current := os.Getenv("PATH")
	if current == "" {
		return "PATH=" + pathDir
	}
	return "PATH=" + pathDir + string(os.PathListSeparator) + current
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeManifestFixture(t *testing.T, homeDir, machine, dirName string, files map[string]string) string {
	t.Helper()

	scanDir := filepath.Join(homeDir, dirName)
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", scanDir, err)
	}
	for relPath, content := range files {
		fullPath := filepath.Join(scanDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir parent for %s: %v", fullPath, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", fullPath, err)
		}
	}

	cache := make(map[string]CacheEntry)
	fileInfos, _, err := scanDirectory(scanDir, cache, newPhotoIgnore(scanDir))
	if err != nil {
		t.Fatalf("scan fixture dir %s: %v", scanDir, err)
	}

	manifestPath := filepath.Join(homeDir, "manifests", "_Manifest", manifestFilename(machine, scanDir))
	if _, err := updateManifest(scanDir, fileInfos, manifestPath, machine, false); err != nil {
		t.Fatalf("update manifest %s: %v", manifestPath, err)
	}
	return manifestPath
}

func writeManifestFixtureSet(t *testing.T, homeDir string) {
	t.Helper()

	writeManifestFixture(t, homeDir, "machine-a", "photos-a", map[string]string{
		"dup/shared.jpg":      "same-bytes",
		"albums/unique-a.jpg": "only-a",
	})
	writeManifestFixture(t, homeDir, "machine-b", "photos-b", map[string]string{
		"dup/shared.jpg":      "same-bytes",
		"albums/unique-b.jpg": "only-b",
	})
}

func TestMachinesConfigSaveLoad(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cfg := map[string]string{
		"nas":        "admin@nas.local:/mnt/backup",
		"ubuntu-max": "ubuntu@192.168.1.100:/photos",
	}

	if err := saveMachinesConfig(cfg); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}

	loaded := loadMachinesConfig()
	if len(loaded) != len(cfg) {
		t.Fatalf("loaded %d entries, want %d", len(loaded), len(cfg))
	}
	for key, want := range cfg {
		if got := loaded[key]; got != want {
			t.Fatalf("loaded[%q] = %q, want %q", key, got, want)
		}
	}

	data, err := os.ReadFile(machinesConfFile())
	if err != nil {
		t.Fatalf("read machines.conf: %v", err)
	}
	text := string(data)
	if strings.Index(text, "nas") > strings.Index(text, "ubuntu-max") {
		t.Fatalf("machines.conf not sorted:\n%s", text)
	}
}

func TestCLIHelpCurrentSurface(t *testing.T) {
	out, code := runCLI(t, "help")
	if code != 0 {
		t.Fatalf("help exit code = %d, output:\n%s", code, out)
	}

	for _, want := range []string{
		"photo-organizer dups",
		"photo-organizer dup-folders",
		"photo-organizer backup",
		"photo-organizer archive",
		"photo-organizer check-backup",
		"verify-archive",
		"storage-status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\n%s", want, out)
		}
	}

	for _, stale := range []string{
		"photo-organizer analyze",
		"photo-organizer plan",
		"photo-organizer migrate",
		"verify-backup",
		"sign-manifest",
		"repair-manifest",
		"deprecated",
	} {
		if strings.Contains(out, stale) {
			t.Errorf("help output contains stale text %q\n%s", stale, out)
		}
	}
}

func TestCLIOldCommandNamesAreUnknown(t *testing.T) {
	for _, cmd := range []string{
		"analyze",
		"plan",
		"migrate",
		"verify-backup",
		"sign-manifest",
		"repair-manifest",
		"rescan",
		"risk-report",
	} {
		t.Run(cmd, func(t *testing.T) {
			out, code := runCLI(t, cmd)
			if code == 0 {
				t.Fatalf("%s exit code = 0, want nonzero\n%s", cmd, out)
			}
			if !strings.Contains(out, `unknown command "`+cmd+`"`) {
				t.Fatalf("%s output missing unknown-command message:\n%s", cmd, out)
			}
			if strings.Contains(strings.ToLower(out), "deprecated") {
				t.Fatalf("%s output should not mention deprecated commands:\n%s", cmd, out)
			}
		})
	}
}

func TestCLIActiveCommandsRoute(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"dups", []string{"dups", "--help"}, "Usage: photo-organizer dups"},
		{"dup-folders", []string{"dup-folders", "--help"}, "Usage: photo-organizer dup-folders"},
		{"sign", []string{"sign"}, "Usage: photo-organizer sign"},
		{"verify-archive", []string{"verify-archive"}, "Usage: photo-organizer verify-archive"},
		{"fix", []string{"fix"}, "Usage: photo-organizer fix"},
		{"backup-missing", []string{"backup-missing"}, "'backup-missing' is deprecated"},
		{"collect-list", []string{"collect", "--list"}, "No machines configured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := runCLI(t, tt.args...)
			if strings.Contains(out, "unknown command") {
				t.Fatalf("%s routed as unknown command:\n%s", tt.name, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("%s output missing %q:\n%s", tt.name, tt.want, out)
			}
		})
	}
}

func TestCLIUsageAndEmptyStateCoverage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"scan-help", []string{"scan", "--help"}, "photo-organizer — scan, deduplicate, and verify photo backups"},
		{"collect-help", []string{"collect", "--help"}, "Usage: photo-organizer collect"},
		{"backup", []string{"backup"}, "Usage: photo-organizer backup"},
		{"restore", []string{"restore"}, "Usage: photo-organizer restore"},
		{"list", []string{"list"}, "Usage: photo-organizer list"},
		{"lookup", []string{"lookup"}, "Usage: photo-organizer lookup"},
		{"archive", []string{"archive"}, "Usage: photo-organizer archive"},
		{"check-backup", []string{"check-backup"}, "Usage: photo-organizer check-backup"},
		{"sign", []string{"sign"}, "Usage: photo-organizer sign"},
		{"verify-archive", []string{"verify-archive"}, "Usage: photo-organizer verify-archive"},
		{"fix", []string{"fix"}, "Usage: photo-organizer fix"},
		{"machines-empty", []string{"machines"}, "No manifests found"},
		{"manifests-empty", []string{"manifests"}, "No manifests found"},
		{"storage-status-empty", []string{"storage-status"}, "No manifests found"},
		{"storage-plan-empty", []string{"storage-plan"}, "No manifests found"},
		{"lookup-empty", []string{"lookup", "vacation.jpg"}, "No manifests found"},
		{"check-backup-empty", []string{"check-backup", filepath.Join(t.TempDir(), "Photos")}, "No manifests found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := runCLI(t, tt.args...)
			if !strings.Contains(out, tt.want) {
				t.Fatalf("%s output missing %q:\n%s", tt.name, tt.want, out)
			}
		})
	}
}

func TestCLICollectListsConfiguredMachines(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cfg := map[string]string{
		"nas":        "admin@nas.local:/mnt/backup",
		"ubuntu-max": "ubuntu@192.168.1.100:/photos",
	}
	if err := saveMachinesConfig(cfg); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}

	out, code := runCLIWithHome(t, homeDir, "collect", "--list")
	if code != 0 {
		t.Fatalf("collect --list exit code = %d, output:\n%s", code, out)
	}

	for _, want := range []string{
		"Configured machines:",
		"nas",
		"ubuntu-max",
		"admin@nas.local:/mnt/backup",
		"ubuntu@192.168.1.100:/photos",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("collect --list output missing %q:\n%s", want, out)
		}
	}
}

func TestCLIMachinesListsManifestMachines(t *testing.T) {
	homeDir := t.TempDir()
	writeManifestFixtureSet(t, homeDir)

	out, code := runCLIWithHome(t, homeDir, "machines")
	if code != 0 {
		t.Fatalf("machines exit code = %d, output:\n%s", code, out)
	}

	for _, want := range []string{
		"Machines in manifests:",
		"machine-a",
		"machine-b",
		"photos-a",
		"photos-b",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("machines output missing %q:\n%s", want, out)
		}
	}
}

func TestCLIMachinesWriteConfFromManifests(t *testing.T) {
	homeDir := t.TempDir()
	writeManifestFixtureSet(t, homeDir)

	out, code := runCLIWithHome(t, homeDir, "machines", "--write-conf")
	if code != 0 {
		t.Fatalf("machines --write-conf exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Generated machines.conf") {
		t.Fatalf("machines --write-conf missing success output:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(homeDir, "manifests", "machines.conf"))
	if err != nil {
		t.Fatalf("read generated machines.conf: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"# Machine SSH Configuration",
		"machine-a",
		"machine-b",
		"[local] scanned from:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated machines.conf missing %q:\n%s", want, text)
		}
	}
}

func TestCLIStorageReportsUseManifestFixtures(t *testing.T) {
	homeDir := t.TempDir()
	writeManifestFixtureSet(t, homeDir)

	statusOut, statusCode := runCLIWithHome(t, homeDir, "storage-status")
	if statusCode != 0 {
		t.Fatalf("storage-status exit code = %d, output:\n%s", statusCode, statusOut)
	}
	for _, want := range []string{
		"STORAGE STATUS REPORT",
		"machine-a",
		"machine-b",
		"Duplicated files: 1",
	} {
		if !strings.Contains(statusOut, want) {
			t.Fatalf("storage-status output missing %q:\n%s", want, statusOut)
		}
	}

	planOut, planCode := runCLIWithHome(t, homeDir, "storage-plan")
	if planCode != 0 {
		t.Fatalf("storage-plan exit code = %d, output:\n%s", planCode, planOut)
	}
	for _, want := range []string{
		"STORAGE PLANNING REPORT",
		"PRIORITY BACKUPS",
		"machine-a",
		"machine-b",
		"Files at-risk:",
	} {
		if !strings.Contains(planOut, want) {
			t.Fatalf("storage-plan output missing %q:\n%s", want, planOut)
		}
	}
}

func TestCLIManifestsStalledShowsMissingSources(t *testing.T) {
	homeDir := t.TempDir()
	writeManifestFixtureSet(t, homeDir)

	missingDir := filepath.Join(homeDir, "photos-b")
	if err := os.RemoveAll(missingDir); err != nil {
		t.Fatalf("remove %s: %v", missingDir, err)
	}

	out, code := runCLIWithHome(t, homeDir, "manifests", "--stalled")
	if code != 0 {
		t.Fatalf("manifests --stalled exit code = %d, output:\n%s", code, out)
	}

	for _, want := range []string{
		"STALLED MANIFESTS",
		"machine-b",
		missingDir + " (missing)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifests --stalled output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "machine-a") && strings.Contains(out, filepath.Join(homeDir, "photos-a")+" (missing)") {
		t.Fatalf("manifests --stalled incorrectly reported healthy manifest:\n%s", out)
	}
}

func TestCLIManifestsRemoveDeletesMatchingManifest(t *testing.T) {
	homeDir := t.TempDir()
	manifestA := writeManifestFixture(t, homeDir, "machine-a", "photos-a", map[string]string{
		"albums/unique-a.jpg": "only-a",
	})
	manifestB := writeManifestFixture(t, homeDir, "machine-b", "photos-b", map[string]string{
		"albums/unique-b.jpg": "only-b",
	})

	removePath := filepath.Join(homeDir, "photos-a")
	out, code := runCLIWithHome(t, homeDir, "manifests", "--remove", removePath)
	if code != 0 {
		t.Fatalf("manifests --remove exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"Removed manifest:",
		"Removed 1 manifest(s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifests --remove output missing %q:\n%s", want, out)
		}
	}

	if _, err := os.Stat(manifestA); !os.IsNotExist(err) {
		t.Fatalf("removed manifest still exists: %s", manifestA)
	}
	if _, err := os.Stat(manifestB); err != nil {
		t.Fatalf("non-matching manifest missing after remove: %v", err)
	}
}

func TestCLIArchiveMovesFolderAndRewritesManifests(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	archiveRoot := filepath.Join(homeDir, "archive")
	oldManifest := writeManifestFixture(t, homeDir, "test-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})

	if err := os.MkdirAll(filepath.Join(homeDir, "manifests"), 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte("test-machine\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}

	out, code := runCLIWithHomeInput(t, homeDir, "y\n", "archive", sourceDir, "--dest", archiveRoot)
	if code != 0 {
		t.Fatalf("archive exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"ARCHIVE PREVIEW",
		"Cleaning up old manifest entries...",
		"Folder archived to",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("archive output missing %q:\n%s", want, out)
		}
	}

	if _, err := os.Stat(sourceDir); !os.IsNotExist(err) {
		t.Fatalf("source dir still exists after archive: %s", sourceDir)
	}
	if _, err := os.Stat(oldManifest); !os.IsNotExist(err) {
		t.Fatalf("old source manifest still exists: %s", oldManifest)
	}

	archivedDir := singleDirEntry(t, archiveRoot)
	if _, err := os.Stat(filepath.Join(archivedDir, "album", "photo1.jpg")); err != nil {
		t.Fatalf("archived photo missing: %v", err)
	}

	sources := readAllManifestSources(t, homeDir)
	foundArchiveManifest := false
	for _, src := range sources {
		if src.ScanPath != archiveRoot {
			continue
		}
		for _, row := range src.Rows {
			if row.RelativePath == filepath.Join(filepath.Base(archivedDir), "album", "photo1.jpg") {
				foundArchiveManifest = true
				break
			}
		}
	}
	if !foundArchiveManifest {
		t.Fatalf("did not find archive manifest entry for moved photo in %s", archiveRoot)
	}
}

func TestCLIArchiveCancelledLeavesSourceUntouched(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	archiveRoot := filepath.Join(homeDir, "archive")
	oldManifest := writeManifestFixture(t, homeDir, "test-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})

	if err := os.MkdirAll(filepath.Join(homeDir, "manifests"), 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte("test-machine\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}

	out, code := runCLIWithHomeInput(t, homeDir, "n\n", "archive", sourceDir, "--dest", archiveRoot)
	if code != 0 {
		t.Fatalf("archive cancel exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Cancelled.") {
		t.Fatalf("archive cancel output missing cancellation message:\n%s", out)
	}

	if _, err := os.Stat(sourceDir); err != nil {
		t.Fatalf("source dir missing after cancel: %v", err)
	}
	if _, err := os.Stat(oldManifest); err != nil {
		t.Fatalf("manifest missing after cancel: %v", err)
	}
	entries, err := os.ReadDir(archiveRoot)
	if err != nil {
		t.Fatalf("read archive root after cancel: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("archive root should be empty after cancel")
	}
}

// fakeRsync copies files listed by --files-from into the destination path,
// standing in for a real rsync over SSH.
const fakeRsync = "#!/bin/sh\nset -eu\nfiles_from=\nsrc=\ndest=\nfor arg in \"$@\"; do\n  case \"$arg\" in\n    --files-from=*) files_from=${arg#--files-from=} ;;\n    -*) ;;\n    *)\n      if [ -z \"$src\" ]; then src=$arg; elif [ -z \"$dest\" ]; then dest=$arg; fi ;;\n  esac\ndone\nremote_path=${dest#*:}\nmkdir -p \"$remote_path\"\nwhile IFS= read -r rel; do\n  mkdir -p \"$remote_path/$(dirname \"$rel\")\"\n  cp \"$src$rel\" \"$remote_path/$rel\"\ndone < \"$files_from\"\n"

// setupRemoteShims installs fake rsync/ssh/photo-organizer binaries and returns
// the env entry that puts them on PATH.
func setupRemoteShims(t *testing.T, homeDir, rsyncScript string) string {
	t.Helper()

	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "rsync"), rsyncScript)
	writeExecutable(t, filepath.Join(binDir, "ssh"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(binDir, "photo-organizer"), "#!/bin/sh\nexit 0\n")
	return prependPathEnv(binDir)
}

func writeMachineID(t *testing.T, homeDir, machineID string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(homeDir, "manifests"), 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte(machineID+"\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}
}

// =============================================================================
// backup — mirrors a folder, converging on every run
// =============================================================================

func TestCLIBackupMirrorsToLocalDest(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	destDir := filepath.Join(homeDir, "external", "photos")

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
	})
	writeMachineID(t, homeDir, "local-machine")

	out, code := runCLIWithHome(t, homeDir, "backup", sourceDir, "--dest", destDir)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Files copied:        2") {
		t.Fatalf("backup output missing copied count:\n%s", out)
	}

	// The destination mirrors the source tree: no timestamped folder in between.
	for relPath, want := range map[string]string{
		filepath.Join("album", "photo1.jpg"): "photo-one",
		filepath.Join("album", "photo2.jpg"): "photo-two",
	} {
		data, err := os.ReadFile(filepath.Join(destDir, relPath))
		if err != nil {
			t.Fatalf("read mirrored file %s: %v", relPath, err)
		}
		if string(data) != want {
			t.Fatalf("mirrored file %s = %q, want %q", relPath, string(data), want)
		}
	}

	// The destination is scanned, so its copies are visible to other commands.
	foundDestManifest := false
	for _, src := range readAllManifestSources(t, homeDir) {
		if src.ScanPath == destDir {
			foundDestManifest = true
		}
	}
	if !foundDestManifest {
		t.Fatalf("no manifest was written for the backup destination %s", destDir)
	}

	// Running again converges: everything is already there.
	out, code = runCLIWithHome(t, homeDir, "backup", sourceDir, "--dest", destDir)
	if code != 0 {
		t.Fatalf("second backup exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Nothing to copy") {
		t.Fatalf("second backup should be a no-op:\n%s", out)
	}
	if entries, err := os.ReadDir(destDir); err != nil || len(entries) != 1 {
		t.Fatalf("destination should hold only the mirrored tree, got %v (err %v)", entries, err)
	}
}

func TestCLIBackupSkipsFilesBackedUpElsewhere(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	destDir := filepath.Join(homeDir, "external", "photos")

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/shared.jpg": "shared-photo",
		"album/unique.jpg": "unique-photo",
	})
	writeManifestFixture(t, homeDir, "other-machine", filepath.Join("mirror", "Photos"), map[string]string{
		"album/shared.jpg": "shared-photo",
	})
	writeMachineID(t, homeDir, "local-machine")

	out, code := runCLIWithHome(t, homeDir, "backup", sourceDir, "--dest", destDir)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Files copied:        1") || !strings.Contains(out, "Backed up elsewhere: 1") {
		t.Fatalf("backup output missing dedup counts:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(destDir, "album", "unique.jpg")); err != nil {
		t.Fatalf("unique file missing from backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "album", "shared.jpg")); !os.IsNotExist(err) {
		t.Fatalf("file backed up on another machine should be skipped")
	}

	// --all ignores the dedup index and copies everything.
	out, code = runCLIWithHome(t, homeDir, "backup", sourceDir, "--dest", destDir, "--all")
	if code != 0 {
		t.Fatalf("backup --all exit code = %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(destDir, "album", "shared.jpg")); err != nil {
		t.Fatalf("--all should copy files backed up elsewhere: %v", err)
	}
}

func TestCLIBackupNoOpWhenAlreadyBackedUp(t *testing.T) {
	homeDir := t.TempDir()
	localDir := filepath.Join(homeDir, "library", "Photos")
	shared := map[string]string{
		"album/photo1.jpg": "photo-one",
	}
	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), shared)
	writeManifestFixture(t, homeDir, "remote-machine", filepath.Join("remote-backup", "Photos"), shared)
	writeMachineID(t, homeDir, "local-machine")

	destDir := filepath.Join(homeDir, "external", "photos")
	out, code := runCLIWithHome(t, homeDir, "backup", localDir, "--dest", destDir)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "Nothing to copy") || !strings.Contains(out, "already backed up elsewhere") {
		t.Fatalf("backup output missing no-op message:\n%s", out)
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Fatalf("no-op backup should not write anything, got %v", entries)
	}
}

func TestCLIBackupSameMachineCopyDoesNotCountAsBackup(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("other-local-copy", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeMachineID(t, homeDir, "local-machine")
	pathEnv := setupRemoteShims(t, homeDir, fakeRsync)

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{pathEnv}, "backup", sourceDir, "--dest", "backup@example:"+remoteRoot)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "manifest refresh was skipped") {
		t.Fatalf("backup output missing direct-host warning:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "album", "photo1.jpg")); err != nil {
		t.Fatalf("file was not copied when only same-machine duplicate existed: %v", err)
	}
}

// loggingRsync behaves like fakeRsync but also appends its file list to
// $RSYNC_LOG, one line per invocation, so a test can count batches.
const loggingRsync = "#!/bin/sh\nset -eu\nfiles_from=\nsrc=\ndest=\nfor arg in \"$@\"; do\n  case \"$arg\" in\n    --files-from=*) files_from=${arg#--files-from=} ;;\n    --dry-run) exit 0 ;;\n    -*) ;;\n    *)\n      if [ -z \"$src\" ]; then src=$arg; elif [ -z \"$dest\" ]; then dest=$arg; fi ;;\n  esac\ndone\nremote_path=${dest#*:}\nmkdir -p \"$remote_path\"\nbatch=\nwhile IFS= read -r rel; do\n  mkdir -p \"$remote_path/$(dirname \"$rel\")\"\n  cp \"$src$rel\" \"$remote_path/$rel\"\n  batch=\"$batch $rel\"\ndone < \"$files_from\"\necho \"batch:$batch\" >> \"$RSYNC_LOG\"\n"

// A remote backup must split its file list into byte-bounded batches and still
// deliver every file exactly once.
func TestCLIBackupToRemoteBatchesFileList(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	rsyncLog := filepath.Join(homeDir, "rsync.log")
	t.Setenv("HOME", homeDir)

	files := map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
		"album/photo3.jpg": "photo-three",
		"album/photo4.jpg": "photo-four",
		"album/photo5.jpg": "photo-five",
	}
	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), files)
	writeMachineID(t, homeDir, "local-machine")
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	pathEnv := setupRemoteShims(t, homeDir, loggingRsync)

	// Two files per batch over five files means three invocations.
	out, code := runCLIWithHomeInputEnv(t, homeDir, "",
		[]string{pathEnv, "RSYNC_LOG=" + rsyncLog, "PHOTO_ORGANIZER_RSYNC_BATCH_FILES=2"},
		"backup", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}

	log, err := os.ReadFile(rsyncLog)
	if err != nil {
		t.Fatalf("read rsync log: %v", err)
	}
	batches := strings.Count(string(log), "batch:")
	if batches != 3 {
		t.Errorf("rsync invoked %d times, want 3 (2 files per batch over 5 files)\nlog:\n%s", batches, log)
	}

	// Batching must not lose or duplicate anything.
	for rel, want := range files {
		data, err := os.ReadFile(filepath.Join(remoteRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read remote %s: %v", rel, err)
		}
		if string(data) != want {
			t.Errorf("remote %s = %q, want %q", rel, data, want)
		}
		if got := strings.Count(string(log), " "+rel); got != 1 {
			t.Errorf("%s appeared in %d batches, want 1", rel, got)
		}
	}
}

func TestCLIBackupToRemoteMirrorsWithConfiguredMachine(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
	})
	writeMachineID(t, homeDir, "local-machine")
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	pathEnv := setupRemoteShims(t, homeDir, fakeRsync)

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{pathEnv}, "backup", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"Checking which files need backup...",
		"Backing up 2 files",
		"Backup complete",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("backup output missing %q:\n%s", want, out)
		}
	}

	// Remote backups mirror too — no dated folder in the path.
	for relPath, want := range map[string]string{
		filepath.Join("album", "photo1.jpg"): "photo-one",
		filepath.Join("album", "photo2.jpg"): "photo-two",
	} {
		data, err := os.ReadFile(filepath.Join(remoteRoot, relPath))
		if err != nil {
			t.Fatalf("read remote copied file %s: %v", relPath, err)
		}
		if string(data) != want {
			t.Fatalf("remote copied file %s = %q, want %q", relPath, string(data), want)
		}
	}
}

func TestCLIBackupToRemoteFailsWhenRsyncFails(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeMachineID(t, homeDir, "local-machine")
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	pathEnv := setupRemoteShims(t, homeDir, "#!/bin/sh\nexit 12\n")

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{pathEnv}, "backup", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code == 0 {
		t.Fatalf("backup exit code = 0, want nonzero\n%s", out)
	}
	if !strings.Contains(out, "Error: rsync failed") {
		t.Fatalf("backup failure output missing rsync error:\n%s", out)
	}
	if _, err := os.Stat(remoteRoot); !os.IsNotExist(err) {
		t.Fatalf("remote root should not exist after rsync failure")
	}
}

func TestCLIBackupToRemoteFailsWhenCollectFailsAfterCopy(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeMachineID(t, homeDir, "local-machine")
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	pathEnv := setupRemoteShims(t, homeDir, fakeRsync)
	writeExecutable(t, filepath.Join(homeDir, "bin", "photo-organizer"), "#!/bin/sh\nexit 9\n")

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{pathEnv}, "backup", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code == 0 {
		t.Fatalf("collect-failure exit code = 0, want nonzero\n%s", out)
	}
	if !strings.Contains(out, "manifest collection failed") {
		t.Fatalf("collect-failure output missing message:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "album", "photo1.jpg")); err != nil {
		t.Fatalf("file copy should complete before collect failure: %v", err)
	}
}

func TestCLIBackupRemoteUnknownMachineDoesNotCreateLocalDir(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}

	out, code := runCLIWithHome(t, homeDir, "backup", sourceDir, "--dest", "nope:/backups")
	if code == 0 {
		t.Fatalf("unknown machine exit code = 0, want nonzero\n%s", out)
	}
	for _, want := range []string{"not found in machines.conf", "Available options", "backup@example"} {
		if !strings.Contains(out, want) {
			t.Fatalf("unknown machine output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(homeDir, "nope:")); !os.IsNotExist(err) {
		t.Fatalf("remote-looking dest must not create a local directory")
	}
}

func TestCLIBackupMissingAliasForwardsToBackup(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	destDir := filepath.Join(homeDir, "external", "photos")

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeMachineID(t, homeDir, "local-machine")

	out, code := runCLIWithHome(t, homeDir, "backup-missing", sourceDir, "--dest", destDir)
	if code != 0 {
		t.Fatalf("backup-missing exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "'backup-missing' is deprecated") {
		t.Fatalf("backup-missing output missing deprecation notice:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(destDir, "album", "photo1.jpg")); err != nil {
		t.Fatalf("deprecated alias did not copy the file: %v", err)
	}
}

// =============================================================================
// archive — dated snapshots, local and remote
// =============================================================================

func TestCLIArchiveKeepCopiesAndRestores(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	archiveRoot := filepath.Join(homeDir, "archive")
	restoreDir := filepath.Join(homeDir, "restored")

	writeManifestFixture(t, homeDir, "test-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
	})
	writeMachineID(t, homeDir, "test-machine")

	out, code := runCLIWithHomeInput(t, homeDir, "y\n", "archive", sourceDir, "--dest", archiveRoot, "--keep")
	if code != 0 {
		t.Fatalf("archive --keep exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"ARCHIVE PREVIEW",
		"Source folder is kept",
		"Folder archived to",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("archive --keep output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "album", "photo1.jpg")); err != nil {
		t.Fatalf("--keep must leave the source in place: %v", err)
	}

	// The snapshot folder is dated, so repeat runs never collide.
	archiveDir := singleDirEntry(t, archiveRoot)
	if !archiveTimestampPrefix.MatchString(filepath.Base(archiveDir)) {
		t.Fatalf("archive folder %q is not timestamped", filepath.Base(archiveDir))
	}

	restoreOut, restoreCode := runCLIWithHome(t, homeDir, "restore", archiveDir, restoreDir)
	if restoreCode != 0 {
		t.Fatalf("restore exit code = %d, output:\n%s", restoreCode, restoreOut)
	}
	for _, want := range []string{
		"RESTORE FROM ARCHIVE",
		"RESTORE COMPLETE",
		"Files restored: 2",
		"All files successfully restored",
	} {
		if !strings.Contains(restoreOut, want) {
			t.Fatalf("restore output missing %q:\n%s", want, restoreOut)
		}
	}
	for relPath, want := range map[string]string{
		filepath.Join("album", "photo1.jpg"): "photo-one",
		filepath.Join("album", "photo2.jpg"): "photo-two",
	} {
		data, err := os.ReadFile(filepath.Join(restoreDir, relPath))
		if err != nil {
			t.Fatalf("read restored file %s: %v", relPath, err)
		}
		if string(data) != want {
			t.Fatalf("restored file %s = %q, want %q", relPath, string(data), want)
		}
	}
}

func TestCLIArchiveToRemoteCreatesDatedSnapshot(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-archive")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	writeMachineID(t, homeDir, "local-machine")
	if err := saveMachinesConfig(map[string]string{"remote-machine": "backup@example"}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	pathEnv := setupRemoteShims(t, homeDir, fakeRsync)

	out, code := runCLIWithHomeInputEnv(t, homeDir, "y\n", []string{pathEnv}, "archive", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code != 0 {
		t.Fatalf("remote archive exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{"ARCHIVE PREVIEW", "Folder archived to", "Source folder was kept"} {
		if !strings.Contains(out, want) {
			t.Fatalf("remote archive output missing %q:\n%s", want, out)
		}
	}

	archiveDir := singleDirEntry(t, remoteRoot)
	if !archiveTimestampPrefix.MatchString(filepath.Base(archiveDir)) {
		t.Fatalf("remote archive folder %q is not timestamped", filepath.Base(archiveDir))
	}
	data, err := os.ReadFile(filepath.Join(archiveDir, "album", "photo1.jpg"))
	if err != nil {
		t.Fatalf("read remote archived file: %v", err)
	}
	if string(data) != "photo-one" {
		t.Fatalf("remote archived file = %q, want %q", string(data), "photo-one")
	}

	// Archiving across the network never deletes the source.
	if _, err := os.Stat(filepath.Join(sourceDir, "album", "photo1.jpg")); err != nil {
		t.Fatalf("remote archive must leave the source in place: %v", err)
	}
}

var archiveTimestampPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{6}-`)

func TestDocsUseCurrentCommandExamples(t *testing.T) {
	stale := regexp.MustCompile(`photo-organizer\s+(analyze|plan|migrate|rescan|risk-report|verify-backup|sign-manifest|repair-manifest|backup-missing)\b`)
	files, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if match := stale.Find(data); match != nil {
			t.Fatalf("%s contains stale command example %q", path, string(match))
		}
	}
}
