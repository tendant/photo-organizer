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
	holdings []string // --holding (repeatable): one per filesystem the sources live on
	report   string
	fromRep  string
	apply    bool
	kind     mediaKind
	typeSet  bool
	byBytes  bool // --verify bytes: compare file contents instead of trusting manifest hashes
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
			opts.holdings = append(opts.holdings, args[i+1])
			i++
		case "--report":
			if err := need(i, "a file name"); err != nil {
				return opts, err
			}
			opts.report = args[i+1]
			i++
		case "--verify":
			if err := need(i, "'manifest' or 'bytes'"); err != nil {
				return opts, err
			}
			switch strings.ToLower(args[i+1]) {
			case "manifest", "hash":
				opts.byBytes = false
			case "bytes", "full":
				opts.byBytes = true
			default:
				return opts, fmt.Errorf("unknown verify mode %q (use manifest or bytes)", args[i+1])
			}
			i++
		case "--from-report":
			if err := need(i, "a report file"); err != nil {
				return opts, err
			}
			opts.fromRep = args[i+1]
			i++
		case "--type":
			opts.typeSet = true
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
	if opts.fromRep != "" && opts.typeSet {
		return opts, errors.New("--type has no effect with --from-report: the report already decides which files qualify")
	}
	if opts.apply && len(opts.holdings) == 0 {
		return opts, errors.New("--apply needs --holding <folder>: files are moved there, never deleted")
	}
	return opts, nil
}

func printPruneUsage() {
	fmt.Fprintf(os.Stderr, "Usage: photo-organizer prune-sources <source>... --library DIR --backup DIR [--backup DIR]\n")
	fmt.Fprintf(os.Stderr, "                                     [--type photo|video|all] [--report FILE] [--jobs N]\n")
	fmt.Fprintf(os.Stderr, "                                     [--holding DIR [--holding DIR]... --apply] [--from-report FILE]\n")
	fmt.Fprintf(os.Stderr, "                                     [--verify manifest|bytes]\n\n")
	fmt.Fprintf(os.Stderr, "Finds source files that have a byte-identical copy in the library AND in a backup.\n")
	fmt.Fprintf(os.Stderr, "Preview by default. With --apply the files are MOVED into --holding (same relative\n")
	fmt.Fprintf(os.Stderr, "paths); nothing is deleted. Delete the holding folder yourself once you are sure.\n")
	fmt.Fprintf(os.Stderr, "  - --verify manifest (default) matches files by the hashes in fresh manifests (scan the source,\n")
	fmt.Fprintf(os.Stderr, "    library and backup first). It reads no photo data. A file counts only if its size and\n")
	fmt.Fprintf(os.Stderr, "    modified time still equal the manifest row. The hash covers the size plus the first and\n")
	fmt.Fprintf(os.Stderr, "    last 32 KB (and the full hash where both rows have one).\n")
	fmt.Fprintf(os.Stderr, "  - --verify bytes compares every byte of the source, library and backup file instead (slow).\n")
	fmt.Fprintf(os.Stderr, "  - Library and backup must be on different devices.\n")
	fmt.Fprintf(os.Stderr, "  - Sidecars, files of the other --type and unverified files are always kept.\n")
	fmt.Fprintf(os.Stderr, "  - --from-report FILE reuses the byte comparison of an earlier preview (its --report file):\n")
	fmt.Fprintf(os.Stderr, "    it only re-checks that the source, library and backup files still exist with the same\n")
	fmt.Fprintf(os.Stderr, "    size and were not modified after the report was written. Fast; nothing is re-read.\n")
	fmt.Fprintf(os.Stderr, "  - A move cannot cross filesystems, so give one --holding per filesystem your sources live on\n")
	fmt.Fprintf(os.Stderr, "    (repeat the flag); each file goes to the holding folder on its own filesystem. The folder\n")
	fmt.Fprintf(os.Stderr, "    may not exist yet. Before moving anything, apply checks that every file has such a folder\n")
	fmt.Fprintf(os.Stderr, "    and that you may write to its source folder, and refuses with a list if not.\n")
}

type pruneVerdict int

const (
	pruneSafe pruneVerdict = iota
	pruneNoLibrary
	pruneNoBackup
	pruneSidecar
	pruneOtherKind
	pruneUnreadable
	pruneChanged
	pruneNotFresh
	pruneNotInManifest
	pruneApplePhotos
)

func (v pruneVerdict) String() string {
	return [...]string{"safe", "not in library", "not in backup", "sidecar kept", "other type", "unreadable", "changed since report", "manifest out of date", "not in a manifest", "inside Apple Photos library"}[v]
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
	for _, holding := range opts.holdings {
		h := resolveThroughSymlinks(holding)
		for _, d := range dirs {
			if samePathOrInside(h, resolveThroughSymlinks(d)) {
				return fmt.Errorf("--holding %s overlaps %s: use a separate folder", holding, d)
			}
		}
		// A holding folder may sit inside a source (a drive that is both the source and
		// the only place a move can go); it is skipped when the source is listed. It may
		// not be, or contain, a whole source.
		for _, src := range opts.sources {
			if pathWithin(resolveThroughSymlinks(src), h) {
				return fmt.Errorf("--holding %s contains the source %s: use a separate folder", holding, src)
			}
		}
	}
	return nil
}

// inHolding reports whether path lies inside one of the holding folders, whose files were
// already moved out of the sources and must not be offered again.
func inHolding(path string, holdings []string) bool {
	for _, h := range holdings {
		if pathWithin(path, filepath.Clean(h)) {
			return true
		}
	}
	return false
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

	var items []*pruneItem
	if opts.fromRep != "" {
		var err error
		if items, err = loadPruneReport(opts); err != nil {
			return err
		}
		return finishPrune(items, opts)
	}

	if !opts.byBytes {
		items = collectPruneItems(opts, showProgress)
		if err := verifyByManifest(items, opts); err != nil {
			return err
		}
		return finishPrune(items, opts)
	}

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

	items = collectPruneItems(opts, showProgress)
	verifyPruneItems(items, libIdx, bakIdx, opts, showProgress)
	return finishPrune(items, opts)
}

// finishPrune prints the summary, writes the report and (with --apply) moves files.
func finishPrune(items []*pruneItem, opts pruneOptions) error {
	printPruneSummary(items, opts)

	if opts.report != "" && opts.fromRep == "" {
		if err := writePruneReport(opts.report, items); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Report written to %s\n", opts.report)
	}
	blocked := checkPruneMoves(items, opts.holdings)
	if len(blocked) > 0 {
		printPruneBlocked(blocked, len(opts.holdings) > 0)
	}
	if !opts.apply {
		fmt.Fprintf(os.Stderr, "\nPreview only. Nothing was moved. Re-run with --holding <folder> --apply to move the safe files.\n")
		return nil
	}
	if len(blocked) > 0 {
		fmt.Fprintf(os.Stderr, "\nError: some safe files cannot be moved; nothing was changed. Fix the above and run again.\n")
		return errFailed
	}
	moved, failed := movePruneItems(items, opts.holdings)
	fmt.Fprintf(os.Stderr, "\nMoved %s files into %s", formatCount(moved), strings.Join(opts.holdings, ", "))
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "; %s could not be moved and stay in place", formatCount(failed))
	}
	fmt.Fprintf(os.Stderr, ".\nNothing was deleted. Delete the holding folder(s) yourself when you are sure.\n")
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
				if inHolding(f.path, opts.holdings) {
					continue
				}
				it := &pruneItem{path: f.path, root: root, size: f.size}
				switch {
				case inApplePhotos(f.path):
					it.verdict = pruneApplePhotos // the app owns these files; never moved
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
	checked := len(items) - count[pruneOtherKind]
	if count[pruneOtherKind] > 0 {
		fmt.Fprintf(os.Stderr, "\nSource files checked: %s (--type %s). Not checked, of the other type: %s files, %s\n",
			formatCount(checked), opts.kind, formatCount(count[pruneOtherKind]), formatSize(size[pruneOtherKind]))
	} else {
		fmt.Fprintf(os.Stderr, "\nSource files checked: %s\n", formatCount(checked))
	}
	fmt.Fprintf(os.Stderr, "  Safe to remove (identical copy in library and backup): %s files, %s\n",
		formatCount(count[pruneSafe]), formatSize(size[pruneSafe]))
	for _, v := range []pruneVerdict{pruneNoLibrary, pruneNoBackup, pruneSidecar, pruneUnreadable, pruneChanged, pruneNotFresh, pruneNotInManifest, pruneApplePhotos} {
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

// holdingFor picks the holding folder on the same filesystem as path (a rename cannot cross
// filesystems). The folder may not exist yet; its nearest existing parent decides.
func holdingFor(holdings []string, path string) (string, bool) {
	for _, h := range holdings {
		if sameFilesystem(path, h) {
			return h, true
		}
	}
	return "", false
}

// pruneBlock is a reason some safe files cannot be moved, with how many and where.
type pruneBlock struct {
	kind  string // "nohold": no --holding on the file's filesystem; "perm": source folder not writable
	where string
	files int
	bytes int64
}

// checkPruneMoves finds safe files that a move would fail on, before anything is moved: no
// holding folder on their filesystem, or a source folder we may not remove files from.
// Without any --holding only the permission problem can be reported.
func checkPruneMoves(items []*pruneItem, holdings []string) []pruneBlock {
	byKey := map[string]*pruneBlock{}
	var order []string
	add := func(kind, where string, size int64) {
		k := kind + "\x00" + where
		b := byKey[k]
		if b == nil {
			b = &pruneBlock{kind: kind, where: where}
			byKey[k] = b
			order = append(order, k)
		}
		b.files++
		b.bytes += size
	}
	writable := map[string]bool{}
	for _, it := range items {
		if it.verdict != pruneSafe {
			continue
		}
		dir := filepath.Dir(it.path)
		ok, seen := writable[dir]
		if !seen {
			ok = canModifyDir(dir)
			writable[dir] = ok
		}
		if !ok {
			add("perm", dir, it.size)
		}
		if len(holdings) > 0 {
			if _, found := holdingFor(holdings, it.path); !found {
				add("nohold", it.root, it.size)
			}
		}
	}
	sort.Strings(order)
	out := make([]pruneBlock, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

func printPruneBlocked(blocked []pruneBlock, haveHolding bool) {
	var perm, nohold []pruneBlock
	for _, b := range blocked {
		if b.kind == "perm" {
			perm = append(perm, b)
		} else {
			nohold = append(nohold, b)
		}
	}
	list := func(bs []pruneBlock) {
		for i, b := range bs {
			if i == 8 {
				fmt.Fprintf(os.Stderr, "     ... and %d more\n", len(bs)-8)
				return
			}
			fmt.Fprintf(os.Stderr, "     %s (%s files, %s)\n", b.where, formatCount(b.files), formatSize(b.bytes))
		}
	}
	if len(nohold) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠  No --holding on the same filesystem as these sources (a move cannot cross filesystems);\n"+
			"   add a --holding folder on each, e.g. <mount>/_holding/pruned:\n")
		list(nohold)
	}
	if len(perm) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠  You have no write permission on these source folders, so files cannot be moved out of them\n"+
			"   (fix with e.g. sudo chown -R $USER <folder>):\n")
		list(perm)
	}
}

func movePruneItems(items []*pruneItem, holdings []string) (moved, failed int) {
	for _, it := range items {
		if it.verdict != pruneSafe || inApplePhotos(it.path) {
			continue
		}
		holding, ok := holdingFor(holdings, it.path)
		if !ok && len(holdings) > 0 {
			holding = holdings[0] // single --holding: keep the old behaviour and let the move report any error
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

// loadPruneReport turns an earlier preview's report into items, re-checking cheaply
// that nothing changed since: each file must still exist with the recorded size and
// must not have been modified after the report was written.
func loadPruneReport(opts pruneOptions) ([]*pruneItem, error) {
	rst, err := os.Stat(opts.fromRep)
	if err != nil {
		return nil, err
	}
	written := rst.ModTime()
	f, err := os.Open(opts.fromRep)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", opts.fromRep, err)
	}
	if len(rows) < 1 || len(rows[0]) < 5 || rows[0][0] != "source_path" || rows[0][4] != "backup_copy" {
		return nil, fmt.Errorf("%s is not a prune-sources report", opts.fromRep)
	}
	var roots []string
	for _, s := range opts.sources {
		roots = append(roots, filepath.Clean(s))
	}
	rootOf := func(path string) string {
		for _, r := range roots {
			if pathWithin(path, r) {
				return r
			}
		}
		return ""
	}
	unchanged := func(path string, size int64) bool {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && st.Size() == size && !st.ModTime().After(written)
	}
	within := func(path string, dirs ...string) bool {
		for _, d := range dirs {
			if pathWithin(path, filepath.Clean(d)) {
				return true
			}
		}
		return false
	}
	var items []*pruneItem
	for _, r := range rows[1:] {
		if len(r) < 5 {
			continue
		}
		size, err := strconv.ParseInt(r[1], 10, 64)
		if err != nil {
			continue
		}
		root := rootOf(r[0])
		if root == "" {
			continue // not under a source you named this time
		}
		it := &pruneItem{path: r[0], root: root, size: size, libCopy: r[3], bakCopy: r[4]}
		if inApplePhotos(it.path) {
			it.verdict = pruneApplePhotos // an older report may call it safe; it never moves
			items = append(items, it)
			continue
		}
		switch r[2] {
		case pruneSafe.String():
			ok := within(it.libCopy, opts.library) && within(it.bakCopy, opts.backups...) &&
				unchanged(it.path, size) && unchanged(it.libCopy, size) && unchanged(it.bakCopy, size)
			if ok {
				it.verdict = pruneSafe
			} else {
				it.verdict = pruneChanged
			}
		case pruneNoLibrary.String():
			it.verdict = pruneNoLibrary
		case pruneNoBackup.String():
			it.verdict = pruneNoBackup
		case pruneSidecar.String():
			it.verdict = pruneSidecar
		case pruneOtherKind.String():
			it.verdict = pruneOtherKind
		case pruneNotFresh.String():
			it.verdict = pruneNotFresh
		case pruneNotInManifest.String():
			it.verdict = pruneNotInManifest
		case pruneApplePhotos.String():
			it.verdict = pruneApplePhotos
		case pruneChanged.String():
			it.verdict = pruneChanged
		default:
			it.verdict = pruneUnreadable
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no files in %s belong to the source folders you gave", opts.fromRep)
	}
	fmt.Fprintf(os.Stderr, "Using %s (written %s): re-checked %s files without re-reading them.\n",
		opts.fromRep, written.Format("2006-01-02 15:04"), formatCount(len(items)))
	return items, nil
}
