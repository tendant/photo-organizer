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
	return runCLIWithHome(t, t.TempDir(), args...)
}

func runCLIWithHome(t *testing.T, homeDir string, args ...string) (string, int) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestCLIMainHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Dir = "."
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
