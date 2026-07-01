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
	return runCLIWithHomeInput(t, t.TempDir(), "", args...)
}

func runCLIWithHome(t *testing.T, homeDir string, args ...string) (string, int) {
	t.Helper()
	return runCLIWithHomeInput(t, homeDir, "", args...)
}

func runCLIWithHomeInput(t *testing.T, homeDir, stdin string, args ...string) (string, int) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestCLIMainHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Dir = "."
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"PHOTO_ORGANIZER_TEST_MAIN=1",
		"PHOTO_ORGANIZER_CHECKPOINT_DIR="+filepath.Join(homeDir, "checkpoints"),
	)
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

	entries, err := os.ReadDir(archiveRoot)
	if err != nil {
		t.Fatalf("read archive root: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("archive root entries = %d, want 1 archived folder", len(entries))
	}
	archivedDir := filepath.Join(archiveRoot, entries[0].Name())
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
			if row.RelativePath == filepath.Join(entries[0].Name(), "album", "photo1.jpg") {
				foundArchiveManifest = true
				break
			}
		}
	}
	if !foundArchiveManifest {
		t.Fatalf("did not find archive manifest entry for moved photo in %s", archiveRoot)
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
