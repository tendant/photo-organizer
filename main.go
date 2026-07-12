// photo-organizer: scan a folder and generate a photo manifest CSV.
//
// Usage:
//
//	photo-organizer [directory]            scan directory, manifest written inside it
//	photo-organizer scan [directory]       explicit scan subcommand
//	photo-organizer dups a.csv b.csv       compare manifests across machines
package main

import (
	"fmt"
	"os"
	"strings"
)

// =============================================================================
// Helper Functions for Consistency
// =============================================================================

// confirmPrompt asks user for y/n confirmation
func confirmPrompt(message string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", message)
	var response string
	fmt.Scanln(&response)
	return response == "y" || response == "Y"
}

// userHomeDir returns the user's home directory (cross-platform safe)
func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot determine home directory: %v\n", err)
		return "."
	}
	return home
}

// =============================================================================
// Supported File Types
// =============================================================================

// suggestCommand finds similar command names for typo suggestions
func suggestCommand(typo string) string {
	commands := []string{
		"archive", "backup", "backup-missing", "check-backup", "collect",
		"dups", "dup-folders", "fix", "help", "list", "lookup",
		"machines", "manifests", "restore", "scan", "search", "sign",
		"storage-plan", "storage-status", "verify-archive",
	}

	// Simple similarity: count matching prefixes and characters
	bestMatch := ""
	bestScore := 0

	for _, cmd := range commands {
		score := 0

		// Bonus for common prefix
		for i := 0; i < len(typo) && i < len(cmd); i++ {
			if typo[i] == cmd[i] {
				score += 2
			}
		}

		// Bonus if typo appears as substring
		if strings.Contains(cmd, typo) || strings.Contains(typo, cmd) {
			score += 3
		}

		// Penalize based on length difference
		lenDiff := len(cmd) - len(typo)
		if lenDiff < 0 {
			lenDiff = -lenDiff
		}
		score -= lenDiff

		if score > bestScore {
			bestScore = score
			bestMatch = cmd
		}
	}

	// Only suggest if there's a reasonable match
	if bestScore > 2 {
		return bestMatch
	}
	return ""
}

// =============================================================================

func main() {
	// Check for interrupted operations at startup
	DetectInterruptedOperations()

	if len(os.Args) >= 2 {
		cmd := os.Args[1]
		switch cmd {
		// Core workflow commands (14 total)
		case "scan":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "dups":
			runAnalyze(os.Args[2:])
			return
		case "dup-folders":
			runFindDuplicateFolders(os.Args[2:])
			return
		case "storage-status":
			runStorageStatus(os.Args[2:])
			return
		case "storage-plan":
			runStoragePlan(os.Args[2:])
			return
		case "backup":
			runBackup(os.Args[2:])
			return
		case "restore":
			runRestore(os.Args[2:])
			return
		case "list":
			runListArchives(os.Args[2:])
			return
		case "verify-archive":
			runVerifyBackup(os.Args[2:])
			return
		case "check-backup":
			runCheckBackupStatus(os.Args[2:])
			return
		case "sign":
			runSignManifest(os.Args[2:])
			return
		case "fix":
			runRepairManifest(os.Args[2:])
			return
		case "archive":
			runArchive(os.Args[2:])
			return
		case "manifests":
			runManifests(os.Args[2:])
			return
		case "machines":
			runMachines(os.Args[2:])
			return
		case "lookup":
			runLookup(os.Args[2:])
			return
		case "search":
			runSearch(os.Args[2:])
			return
		case "collect":
			runCollect(os.Args[2:])
			return
		case "backup-missing":
			runBackupMissing(os.Args[2:])
			return

		case "help", "--help", "-h":
			printUsage()
			return
		default:
			// Check if it looks like a command (not a directory path)
			if !strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, ".") {
				// Might be a typo'd command, suggest similar ones
				suggested := suggestCommand(cmd)
				fmt.Fprintf(os.Stderr, "Error: unknown command %q\n", cmd)
				if suggested != "" {
					fmt.Fprintf(os.Stderr, "Did you mean: %s?\n\n", suggested)
				}
				fmt.Fprintf(os.Stderr, "Run 'photo-organizer help' for available commands.\n")
				os.Exit(1)
			}
			// Looks like a directory path, try to scan it
		}
		runScan(os.Args[1:])
		return
	}
	printUsage()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "photo-organizer — scan, deduplicate, and verify photo backups\n\n")
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan <folder> [--media-id ID] [--prune]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dups [manifest.csv ...]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders [--top N] [-s]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup <folder> <archive-root> [--new-only]\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup-missing <folder> --dest <target>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer check-backup <folder>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer verify-archive <manifest.csv>\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer search [filters]\n\n")
	fmt.Fprintf(os.Stderr, "Other commands:\n")
	fmt.Fprintf(os.Stderr, "  archive, collect, fix, list, lookup, machines, manifests,\n")
	fmt.Fprintf(os.Stderr, "  restore, sign, storage-plan, storage-status\n\n")
	fmt.Fprintf(os.Stderr, "Examples:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer scan ~/Photos\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer backup ~/Photos /mnt/archive\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer dup-folders --top 10 -s\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer check-backup ~/Photos\n")
}
