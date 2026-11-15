package main

// prune-sources: find source photos that are safely duplicated in the library
// AND in an independent backup, and (only with --apply) move them into a
// holding folder. Nothing is ever deleted by this command.
//
// Safety rules:
//   - a file qualifies only if a byte-identical copy exists in the library and
//     in a backup, found by walking those folders now (no stale manifests);
//   - library and backup must be on different devices, and neither may sit
//     inside a source (or the other way round);
//   - sidecars, other-kind files and anything unverified are always kept.

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type pruneOptions struct {
	sources  []string
	library  string
	backups  []string
	holding  string
	report   string
	apply    bool
	kind     mediaKind
	jobs     int
	progress progressMode
}

func parsePruneArgs(args []string) (pruneOptions, error) {
	opts := pruneOptions{jobs: 4, progress: progressAuto, kind: kindAll}
	need := func(i int, what string) error {
		if i+1 >= len(args) {
			return fmt.Errorf("%s needs %s", args[i], what)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--library":
			if err := need(i, "a folder"); err != nil {
				return opts, err
			}
			opts.library = args[i+1]
			i++
		case "--backup":
			if err := need(i, "a folder"); err != nil {
				return opts, err
			}
			opts.backups = append(opts.backups, args[i+1])
			i++
		case "--holding":
			if err := need(i, "a folder"); err != nil {
				return opts, err
			}
			opts.holding = args[i+1]
			i++
		case "--report":
			if err := need(i, "a file name"); err != nil {
				return opts, err
			}
			opts.report = args[i+1]
			i++
		case "--type":
			if err := need(i, "'photo', 'video' or 'all'"); err != nil {
				return opts, err
			}
			switch strings.ToLower(args[i+1]) {
			case "photo", "photos":
				opts.kind = kindPhoto
			case "video", "videos":
				opts.kind = kindVideo
			case "all":
				opts.kind = kindAll
			default:
				return opts, fmt.Errorf("unknown type %q (use photo, video or all)", args[i+1])
			}
			i++
		case "--jobs":
			if err := need(i, "a number"); err != nil {
				return opts, err
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 64 {
				return opts, fmt.Errorf("--jobs must be between 1 and 64, got %q", args[i+1])
			}
			opts.jobs = n
			i++
		case "--apply":
			opts.apply = true
		case "--no-progress":
			opts.progress = progressOff
		default:
			if strings.HasPrefix(args[i], "--") {
				return opts, fmt.Errorf("unknown option %q", args[i])
			}
			opts.sources = append(opts.sources, args[i])
		}
	}
	if len(opts.sources) == 0 {
		return opts, errors.New("give at least one source folder")
	}
	if opts.library == "" {
		return opts, errors.New("--library is required")
	}
	if len(opts.backups) == 0 {
		return opts, errors.New("--backup is required: a source is only safe to remove when a copy also exists outside the library")
	}
	if opts.apply && opts.holding == "" {
		return opts, errors.New("--apply needs --holding <folder>: files are moved there, never deleted")
	}
	return opts, nil
}

func printPruneUsage() {
	fmt.Fprintf(os.Stderr, "Usage: photo-organizer prune-sources <source>... --library DIR --backup DIR [--backup DIR]\n")
	fmt.Fprintf(os.Stderr, "                                     [--type photo|video|all] [--report FILE] [--jobs N]\n")
	fmt.Fprintf(os.Stderr, "                                     [--holding DIR --apply]\n\n")
	fmt.Fprintf(os.Stderr, "Finds source files that have a byte-identical copy in the library AND in a backup.\n")
	fmt.Fprintf(os.Stderr, "Preview by default. With --apply the files are MOVED into --holding (same relative\n")
	fmt.Fprintf(os.Stderr, "paths); nothing is deleted. Delete the holding folder yourself once you are sure.\n")
	fmt.Fprintf(os.Stderr, "  - Library and backup are read from disk now, not from manifests.\n")
	fmt.Fprintf(os.Stderr, "  - Library and backup must be on different devices.\n")
	fmt.Fprintf(os.Stderr, "  - Sidecars, files of the other --type and unverified files are always kept.\n")
	fmt.Fprintf(os.Stderr, "  - Keep --holding on the same filesystem as the source so the move is instant.\n")
}

type pruneVerdict int

const (
	pruneSafe pruneVerdict = iota
	pruneNoLibrary
	pruneNoBackup
	pruneSidecar
	pruneOtherKind
	pruneUnreadable
)

func (v pruneVerdict) String() string {
	return [...]string{"safe", "not in library", "not in backup", "sidecar kept", "other type", "unreadable"}[v]
}

type pruneItem struct {
	path    string
	root    string
	size    int64
	verdict pruneVerdict
	libCopy string
	bakCopy string
	moved   bool
	moveErr error
}

// sizeIndex maps file size -> media files of that size under a folder.
type sizeIndex map[int64][]string

func buildSizeIndex(root string) (sizeIndex, int) {
	idx := sizeIndex{}
	n := 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && isJunkDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if !isMediaFile(ext) || isSidecarExt(ext) || shouldSkipFile(path) || noiseFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		idx[info.Size()] = append(idx[info.Size()], path)
		n++
		return nil
	})
	return idx, n
}

// findIdentical returns a file in idx with exactly the same bytes as path.
func (idx sizeIndex) findIdentical(path string, size int64) string {
	for _, c := range idx[size] {
		if sameContent(c, path) {
			return c
		}
	}
	return ""
}

// deviceOf is a variable so tests (whose temp folders share one device) can fake it.
var deviceOf = func(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

func samePathOrInside(a, b string) bool {
	return pathWithin(a, b) || pathWithin(b, a)
}

// checkPruneLayout refuses setups where "copy elsewhere" would not be true.
func checkPruneLayout(opts pruneOptions) error {
	dirs := append([]string{opts.library}, opts.backups...)
	for _, d := range dirs {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a readable folder", d)
		}
	}
	for _, s := range opts.sources {
		if st, err := os.Stat(s); err != nil || !st.IsDir() {
			return fmt.Errorf("source %s is not a readable folder", s)
		}
		for _, d := range dirs {
			if samePathOrInside(resolveThroughSymlinks(s), resolveThroughSymlinks(d)) {
				return fmt.Errorf("source %s and %s overlap: a source cannot be its own copy", s, d)
			}
		}
	}
	libDev, ok := deviceOf(opts.library)
	if !ok {
		return fmt.Errorf("cannot stat %s", opts.library)
	}
	for _, b := range opts.backups {
		if samePathOrInside(resolveThroughSymlinks(opts.library), resolveThroughSymlinks(b)) {
			return fmt.Errorf("backup %s overlaps the library", b)
		}
		bd, ok := deviceOf(b)
		if !ok {
			return fmt.Errorf("cannot stat %s", b)
		}
		if bd == libDev {
			return fmt.Errorf("backup %s is on the same device as the library: that is not an independent copy", b)
		}
	}
	if opts.holding != "" {
		h := resolveThroughSymlinks(opts.holding)
		for _, d := range append(dirs, opts.sources...) {
			if samePathOrInside(h, resolveThroughSymlinks(d)) {
				return fmt.Errorf("--holding %s overlaps %s: use a separate folder", opts.holding, d)
			}
		}
	}
	return nil
}

func runPruneSources(args []string) error {
	opts, err := parsePruneArgs(args)
	if err != nil {
		printPruneUsage()
		return err
	}
	if err := checkPruneLayout(opts); err != nil {
		return err
	}
	showProgress := progressEnabled(opts.progress)

	fmt.Fprintf(os.Stderr, "Reading library and backup folders...\n")
	libIdx, libN := buildSizeIndex(opts.library)
	bakIdx := make([]sizeIndex, len(opts.backups))
	bakN := 0
	for i, b := range opts.backups {
		var n int
		bakIdx[i], n = buildSizeIndex(b)
		bakN += n
	}
	fmt.Fprintf(os.Stderr, "  library: %s files, backup: %s files\n", formatCount(libN), formatCount(bakN))
	if libN == 0 || bakN == 0 {
		return errors.New("library or backup holds no photos/videos; refusing to continue")
	}

	items := collectPruneItems(opts, showProgress)
	verifyPruneItems(items, libIdx, bakIdx, opts, showProgress)
	printPruneSummary(items, opts)

	if opts.report != "" {
		if err := writePruneReport(opts.report, items); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Report written to %s\n", opts.report)
	}
	if !opts.apply {
		fmt.Fprintf(os.Stderr, "\nPreview only. Nothing was moved. Re-run with --holding <folder> --apply to move the safe files.\n")
		return nil
	}
	moved, failed := movePruneItems(items, opts.holding)
	fmt.Fprintf(os.Stderr, "\nMoved %s files into %s", formatCount(moved), opts.holding)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "; %s could not be moved and stay in place", formatCount(failed))
	}
	fmt.Fprintf(os.Stderr, ".\nNothing was deleted. Delete the holding folder yourself when you are sure.\n")
	if failed > 0 {
		return errFailed
	}
	return nil
}

// collectPruneItems lists the source files and classifies the ones that never qualify.
func collectPruneItems(opts pruneOptions, showProgress bool) []*pruneItem {
	var items []*pruneItem
	for _, root := range opts.sources {
		scan := scanSources([]string{root}, showProgress)
		for _, files := range scan.groups {
			other := opts.kind != kindAll && groupKind(files) != opts.kind
			for _, f := range files {
				it := &pruneItem{path: f.path, root: root, size: f.size}
				switch {
				case isSidecarExt(filepath.Ext(f.path)):
					it.verdict = pruneSidecar
				case other:
					it.verdict = pruneOtherKind
				default:
					it.verdict = pruneSafe // provisional, verified below
				}
				items = append(items, it)
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].path < items[j].path })
	return items
}

func verifyPruneItems(items []*pruneItem, lib sizeIndex, baks []sizeIndex, opts pruneOptions, showProgress bool) {
	var todo []*pruneItem
	var total int64
	for _, it := range items {
		if it.verdict == pruneSafe {
			todo = append(todo, it)
			total += it.size
		}
	}
	prog := newProgressReporter("Comparing bytes", unitBytes, total, int64(len(todo)), showProgress)
	work := make(chan *pruneItem)
	var wg sync.WaitGroup
	for w := 0; w < opts.jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				if _, err := os.Stat(it.path); err != nil {
					it.verdict = pruneUnreadable
				} else if it.libCopy = lib.findIdentical(it.path, it.size); it.libCopy == "" {
					it.verdict = pruneNoLibrary
				} else {
					for _, b := range baks {
						if it.bakCopy = b.findIdentical(it.path, it.size); it.bakCopy != "" {
							break
						}
					}
					if it.bakCopy == "" {
						it.verdict = pruneNoBackup
					}
				}
				prog.Add(it.size, 1)
			}
		}()
	}
	for _, it := range todo {
		work <- it
	}
	close(work)
	wg.Wait()
	prog.Done(fmt.Sprintf("Compared %s files", formatCount(len(todo))))
}

func printPruneSummary(items []*pruneItem, opts pruneOptions) {
	count := map[pruneVerdict]int{}
	size := map[pruneVerdict]int64{}
	for _, it := range items {
		count[it.verdict]++
		size[it.verdict] += it.size
	}
	fmt.Fprintf(os.Stderr, "\nSource files looked at: %s\n", formatCount(len(items)))
	fmt.Fprintf(os.Stderr, "  Safe to remove (identical copy in library and backup): %s files, %s\n",
		formatCount(count[pruneSafe]), formatSize(size[pruneSafe]))
	for _, v := range []pruneVerdict{pruneNoLibrary, pruneNoBackup, pruneSidecar, pruneOtherKind, pruneUnreadable} {
		if count[v] > 0 {
			fmt.Fprintf(os.Stderr, "  Kept — %-15s %s files, %s\n", v.String()+":", formatCount(count[v]), formatSize(size[v]))
		}
	}
	if count[pruneNoLibrary] > 0 {
		fmt.Fprintf(os.Stderr, "  (files not in the library: run organize for them first)\n")
	}
	if count[pruneNoBackup] > 0 {
		fmt.Fprintf(os.Stderr, "  (files in the library but not yet in a backup: run backup first)\n")
	}
	if count[pruneSidecar] > 0 {
		fmt.Fprintf(os.Stderr, "  (sidecars are never moved automatically; check them by hand)\n")
	}
	// Per top folder so you can retire one folder at a time.
	perDir := map[string]int{}
	for _, it := range items {
		if it.verdict == pruneSafe {
			rel, _ := filepath.Rel(it.root, it.path)
			top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
			perDir[filepath.Join(it.root, top)]++
		}
	}
	if len(perDir) > 0 {
		fmt.Fprintf(os.Stderr, "  Safe files by top folder: %s\n", topCounts(perDir, 8))
	}
}

func writePruneReport(path string, items []*pruneItem) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"source_path", "size_bytes", "verdict", "library_copy", "backup_copy"})
	for _, it := range items {
		w.Write([]string{it.path, strconv.FormatInt(it.size, 10), it.verdict.String(), it.libCopy, it.bakCopy})
	}
	w.Flush()
	return w.Error()
}

// holdingPath keeps each source root apart so same-named roots cannot collide.
func holdingPath(holding, root, path string) string {
	tag := strings.Trim(strings.ReplaceAll(filepath.ToSlash(resolveThroughSymlinks(root)), "/", "_"), "_")
	rel, _ := filepath.Rel(root, path)
	return filepath.Join(holding, tag, rel)
}

func movePruneItems(items []*pruneItem, holding string) (moved, failed int) {
	for _, it := range items {
		if it.verdict != pruneSafe {
			continue
		}
		dst := holdingPath(holding, it.root, it.path)
		if _, err := os.Lstat(dst); err == nil {
			it.moveErr = fmt.Errorf("%s already exists in the holding folder", dst)
		} else if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			it.moveErr = err
		} else {
			it.moveErr = os.Rename(it.path, dst)
		}
		if it.moveErr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  could not move %s: %v\n", it.path, it.moveErr)
			continue
		}
		it.moved = true
		moved++
	}
	return
}
