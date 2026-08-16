package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func resolveExistingFolder(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

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

func hasIndependentBackup(sources []ManifestSource, idx map[string][]hashLocation, machinesCfg map[string]string, machineName, partialHash string, sizeBytes int64) bool {
	locs := idx[indexKey(partialHash, sizeBytes)]
	for _, loc := range locs {
		src := sources[loc.sourceIdx]
		if src.MachineName == machineName {
			continue
		}
		if isRemovableSource(src.MachineName, src.ScanPath, machinesCfg) {
			continue
		}
		return true
	}
	return false
}

func resolveBackupDestination(destLocation string, machines map[string]string) (remoteUserHost, remoteMachineID, remotePath string, err error) {
	parts := strings.SplitN(destLocation, ":", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid destination format. Use: machine-id:/path or user@host:/path")
	}

	destIdentifier := parts[0]
	remotePath = parts[1]

	if sshHost, exists := machines[destIdentifier]; exists {
		return sshHost, destIdentifier, remotePath, nil
	}

	if strings.Contains(destIdentifier, "@") {
		remoteUserHost = destIdentifier
		for machID, sshHost := range machines {
			if sshHost == remoteUserHost {
				return remoteUserHost, machID, remotePath, nil
			}
		}
		return remoteUserHost, "", remotePath, nil
	}

	for machID, sshHost := range machines {
		if sshHost == destIdentifier {
			return sshHost, machID, remotePath, nil
		}
	}

	return "", "", "", fmt.Errorf("%q not found in machines.conf", destIdentifier)
}

// isRemoteDestination reports whether dest names an SSH target
// (machine-id:/path or user@host:/path) rather than a local directory.
// A local path never has a colon before its first slash.
func isRemoteDestination(dest string) bool {
	colon := strings.Index(dest, ":")
	if colon <= 0 || strings.HasPrefix(dest, ".") || strings.HasPrefix(dest, "~") {
		return false
	}
	return !strings.Contains(dest[:colon], "/")
}

func printBackupDestinationOptions(machines map[string]string) {
	fmt.Fprintf(os.Stderr, "Available options:\n")
	for machID, sshHost := range machines {
		if !strings.HasPrefix(sshHost, "[removable]") {
			fmt.Fprintf(os.Stderr, "  - %s (machine-id: %s)\n", sshHost, machID)
		}
	}
}

// remoteArchivePath joins a remote archive root and a snapshot folder name.
// Remote paths are always POSIX, so filepath.Join is not used here.
func remoteArchivePath(remoteRoot, folderName string) string {
	return strings.TrimSuffix(remoteRoot, "/") + "/" + folderName
}

// listFilesUnder returns every backup-worthy file under root, as paths relative
// to root, along with each file's size and their total. sizes is parallel to
// relPaths — the copy phase accounts progress in bytes, and rsync batches are
// bounded by bytes, so both need per-file sizes rather than just the total.
func listFilesUnder(root string) (relPaths []string, sizes []int64, totalSize int64) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == root || shouldSkipFile(path) {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		var size int64
		if info, err := d.Info(); err == nil {
			size = info.Size()
			totalSize += size
		}
		relPaths = append(relPaths, relPath)
		sizes = append(sizes, size)
		return nil
	})

	return relPaths, sizes, totalSize
}

// copyFile copies a single file from source to destination, preserving mode.
func copyFile(src, dst string) error {
	return copyFileProgress(src, dst, nil)
}

// copyFileProgress is copyFile with byte-level progress reporting. prog may be
// nil. WHY byte-level rather than one tick per file: a single 4GB video would
// otherwise look identical to a stall.
func copyFileProgress(src, dst string, prog *progressReporter) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(&progressWriter{dstFile, prog}, srcFile); err != nil {
		return err
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}

// refreshLocalManifest rescans a local destination so copies there show up in
// dups, check-backup, and storage-status.
func refreshLocalManifest(destPath string) error {
	machineName := resolveMachineID("")
	manifestFile := filepath.Join(userHomeDir(), "manifests", "_Manifest", manifestFilename(machineName, destPath))

	// WHY loadCache: without it every backup re-hashes the entire destination,
	// which on a converged mirror is the slowest part of the whole run and is
	// pure waste — it re-reads the files it just wrote. Same size+path caching
	// rule that `scan` already relies on, with the same tradeoff: a same-size
	// in-place edit at the destination is missed until a full rescan.
	files, _, err := scanDirectory(destPath, loadCache(manifestFile), nil)
	if err != nil {
		return err
	}

	_, err = updateManifest(destPath, files, manifestFile, machineName, false)
	return err
}

// Batches are bounded PRIMARILY BY BYTES: bytes are what the progress line and
// the ETA are denominated in, so a byte-bounded batch is a bounded step on the
// bar. The file cap is only a safety valve against a pathological run of tiny
// files making the --files-from list absurdly long — it is not the primary
// bound and must never be tightened to the point where it, rather than bytes,
// decides batch size.
//
// WHY batch at all, rather than parse rsync's own --info=progress2: that flag
// needs rsync 3.1+, and stock macOS ships openrsync, which lacks it.
//
// Batching does NOT weaken the rsync algorithm. Delta-transfer is negotiated
// per file, so every batch still does full block-level reuse against whatever
// is already at the destination, and -a/-t still quick-check and skip unchanged
// files. What IS invocation-scoped: --delete (NEVER add it here — each batch
// would delete everything outside its own list), -H hardlinks, --link-dest, and
// the -z compression stream. None are used today.
const (
	rsyncBatchBytes int64 = 256 << 20 // ~1% steps on a 25GB backup
	rsyncBatchFiles       = 2000      // safety valve only
)

// batchBounds resolves the batch limits. The env overrides exist so tests can
// force many small batches, and so an unusual link can trade round trips for
// finer progress steps.
func batchBounds() (maxBytes int64, maxFiles int) {
	maxBytes, maxFiles = rsyncBatchBytes, rsyncBatchFiles
	if v, err := strconv.ParseInt(os.Getenv("PHOTO_ORGANIZER_RSYNC_BATCH_BYTES"), 10, 64); err == nil && v > 0 {
		maxBytes = v
	}
	if v, err := strconv.Atoi(os.Getenv("PHOTO_ORGANIZER_RSYNC_BATCH_FILES")); err == nil && v > 0 {
		maxFiles = v
	}
	return maxBytes, maxFiles
}

// rsyncRelPaths copies the given srcRoot-relative paths to remoteUserHost:remotePath,
// creating the destination tree as needed. relSizes is parallel to relPaths and
// may be nil, in which case progress is reported in files rather than bytes —
// pair a nil relSizes with a unitFiles reporter. prog may be nil.
//
// plan, when non-nil, is the set of paths rsync said it would actually send (see
// rsyncPlanTransfers). It narrows PROGRESS ACCOUNTING ONLY: every path in
// relPaths is still handed to rsync, so the decision about what needs sending
// stays rsync's. Files outside the plan contribute nothing to the numerator,
// matching a denominator computed the same way.
func rsyncRelPaths(srcRoot string, relPaths []string, relSizes []int64,
	remoteUserHost, remotePath string, plan map[string]bool, prog *progressReporter) error {

	// One SSH connection shared by every batch. WHY: batching otherwise costs a
	// handshake per batch; multiplexing collapses them, so batch size becomes a
	// progress-granularity knob rather than a performance tradeoff. If the
	// control socket cannot be set up, ssh just opens a normal connection.
	rsh := ""
	if ctlDir, err := os.MkdirTemp("", "photo-organizer-ssh-*"); err == nil {
		defer os.RemoveAll(ctlDir)
		rsh = "ssh -o ControlMaster=auto -o ControlPath=" +
			filepath.Join(ctlDir, "cm-%C") + " -o ControlPersist=60s"
	}

	maxBytes, maxFiles := batchBounds()

	for start := 0; start < len(relPaths); {
		end, _ := rsyncBatchEnd(relSizes, start, len(relPaths), maxBytes, maxFiles)

		// A lone oversized file would otherwise freeze the line for its whole
		// transfer, which is exactly the stall this reporting exists to remove.
		if end == start+1 && relSizes != nil && relSizes[start] >= maxBytes &&
			inPlan(plan, relPaths[start]) {
			if err := rsyncOneLarge(srcRoot, relPaths[start], relSizes[start],
				remoteUserHost, remotePath, rsh, prog); err != nil {
				return err
			}
			prog.Add(0, 1)
			start = end
			continue
		}

		if err := rsyncBatch(srcRoot, relPaths[start:end], remoteUserHost, remotePath, rsh, nil); err != nil {
			return err
		}
		amount, items := plannedAmount(relPaths[start:end], relSizes, start, plan)
		prog.Add(amount, items)
		start = end
	}
	return nil
}

// inPlan reports whether rel is work rsync will actually do. A nil plan means
// the question could not be answered, so assume yes.
func inPlan(plan map[string]bool, rel string) bool {
	return plan == nil || plan[rel]
}

// plannedAmount totals the progress a finished batch should be credited with,
// counting only files rsync said it would send.
func plannedAmount(batch []string, relSizes []int64, offset int, plan map[string]bool) (amount, items int64) {
	for i, rel := range batch {
		if !inPlan(plan, rel) {
			continue
		}
		items++
		if relSizes != nil {
			amount += relSizes[offset+i]
		} else {
			amount++
		}
	}
	return amount, items
}

// rsyncPlanTransfers asks rsync which of relPaths it would actually send, using
// the same flags as the real run so the answer matches. One round trip, no data
// moved.
//
// WHY: for a remote destination selectFilesToBackup cannot check the target, so
// the list it produces is an upper bound — on a converged mirror most of it is
// already there and rsync will skip it in milliseconds. Without this the ETA
// runs pessimistic and then jumps to done.
//
// Returns nil when the output is unparseable (old rsync, or a stubbed one), in
// which case the caller keeps the unfiltered list: a coarse denominator is
// better than a failed backup.
func rsyncPlanTransfers(srcRoot string, relPaths []string, remoteUserHost, remotePath string) map[string]bool {
	tmpFile, err := os.CreateTemp("", "photo-organizer-plan-*.txt")
	if err != nil {
		return nil
	}
	defer os.Remove(tmpFile.Name())
	for _, rel := range relPaths {
		fmt.Fprintln(tmpFile, rel)
	}
	if err := tmpFile.Close(); err != nil {
		return nil
	}

	// --out-format is rsync 2.6.4+ and newline-delimited, so unlike
	// --info=progress2 this needs no \r-aware scanning and no version probe.
	cmd := exec.Command("rsync", "-az", "--dry-run", "--out-format=%n",
		"--files-from="+tmpFile.Name(), srcRoot+"/", remoteUserHost+":"+remotePath+"/")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	known := make(map[string]bool, len(relPaths))
	for _, rel := range relPaths {
		known[rel] = true
	}
	plan := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		rel := strings.TrimSpace(line)
		// Ignore directory entries and anything not in the list we asked about.
		if known[rel] {
			plan[rel] = true
		}
	}
	if len(plan) == 0 {
		// Either nothing needs sending or the output said nothing we understood.
		// Both are indistinguishable here, so decline to narrow the denominator.
		return nil
	}
	return plan
}

// rsyncBatchEnd returns the exclusive end index of the batch beginning at start
// and the amount it represents — bytes, or the file count when sizes is nil.
// It always advances by at least one file, so a single oversized file cannot
// stall the loop.
func rsyncBatchEnd(sizes []int64, start, n int, maxBytes int64, maxFiles int) (end int, amount int64) {
	if sizes == nil {
		end = start + maxFiles
		if end > n {
			end = n
		}
		return end, int64(end - start)
	}

	for end = start; end < n && end < len(sizes); end++ {
		// Bytes decide first; the file cap only trips on a long run of tiny files.
		if end > start && (amount+sizes[end] > maxBytes || end-start >= maxFiles) {
			break
		}
		amount += sizes[end]
	}
	if end == start && start < n {
		end = start + 1 // never stall
	}
	return end, amount
}

// rsyncBatch runs one rsync invocation over the given relative paths. rsh, when
// non-empty, is passed as -e to share an SSH connection across batches. When
// extraArgs is non-nil those flags are added and stdout is returned to the
// caller instead of being forwarded to stderr.
func rsyncBatch(srcRoot string, relPaths []string, remoteUserHost, remotePath, rsh string,
	extraArgs []string) error {

	tmpFile, err := os.CreateTemp("", "photo-organizer-files-*.txt")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	for _, rel := range relPaths {
		fmt.Fprintln(tmpFile, rel)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("cannot write file list: %w", err)
	}

	args := []string{"-az"}
	if rsh != "" {
		args = append(args, "--rsh="+rsh)
	}
	args = append(args, extraArgs...)
	args = append(args, "--files-from="+tmpFile.Name(), srcRoot+"/", remoteUserHost+":"+remotePath+"/")

	cmd := exec.Command("rsync", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// progress2Unavailable latches once rsync rejects --info=progress2, so a run
// against an old rsync pays the failed-invocation cost only once.
var progress2Unavailable atomic.Bool

// rsyncOneLarge sends a single oversized file, reporting bytes as they move.
// It tries --info=progress2 and falls back to a plain send when the flag is
// unsupported (openrsync, rsync < 3.1) or emits nothing parseable.
//
// WHY parse rsync output only here: doing it costs a \r-aware split func and a
// version fallback. That price is worth paying for the one case where batch
// accounting genuinely fails — a file large enough to be its own batch — and
// not worth paying for the common case.
func rsyncOneLarge(srcRoot, rel string, size int64,
	remoteUserHost, remotePath, rsh string, prog *progressReporter) error {

	// Explain the slow-moving line before it starts moving slowly.
	prog.Note(fmt.Sprintf("sending %s (%s)", filepath.Base(rel), formatSize(size)))
	defer prog.Note("")

	if !progress2Unavailable.Load() {
		sent, err := rsyncStreamProgress(srcRoot, rel, remoteUserHost, remotePath, rsh, prog)
		if err == nil {
			// Nothing parseable came back: credit the file so the total still adds up.
			if sent < size {
				prog.Add(size-sent, 0)
			}
			return nil
		}
		// Treat a failed progress2 run as "this rsync does not support it" and
		// retry plainly. A genuine transfer error will surface again below.
		progress2Unavailable.Store(true)
	}

	if err := rsyncBatch(srcRoot, []string{rel}, remoteUserHost, remotePath, rsh, nil); err != nil {
		return err
	}
	prog.Add(size, 0)
	return nil
}

// rsyncStreamProgress runs rsync with --info=progress2 and feeds the byte
// counts it prints to prog. It returns how many bytes it credited.
func rsyncStreamProgress(srcRoot, rel, remoteUserHost, remotePath, rsh string,
	prog *progressReporter) (int64, error) {

	// Still --files-from, even for one file: it is what creates the remote
	// directory tree. A bare src/dst pair would fail on a missing parent.
	tmpFile, err := os.CreateTemp("", "photo-organizer-files-*.txt")
	if err != nil {
		return 0, fmt.Errorf("cannot create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	fmt.Fprintln(tmpFile, rel)
	if err := tmpFile.Close(); err != nil {
		return 0, fmt.Errorf("cannot write file list: %w", err)
	}

	args := []string{"-az", "--info=progress2"}
	if rsh != "" {
		args = append(args, "--rsh="+rsh)
	}
	args = append(args, "--files-from="+tmpFile.Name(), srcRoot+"/", remoteUserHost+":"+remotePath+"/")

	cmd := exec.Command("rsync", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	var credited int64
	scanner := bufio.NewScanner(stdout)
	// rsync separates progress updates with \r, not \n, so the default
	// ScanLines would buffer the whole transfer into one token.
	scanner.Split(scanCRLines)
	for scanner.Scan() {
		n, ok := parseProgress2Bytes(scanner.Text())
		if !ok || n <= credited {
			continue
		}
		prog.Add(n-credited, 0)
		credited = n
	}
	if err := cmd.Wait(); err != nil {
		return credited, err
	}
	return credited, nil
}

// scanCRLines splits on either \r or \n, so rsync's in-place progress updates
// arrive as separate tokens.
func scanCRLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseProgress2Bytes pulls the byte count off an --info=progress2 line, which
// looks like "  1,234,567,890  38%   84.21MB/s    0:00:24".
func parseProgress2Bytes(line string) (int64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasSuffix(fields[1], "%") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(fields[0], ",", ""), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// refreshRemoteManifest rescans remotePath on the remote machine and pulls the
// updated manifest back, so files copied there count as a backup locally.
func refreshRemoteManifest(remoteUserHost, remoteMachineID, remotePath string) error {
	fmt.Fprintf(os.Stderr, "Updating manifest on %s (rescans the whole destination)...\n", remoteUserHost)
	start := time.Now()

	// The remote scan prints its own progress; piping it through is the only
	// thing standing between the user and several silent minutes.
	scanCmd := fmt.Sprintf("cd %s && for path in photo-organizer ~/bin/photo-organizer /usr/local/bin/photo-organizer; do if command -v $path &>/dev/null || [ -f $path ]; then $path scan . --machine %s; exit $?; fi; done; exit 1", shellQuote(remotePath), shellQuote(remoteMachineID))
	scan := exec.Command("ssh", remoteUserHost, scanCmd)
	scan.Stdout = os.Stderr
	scan.Stderr = os.Stderr
	if err := scan.Run(); err != nil {
		return fmt.Errorf("remote scan failed after copying files. photo-organizer must be installed on remote machine")
	}
	fmt.Fprintf(os.Stderr, "✓ Remote manifest updated in %s\n", formatDuration(time.Since(start)))

	collectCmd := exec.Command("photo-organizer", "collect", "--from", remoteMachineID)
	collectCmd.Stdout = os.Stderr
	collectCmd.Stderr = os.Stderr
	if err := collectCmd.Run(); err != nil {
		return fmt.Errorf("files were copied, but manifest collection failed: %v", err)
	}
	return nil
}
