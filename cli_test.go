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
		"photo-organizer backup-missing",
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
		{"backup-missing", []string{"backup-missing"}, "Usage: photo-organizer backup-missing"},
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

func TestCLIBackupAndRestoreWorkflow(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	archiveRoot := filepath.Join(homeDir, "backups")
	restoreDir := filepath.Join(homeDir, "restored")

	writeManifestFixture(t, homeDir, "test-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
	})

	backupOut, backupCode := runCLIWithHome(t, homeDir, "backup", sourceDir, archiveRoot)
	if backupCode != 0 {
		t.Fatalf("backup exit code = %d, output:\n%s", backupCode, backupOut)
	}
	for _, want := range []string{
		"BACKUP WORKFLOW",
		"BACKUP COMPLETE",
		"Files backed up: 2",
		"All files successfully backed up",
	} {
		if !strings.Contains(backupOut, want) {
			t.Fatalf("backup output missing %q:\n%s", want, backupOut)
		}
	}

	archiveDir := singleDirEntry(t, archiveRoot)
	for _, relPath := range []string{
		filepath.Join("album", "photo1.jpg"),
		filepath.Join("album", "photo2.jpg"),
	} {
		if _, err := os.Stat(filepath.Join(archiveDir, relPath)); err != nil {
			t.Fatalf("archived file missing %s: %v", relPath, err)
		}
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

func TestCLIBackupMissingNoOpWhenAlreadyBackedUp(t *testing.T) {
	homeDir := t.TempDir()
	localDir := filepath.Join(homeDir, "library", "Photos")
	shared := map[string]string{
		"album/photo1.jpg": "photo-one",
	}
	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), shared)
	writeManifestFixture(t, homeDir, "remote-machine", filepath.Join("remote-backup", "Photos"), shared)
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte("local-machine\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}

	out, code := runCLIWithHome(t, homeDir, "backup-missing", localDir, "--dest", "unused:/backups")
	if code != 0 {
		t.Fatalf("backup-missing exit code = %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "All files already backed up") {
		t.Fatalf("backup-missing output missing no-op message:\n%s", out)
	}
}

func TestCLIBackupMissingFailsWhenRsyncFails(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	binDir := filepath.Join(homeDir, "bin")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
	})
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte("local-machine\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}
	if err := saveMachinesConfig(map[string]string{
		"remote-machine": "backup@example",
	}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for name, content := range map[string]string{
		"rsync":           "#!/bin/sh\nexit 12\n",
		"ssh":             "#!/bin/sh\nexit 0\n",
		"photo-organizer": "#!/bin/sh\nexit 0\n",
	} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write shim %s: %v", name, err)
		}
	}

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{prependPathEnv(binDir)}, "backup-missing", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code == 0 {
		t.Fatalf("backup-missing exit code = 0, want nonzero\n%s", out)
	}
	if !strings.Contains(out, "Error: rsync failed") {
		t.Fatalf("backup-missing failure output missing rsync error:\n%s", out)
	}
	if _, err := os.Stat(remoteRoot); !os.IsNotExist(err) {
		t.Fatalf("remote root should not exist after rsync failure")
	}
}

func TestCLIBackupMissingCopiesMissingFilesWithConfiguredMachine(t *testing.T) {
	homeDir := t.TempDir()
	sourceDir := filepath.Join(homeDir, "library", "Photos")
	remoteRoot := filepath.Join(homeDir, "remote-dest")
	binDir := filepath.Join(homeDir, "bin")
	t.Setenv("HOME", homeDir)

	writeManifestFixture(t, homeDir, "local-machine", filepath.Join("library", "Photos"), map[string]string{
		"album/photo1.jpg": "photo-one",
		"album/photo2.jpg": "photo-two",
	})
	if err := os.WriteFile(filepath.Join(homeDir, "manifests", "machine-id"), []byte("local-machine\n"), 0o644); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}
	if err := saveMachinesConfig(map[string]string{
		"remote-machine": "backup@example",
	}); err != nil {
		t.Fatalf("saveMachinesConfig: %v", err)
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	rsyncScript := "#!/bin/sh\nset -eu\nfiles_from=\nsrc=\ndest=\nfor arg in \"$@\"; do\n  case \"$arg\" in\n    --files-from=*) files_from=${arg#--files-from=} ;;\n    -*) ;;\n    *)\n      if [ -z \"$src\" ]; then\n        src=$arg\n      elif [ -z \"$dest\" ]; then\n        dest=$arg\n      fi\n      ;;\n  esac\ndone\nremote_path=${dest#*:}\nmkdir -p \"$remote_path\"\nwhile IFS= read -r rel; do\n  mkdir -p \"$remote_path/$(dirname \"$rel\")\"\n  cp \"$src$rel\" \"$remote_path/$rel\"\ndone < \"$files_from\"\n"
	sshScript := "#!/bin/sh\nexit 0\n"
	collectScript := "#!/bin/sh\nexit 0\n"
	for name, content := range map[string]string{
		"rsync":           rsyncScript,
		"ssh":             sshScript,
		"photo-organizer": collectScript,
	} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write shim %s: %v", name, err)
		}
	}

	out, code := runCLIWithHomeInputEnv(t, homeDir, "", []string{prependPathEnv(binDir)}, "backup-missing", sourceDir, "--dest", "remote-machine:"+remoteRoot)
	if code != 0 {
		t.Fatalf("backup-missing exit code = %d, output:\n%s", code, out)
	}
	for _, want := range []string{
		"Checking which files need backup...",
		"Backing up 2 files",
		"Backup complete",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("backup-missing output missing %q:\n%s", want, out)
		}
	}

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

func TestDocsUseCurrentCommandExamples(t *testing.T) {
	stale := regexp.MustCompile(`photo-organizer\s+(analyze|plan|migrate|rescan|risk-report|verify-backup|sign-manifest|repair-manifest)\b`)
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
