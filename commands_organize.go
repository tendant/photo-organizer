package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ============================================================================
// ORGANIZE COMMAND - Build a date-sorted library from scattered folders
// ============================================================================
//
// organize COPIES photos into a new library laid out as YYYY/YYYY-MM-DD/. The
// source folders are only ever opened for reading: nothing in them is moved,
// renamed, touched or deleted. Run it as often as you like — files whose
// content is already in the library are skipped, so repeated runs converge and
// an interrupted run simply continues where it stopped.
//
// It is a dry run unless --apply is given.

const (
	organizeLogDir    = "_organize"
	organizeTmpPrefix = ".organize-tmp-"
	undatedFolder     = "undated"
)

type organizeOptions struct {
	sources    []string
	dest       string
	apply      bool
	byMonth    bool
	progress   progressMode
	jobs       int
	verify     verifyMode
	videoTZ    *time.Location // nil: video times are used as written (UTC)
	videoTZSet bool           // --video-tz was given (rejected with --plan)
	savePlan   string         // --save-plan: write the previewed plan to this file
	planFile   string         // --plan: apply a plan saved earlier instead of scanning
}

// plannedCopy is one file that will be copied into the library.
type plannedCopy struct {
	src     string
	dst     string
	size    int64
	hash    string
	mtime   int64 // source modified time (UnixNano); the copy keeps it
	date    time.Time
	hasDate bool
}

// organizePlan is everything a dry run reports and an apply run executes.
type organizePlan struct {
	copies           []plannedCopy
	copyBytes        int64
	duplicates       int // content already in the library or earlier in this run
	duplicateBytes   int64
	orphanSidecars   int // .xmp/.aae/... with no photo or video to attach to
	emptySkipped     int
	conflictSidecars []string       // sidecar differs from the one already beside its photo
	skippedTypes     map[string]int // non-media extensions left behind
	junkDirs         map[string]int // system/thumbnail folders skipped
	unreadable       []string
	undated          int
	byFolder         map[string]int // top-level year folder -> files
	freeBytes        uint64
	freeKnown        bool
	hs               *hashStore
	sampledDups      int                // large duplicates confirmed by sampling rather than every byte
	staleTmp         []string           // temp files left by an interrupted earlier run
	dateSources      map[dateSource]int // how each copied photo/video was dated
	mtimeExamples    []string           // a few files dated only by modified time
	jan1             map[string]int     // EXIF dates that fall on January 1st
}

func parseOrganizeArgs(args []string) (organizeOptions, error) {
	opts := organizeOptions{progress: progressAuto, jobs: 4}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dest":
			if i+1 >= len(args) {
				return opts, errors.New("--dest needs a folder")
			}
			opts.dest = args[i+1]
			i++
		case "--apply":
			opts.apply = true
		case "--layout":
			if i+1 >= len(args) {
				return opts, errors.New("--layout needs 'day' or 'month'")
			}
			switch args[i+1] {
			case "day":
				opts.byMonth = false
			case "month":
				opts.byMonth = true
			default:
				return opts, fmt.Errorf("unknown layout %q (use 'day' or 'month')", args[i+1])
			}
			i++
		case "--jobs":
			if i+1 >= len(args) {
				return opts, errors.New("--jobs needs a number")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 64 {
				return opts, fmt.Errorf("--jobs must be between 1 and 64, got %q", args[i+1])
			}
			opts.jobs = n
			i++
		case "--verify":
			if i+1 >= len(args) {
				return opts, errors.New("--verify needs 'auto', 'full' or 'quick'")
			}
			switch args[i+1] {
			case "auto":
				opts.verify = verifyAuto
			case "full":
				opts.verify = verifyFull
			case "quick":
				opts.verify = verifyQuick
			default:
				return opts, fmt.Errorf("unknown verify mode %q (use auto, full or quick)", args[i+1])
			}
			i++
		case "--video-tz":
			opts.videoTZSet = true
			if i+1 >= len(args) {
				return opts, errors.New("--video-tz needs 'utc', 'local' or a zone name like Europe/Paris")
			}
			switch strings.ToLower(args[i+1]) {
			case "utc":
				opts.videoTZ = nil
			case "local":
				opts.videoTZ = time.Local
			default:
				loc, err := time.LoadLocation(args[i+1])
				if err != nil {
					return opts, fmt.Errorf("unknown time zone %q (use utc, local or a name like America/New_York)", args[i+1])
				}
				opts.videoTZ = loc
			}
			i++
		case "--save-plan", "--plan":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs a file name", args[i])
			}
			if args[i] == "--plan" {
				opts.planFile = args[i+1]
			} else {
				opts.savePlan = args[i+1]
			}
			i++
		case "--progress":
			opts.progress = progressOn
		case "--no-progress":
			opts.progress = progressOff
		default:
			if strings.HasPrefix(args[i], "--") {
				return opts, fmt.Errorf("unknown flag %s", args[i])
			}
			opts.sources = append(opts.sources, args[i])
		}
	}
	return opts, nil
}

func printOrganizeUsage() {
	fmt.Fprintf(os.Stderr, "\n🗂  ORGANIZE PHOTOS BY DATE\n\n")
	fmt.Fprintf(os.Stderr, "Usage: photo-organizer organize <folder> [<folder>...] --dest <library> [--apply] [--layout day|month]\n\n")
	fmt.Fprintf(os.Stderr, "Examples:\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer organize /data/Photos-All --dest /tankm1/media/Library\n")
	fmt.Fprintf(os.Stderr, "  photo-organizer organize ~/Pictures /mnt/usb/DCIM --dest ~/Library --apply\n\n")
	fmt.Fprintf(os.Stderr, "Copies photos and videos into <library>/YYYY/YYYY-MM-DD/ using the EXIF\n")
	fmt.Fprintf(os.Stderr, "capture date (falling back to the date in the filename, then the file's\n")
	fmt.Fprintf(os.Stderr, "modified time). Your source folders are never changed.\n\n")
	fmt.Fprintf(os.Stderr, "  - Dry run by default: shows what would happen and writes nothing.\n")
	fmt.Fprintf(os.Stderr, "  - Files already in the library (same content) are skipped, so you can\n")
	fmt.Fprintf(os.Stderr, "    run it again after adding photos, or after an interruption.\n")
	fmt.Fprintf(os.Stderr, "  - RAW+JPG pairs and .xmp/.aae sidecars stay together in the same folder.\n")
	fmt.Fprintf(os.Stderr, "  - Same name, different content: the newcomer gets a _1, _2... suffix.\n")
	fmt.Fprintf(os.Stderr, "  - A log of every copy is written to <library>/%s/.\n", organizeLogDir)
	fmt.Fprintf(os.Stderr, "  - --layout month uses YYYY/YYYY-MM/ instead of one folder per day.\n")
	fmt.Fprintf(os.Stderr, "  - --jobs N hashes N files at once (default 4; 1 for a single spinning disk).\n")
	fmt.Fprintf(os.Stderr, "  - --verify auto|full|quick: how duplicates are confirmed. auto compares every byte\n")
	fmt.Fprintf(os.Stderr, "    up to 64 MB and samples 16 spread points in larger files; full always reads\n")
	fmt.Fprintf(os.Stderr, "    every byte; quick samples anything over 1 MB.\n")
	fmt.Fprintf(os.Stderr, "  - --video-tz utc|local|<zone>: phones record video times in UTC, so an evening clip can\n")
	fmt.Fprintf(os.Stderr, "    land on the next day. 'local' uses this computer's zone; or name one (Europe/Paris).\n")
	fmt.Fprintf(os.Stderr, "    Default utc = as written. Photos (EXIF) are never shifted.\n")
	fmt.Fprintf(os.Stderr, "  - --save-plan FILE (dry run) saves exactly what was previewed; later run\n")
	fmt.Fprintf(os.Stderr, "    'organize --plan FILE --apply' to copy precisely that. It refuses if any\n")
	fmt.Fprintf(os.Stderr, "    source file changed or any target name is now taken.\n")
}

func runOrganize(args []string) error {
	opts, err := parseOrganizeArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printOrganizeUsage()
		return errFailed
	}
	if opts.planFile != "" {
		return runSavedPlan(opts)
	}
	if len(opts.sources) == 0 || opts.dest == "" {
		printOrganizeUsage()
		return errFailed
	}
	if opts.savePlan != "" && opts.apply {
		fmt.Fprintf(os.Stderr, "Error: --save-plan is for previews; save the plan first, then run 'organize --plan FILE --apply'\n")
		return errFailed
	}

	var sources []string
	for _, s := range opts.sources {
		abs, err := resolveExistingFolder(s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: source folder not found: %s\n", s)
			return errFailed
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "Error: source is not a folder: %s\n", s)
			return errFailed
		}
		sources = append(sources, abs)
	}
	dest, err := filepath.Abs(opts.dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid destination path %q\n", opts.dest)
		return errFailed
	}
	// The library and the sources must be separate trees. If they overlapped,
	// the library would be read back as a source (or the source rewritten).
	realDest := resolveThroughSymlinks(dest)
	for _, s := range sources {
		realSrc := resolveThroughSymlinks(s)
		if pathWithin(realDest, realSrc) || pathWithin(realSrc, realDest) {
			fmt.Fprintf(os.Stderr, "Error: %s and the library %s overlap; choose a separate library folder\n", s, dest)
			return errFailed
		}
	}

	showProgress := progressEnabled(opts.progress)

	plan, err := buildOrganizePlan(sources, dest, opts, showProgress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}
	printOrganizePlan(plan, dest, opts.apply)

	if opts.savePlan != "" {
		if err := savePlanFile(opts.savePlan, sources, dest, plan); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return errFailed
		}
		fmt.Fprintf(os.Stderr, "\nPlan saved to %s (%s files). Copy exactly that with:\n  photo-organizer organize --plan %s --apply\n",
			opts.savePlan, formatCount(len(plan.copies)), opts.savePlan)
		return nil
	}

	if opts.apply && plan.lacksSpace() {
		fmt.Fprintf(os.Stderr, "\nError: not enough free space for the library; nothing was copied.\n")
		return errFailed
	}

	if !opts.apply {
		if len(plan.copies) > 0 {
			fmt.Fprintf(os.Stderr, "\nDry run — nothing was written. Re-run with --apply to copy.\n")
		}
		return nil
	}
	if len(plan.copies) == 0 {
		// Nothing to copy, but keep what was learned about the library so the
		// next run does not read it again.
		if plan.hs.dirty {
			if _, statErr := os.Stat(dest); statErr == nil {
				if err := plan.hs.saveCache(dest, nil); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not save the library hash cache: %v\n", err)
				}
			}
		}
		return nil
	}

	return finishApply(plan, dest, showProgress)
}

// finishApply copies the plan, then refreshes the library manifest.
func finishApply(plan *organizePlan, dest string, showProgress bool) error {
	failed, err := applyOrganizePlan(plan, dest, showProgress)
	if errors.Is(err, errInterrupted) {
		fmt.Fprintf(os.Stderr, "Run 'photo-organizer scan %s' to record what was copied.\n", dest)
		return errFailed
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return errFailed
	}

	if err := refreshLocalManifest(dest); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: copied files, but could not refresh the library manifest: %v\n", err)
		fmt.Fprintf(os.Stderr, "Run 'photo-organizer scan %s' to record it.\n", dest)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠  %d file(s) failed to copy; re-run the same command to retry them.\n", failed)
		return errFailed
	}
	return nil
}

// resolveThroughSymlinks resolves symlinks in path, including when the tail of
// the path does not exist yet (a library about to be created), so an overlap
// hidden behind a symlink is still caught.
func resolveThroughSymlinks(path string) string {
	path = filepath.Clean(path)
	rest := ""
	cur := path
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return path
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// pathWithin reports whether path is root or lies inside it.
func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// -----------------------------------------------------------------------------
// Planning (read-only)
// -----------------------------------------------------------------------------

func contentKey(size int64, hash string) string {
	return fmt.Sprintf("%d:%s", size, hash)
}

// isJunkDir reports folders that hold system data, thumbnails or trash rather
// than photos you took. Their contents would otherwise be copied as photos
// (Synology @eaDir alone can hold a thumbnail for every picture).
func isJunkDir(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case "@eadir", "#recycle", "@recycle", ".thumbnails", "$recycle.bin",
		"system volume information", ".spotlight-v100", ".fseventsd",
		".trashes", ".trash", "__macosx", ".appledouble", ".temporaryitems",
		"lost+found", ".stversions", ".stfolder", organizeLogDir:
		return true
	}
	return strings.HasPrefix(l, ".trash-") || strings.HasSuffix(l, ".lrdata")
}

// noiseFile reports OS litter that is not worth mentioning as "left behind".
func noiseFile(name string) bool {
	switch strings.ToLower(name) {
	case "thumbs.db", "desktop.ini", ".ds_store", ".photoignore", ".localized":
		return true
	}
	return strings.HasPrefix(name, "._")
}

// libraryIndex maps content to the library files that hold it, so a source
// file can be recognised as already present.
type libraryIndex map[string][]string

type sourceFile struct {
	path  string
	size  int64
	mtime int64 // UnixNano
}

// sourceScan is what walking the source folders found.
type sourceScan struct {
	groups       map[string][]sourceFile
	skippedTypes map[string]int // unrecognised extension -> files left behind
	junkDirs     map[string]int // skipped system/thumbnail folder name -> count
}

// scanSources walks every source (read-only) and groups files that belong
// together: same folder and same name apart from the extension, so IMG_1.CR3,
// IMG_1.JPG and IMG_1.xmp are handled as one unit.
func scanSources(sources []string, showProgress bool) sourceScan {
	res := sourceScan{
		groups:       map[string][]sourceFile{},
		skippedTypes: map[string]int{},
		junkDirs:     map[string]int{},
	}
	prog := newProgressReporter("Finding files", unitFiles, 0, 0, showProgress)
	var n int64
	for _, root := range sources {
		ignore := newPhotoIgnore(root)
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != root && isJunkDir(d.Name()) {
					res.junkDirs[d.Name()]++
					return filepath.SkipDir
				}
				return nil
			}
			if shouldSkipFile(path) || noiseFile(d.Name()) || ignore.ShouldSkip(path) {
				return nil
			}
			ext := filepath.Ext(path)
			if !isMediaFile(ext) {
				if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
					label := strings.ToLower(ext)
					if label == "" {
						label = "(no extension)"
					}
					res.skippedTypes[label]++
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			stem := strings.TrimSuffix(filepath.Base(path), ext)
			// "IMG_1.jpg.xmp" belongs with IMG_1.jpg, not in a group of its own.
			if inner := filepath.Ext(stem); isSidecarExt(ext) && isMediaFile(inner) && !isSidecarExt(inner) {
				stem = strings.TrimSuffix(stem, inner)
			}
			key := filepath.Join(filepath.Dir(path), strings.ToLower(stem))
			res.groups[key] = append(res.groups[key], sourceFile{path: path, size: info.Size(), mtime: info.ModTime().UnixNano()})
			n++
			prog.Add(1, 1)
			return nil
		})
	}
	prog.Done(fmt.Sprintf("Found %s files", formatCount(int(n))))
	return res
}

func isSidecarExt(ext string) bool { return sidecarExts[strings.ToLower(ext)] }

// captureFolder returns the library subfolder for a capture date.
func captureFolder(t time.Time, ok, byMonth bool) string {
	if !ok {
		return undatedFolder
	}
	if byMonth {
		return filepath.Join(t.Format("2006"), t.Format("2006-01"))
	}
	return filepath.Join(t.Format("2006"), t.Format("2006-01-02"))
}

// plausibleDate rejects the 1970 epoch, camera-clock-unset dates and the future.
func plausibleDate(t time.Time) bool {
	return t.Year() >= 1990 && t.Before(time.Now().Add(48*time.Hour))
}

// planner holds the state shared while deciding where every file goes.
type planner struct {
	dest string
	opts organizeOptions
	plan *organizePlan

	idx          libraryIndex
	taken        map[string]bool // lowercased library + planned paths
	hs           *hashStore
	plannedSrc   map[string]string // planned library path -> source it comes from
	plannedBySrc map[string]string // source path -> library path it will have
}

func buildOrganizePlan(sources []string, dest string, opts organizeOptions, showProgress bool) (*organizePlan, error) {
	hs := newHashStore(opts.verify, opts.jobs)
	idx, taken, staleTmp, err := indexLibrary(dest, hs, showProgress)
	if err != nil {
		return nil, fmt.Errorf("read library %s: %w", dest, err)
	}

	found := scanSources(sources, showProgress)
	keys := make([]string, 0, len(found.groups))
	var totalFiles int64
	for k, files := range found.groups {
		keys = append(keys, k)
		totalFiles += int64(len(files))
	}
	sort.Strings(keys) // deterministic: the same inputs always produce the same plan

	plan := &organizePlan{
		byFolder:     map[string]int{},
		staleTmp:     staleTmp,
		hs:           hs,
		dateSources:  map[dateSource]int{},
		jan1:         map[string]int{},
		skippedTypes: found.skippedTypes,
		junkDirs:     found.junkDirs,
	}
	pl := &planner{
		dest: dest, opts: opts, plan: plan, idx: idx, taken: taken, hs: hs,
		plannedSrc: map[string]string{}, plannedBySrc: map[string]string{},
	}

	prog := newProgressReporter("Examining", unitFiles, totalFiles, totalFiles, showProgress)
	// Work in batches: analyse a batch's files in parallel (hashes, dates), warm
	// the digests the duplicate checks will need, then plan sequentially so the
	// result is identical to a single-threaded run.
	const planBatch = 256
	for start := 0; start < len(keys); start += planBatch {
		end := start + planBatch
		if end > len(keys) {
			end = len(keys)
		}
		infos := make([]*groupInfo, end-start)
		parallelDo(opts.jobs, len(infos), func(i int) {
			files := found.groups[keys[start+i]]
			sort.Slice(files, func(a, b int) bool { return files[a].path < files[b].path })
			infos[i] = analyzeGroup(files, opts.videoTZ)
		})
		pl.warmDigests(infos)
		for i, g := range infos {
			pl.planGroup(g)
			prog.Add(0, int64(len(found.groups[keys[start+i]])))
		}
	}
	prog.Done(fmt.Sprintf("Examined %s files", formatCount(int(totalFiles))))

	plan.freeBytes, plan.freeKnown = freeSpaceFor(dest)
	return plan, nil
}

// freeSpaceFor reports the free space on the filesystem that will hold dest,
// looking at the nearest folder that already exists.
func freeSpaceFor(dest string) (uint64, bool) {
	p := dest
	for {
		if _, err := os.Stat(p); err == nil {
			return diskFreeBytes(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, false
		}
		p = parent
	}
}

// groupInfo is the read-only analysis of one photo/video group. Computing it
// touches only the source files, so many groups can be analysed at once.
type groupInfo struct {
	media, sidecars []sourceFile
	empty           int
	lead            int
	hashes          []string
	date            time.Time
	source          dateSource
}

func analyzeGroup(files []sourceFile, videoTZ *time.Location) *groupInfo {
	g := &groupInfo{}
	for _, f := range files {
		if isSidecarExt(filepath.Ext(f.path)) {
			g.sidecars = append(g.sidecars, f)
		} else if f.size == 0 {
			g.empty++ // an empty file is not a photo, and all of them look identical
		} else {
			g.media = append(g.media, f)
		}
	}
	if len(g.media) == 0 {
		return g
	}

	// The first photo (else video) sets the date for the whole group, so
	// RAW+JPG pairs and sidecars land together.
	for i, f := range g.media {
		if photoExts[strings.ToLower(filepath.Ext(f.path))] {
			g.lead = i
			break
		}
	}
	g.hashes = make([]string, len(g.media))
	for i, f := range g.media {
		g.hashes[i], _ = processFile(f.path)
	}
	g.date, g.source = captureDateFor(g.media[g.lead].path, videoTZ)
	return g
}

// warmDigests precomputes, in parallel, the digests that duplicate checks in
// this batch will ask for: files whose content key already exists in the
// library or earlier in the batch.
func (pl *planner) warmDigests(infos []*groupInfo) {
	seen := map[string]string{} // content key -> first path in this batch
	queued := map[string]bool{}
	var need []sourceFile
	add := func(path string, size int64) {
		if !queued[path] {
			queued[path] = true
			need = append(need, sourceFile{path: path, size: size})
		}
	}
	for _, g := range infos {
		for i, f := range g.media {
			if g.hashes[i] == "" {
				continue
			}
			k := contentKey(f.size, g.hashes[i])
			hit := false
			for _, c := range pl.idx[k] {
				if c != f.path {
					add(c, f.size)
					hit = true
				}
			}
			if first, ok := seen[k]; ok && first != f.path {
				add(first, f.size)
				hit = true
			} else if !ok {
				seen[k] = f.path
			}
			if hit {
				add(f.path, f.size)
			}
		}
	}
	pl.hs.warm(need)
}

// planGroup decides the fate of one photo/video together with its sidecars.
func (pl *planner) planGroup(g *groupInfo) {
	plan := pl.plan
	plan.emptySkipped += g.empty
	media, sidecars, hashes := g.media, g.sidecars, g.hashes
	if len(media) == 0 {
		plan.orphanSidecars += len(sidecars)
		return
	}
	lead := g.lead
	date, source := g.date, g.source
	hasDate := plausibleDate(date)
	folder := captureFolder(date, hasDate, pl.opts.byMonth)

	// placed records where each source file's content lives (or will live) in
	// the library, so sidecars can be put beside it.
	placed := map[string]string{}
	byName := map[string]string{} // lowercased base name -> source path

	for i, f := range media {
		byName[strings.ToLower(filepath.Base(f.path))] = f.path
		if hashes[i] == "" {
			plan.unreadable = append(plan.unreadable, f.path)
			continue
		}
		k := contentKey(f.size, hashes[i])

		if dupOf := pl.firstIdentical(pl.idx[k], f.path, f.size); dupOf != "" {
			if !pl.hs.wantsFull(f.size) {
				plan.sampledDups++
			}
			plan.duplicates++
			plan.duplicateBytes += f.size
			if pathWithin(dupOf, pl.dest) {
				placed[f.path] = dupOf
			} else {
				placed[f.path] = pl.plannedBySrc[dupOf]
			}
			continue
		}

		dst := uniqueDestination(filepath.Join(pl.dest, folder), filepath.Base(f.path), pl.taken)
		pl.reserve(dst, f.path)
		pl.idx[k] = append(pl.idx[k], f.path) // later copies of this content in the run are duplicates
		placed[f.path] = dst

		plan.addCopy(plannedCopy{src: f.path, dst: dst, size: f.size, hash: hashes[i], mtime: f.mtime, date: date, hasDate: hasDate},
			strings.SplitN(folder, string(filepath.Separator), 2)[0])
		if !hasDate {
			plan.undated++
		}
		plan.noteDate(date, source, hasDate, media[lead].path)
	}

	// Sidecars are never judged by content alone: two photos can legitimately
	// have identical .xmp files. Each goes beside its own photo, named after it.
	for _, sc := range sidecars {
		ext := filepath.Ext(sc.path)
		stem := strings.TrimSuffix(filepath.Base(sc.path), ext)

		var base string
		dotted := false
		if owner, ok := byName[strings.ToLower(stem)]; ok { // IMG_1.jpg.xmp style
			base, dotted = placed[owner], true
		} else {
			base = placed[media[lead].path]
		}
		if base == "" {
			plan.orphanSidecars++
			continue
		}
		var target string
		if dotted {
			target = base + ext
		} else {
			target = strings.TrimSuffix(base, filepath.Ext(base)) + ext
		}

		if pl.taken[strings.ToLower(target)] {
			existing := target
			if from, ok := pl.plannedSrc[target]; ok {
				existing = from
			}
			if sameContent(existing, sc.path) {
				plan.duplicates++
				plan.duplicateBytes += sc.size
			} else {
				plan.conflictSidecars = append(plan.conflictSidecars, sc.path)
			}
			continue
		}
		pl.reserve(target, sc.path)
		plan.addCopy(plannedCopy{src: sc.path, dst: target, size: sc.size, mtime: sc.mtime, date: date, hasDate: hasDate}, "")
	}
}

// noteDate records how a copied file was dated, for the summary.
func (p *organizePlan) noteDate(date time.Time, source dateSource, hasDate bool, leadPath string) {
	p.dateSources[source]++
	if source == dateFromModTime && len(p.mtimeExamples) < 3 {
		p.mtimeExamples = append(p.mtimeExamples, leadPath)
	}
	if hasDate && (source == dateFromEXIF || source == dateFromVideo) && date.Month() == time.January && date.Day() == 1 {
		p.jan1[date.Format("2006-01-02")]++
	}
}

func (pl *planner) reserve(dst, src string) {
	pl.taken[strings.ToLower(dst)] = true
	pl.plannedSrc[dst] = src
	pl.plannedBySrc[src] = dst
}

func (p *organizePlan) addCopy(c plannedCopy, yearFolder string) {
	p.copies = append(p.copies, c)
	p.copyBytes += c.size
	if yearFolder != "" {
		p.byFolder[yearFolder]++
	}
}

// firstIdentical returns the first candidate with the same content as path.
// The scan hash only samples the first and last 32KB, so a match there is
// confirmed against a stronger digest (see dupcheck.go) before a file is
// called a duplicate — a near-identical but different photo is never dropped.
func (pl *planner) firstIdentical(candidates []string, path string, size int64) string {
	for _, c := range candidates {
		if pl.hs.same(c, path, size) {
			return c
		}
	}
	return ""
}

// sameContent compares two small files byte for byte (used for sidecars).
func sameContent(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ia, err1 := fa.Stat()
	ib, err2 := fb.Stat()
	if err1 != nil || err2 != nil || ia.Size() != ib.Size() {
		return false
	}
	bufA := make([]byte, 256<<10)
	bufB := make([]byte, 256<<10)
	for {
		na, ea := io.ReadFull(fa, bufA)
		nb, eb := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false
		}
		aDone := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bDone := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if aDone || bDone {
			return aDone && bDone
		}
		if ea != nil || eb != nil {
			return false
		}
	}
}

// uniqueDestination picks dir/name, or dir/name_1.ext, _2... when that path is
// already in the library or already planned. Comparison ignores case so the
// result is also safe on case-insensitive filesystems.
func uniqueDestination(dir, name string, taken map[string]bool) string {
	candidate := filepath.Join(dir, name)
	if !taken[strings.ToLower(candidate)] {
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; ; n++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, n, ext))
		if taken[strings.ToLower(candidate)] {
			continue
		}
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
}

// topCounts renders "a ×3, b ×1" for the largest entries.
func topCounts(m map[string]int, limit int) string {
	type kv struct {
		k string
		n int
	}
	var all []kv
	for k, n := range m {
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, e := range all {
		if i == limit {
			parts = append(parts, fmt.Sprintf("and %d more", len(all)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%s", e.k, formatCount(e.n)))
	}
	return strings.Join(parts, ", ")
}

func (p *organizePlan) lacksSpace() bool {
	return p.freeKnown && uint64(p.copyBytes) > p.freeBytes
}

// printDateSummary shows where the dates came from and flags the doubtful ones.
func (p *organizePlan) printDateSummary() {
	var parts []string
	for _, src := range []dateSource{dateFromEXIF, dateFromVideo, dateFromFilename, dateFromModTime} {
		if n := p.dateSources[src]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", src, formatCount(n)))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(os.Stderr, "Dates taken from: %s\n", strings.Join(parts, " · "))
	}
	if n := p.dateSources[dateFromModTime]; n > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %s files have no date inside them and are placed by file modified time, which a copy or restore\n"+
			"   changes — check these, e.g. %s\n", formatCount(n), strings.Join(p.mtimeExamples, ", "))
	}
	var suspect []string
	for day, n := range p.jan1 {
		if n >= 10 {
			suspect = append(suspect, fmt.Sprintf("%s (%s files)", day, formatCount(n)))
		}
	}
	sort.Strings(suspect)
	if len(suspect) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  Many files are dated January 1st — a camera with an unset clock does this: %s\n",
			strings.Join(suspect, ", "))
	}
}

func printOrganizePlan(p *organizePlan, dest string, apply bool) {
	verb := "Would copy"
	if apply {
		verb = "Will copy"
	}
	fmt.Fprintf(os.Stderr, "\n%s %s files (%s) into %s\n", verb, formatCount(len(p.copies)), formatSize(p.copyBytes), dest)
	fmt.Fprintf(os.Stderr, "Skipping %s files (%s) already in the library or duplicated within the sources\n",
		formatCount(p.duplicates), formatSize(p.duplicateBytes))
	if p.sampledDups > 0 {
		fmt.Fprintf(os.Stderr, "   %s of those are large files confirmed by sampling 16 points across the file;\n"+
			"   use --verify full to compare every byte\n", formatCount(p.sampledDups))
	}
	if p.emptySkipped > 0 {
		fmt.Fprintf(os.Stderr, "Skipping %s empty (0-byte) files\n", formatCount(p.emptySkipped))
	}
	if p.orphanSidecars > 0 {
		fmt.Fprintf(os.Stderr, "Ignoring %s sidecar files with no photo or video to attach to\n", formatCount(p.orphanSidecars))
	}
	if p.undated > 0 {
		fmt.Fprintf(os.Stderr, "%s files have no usable date and go to %s/\n", formatCount(p.undated), undatedFolder)
	}
	if len(p.staleTmp) > 0 {
		fmt.Fprintf(os.Stderr, "Found %d leftover temp file(s) from an interrupted run; --apply removes the ones older than %s\n",
			len(p.staleTmp), staleTmpAge)
	}
	p.printDateSummary()
	if len(p.junkDirs) > 0 {
		fmt.Fprintf(os.Stderr, "Skipped system/thumbnail folders: %s\n", topCounts(p.junkDirs, 6))
	}
	if len(p.unreadable) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %d unreadable file(s) skipped, e.g. %s\n", len(p.unreadable), p.unreadable[0])
	}
	if len(p.conflictSidecars) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %d sidecar(s) differ from the one already beside their photo and were not copied, e.g. %s\n",
			len(p.conflictSidecars), p.conflictSidecars[0])
	}
	if len(p.skippedTypes) > 0 {
		total := 0
		for _, n := range p.skippedTypes {
			total += n
		}
		fmt.Fprintf(os.Stderr, "⚠  %s other files are NOT photos/videos and will not be copied: %s\n",
			formatCount(total), topCounts(p.skippedTypes, 8))
		fmt.Fprintf(os.Stderr, "   They stay only in the source — keep them before you retire or delete it.\n")
	}
	if p.freeKnown {
		if p.lacksSpace() {
			fmt.Fprintf(os.Stderr, "⚠  Not enough free space: need %s, only %s available\n",
				formatSize(p.copyBytes), formatSize(int64(p.freeBytes)))
		} else {
			fmt.Fprintf(os.Stderr, "Free space at the library: %s\n", formatSize(int64(p.freeBytes)))
		}
	}

	if len(p.byFolder) > 0 {
		folders := make([]string, 0, len(p.byFolder))
		for f := range p.byFolder {
			folders = append(folders, f)
		}
		sort.Strings(folders)
		fmt.Fprintf(os.Stderr, "\nBy year:\n")
		for _, f := range folders {
			fmt.Fprintf(os.Stderr, "  %-8s %s files\n", f, formatCount(p.byFolder[f]))
		}
	}
	if !apply && len(p.copies) > 0 {
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		step := len(p.copies)/5 + 1
		for i := 0; i < len(p.copies); i += step {
			rel, _ := filepath.Rel(dest, p.copies[i].dst)
			fmt.Fprintf(os.Stderr, "  %s\n    -> %s\n", p.copies[i].src, rel)
		}
	}
}

// -----------------------------------------------------------------------------
// Applying (the only part that writes — and only inside the library)
// -----------------------------------------------------------------------------

// staleTmpAge is how old a leftover temp file must be before it is removed.
// A copy in progress keeps updating its file, so anything this old is dead.
const staleTmpAge = time.Hour

// errInterrupted means the user stopped the run with Ctrl-C or SIGTERM.
var errInterrupted = errors.New("interrupted")

// removeStaleTemp deletes temp files an earlier, killed run left behind.
func removeStaleTemp(paths []string) int {
	removed := 0
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) < staleTmpAge {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}

// ctxWriter stops a copy in the middle of a file once ctx is cancelled.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c ctxWriter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(b)
}

func applyOrganizePlan(p *organizePlan, dest string, showProgress bool) (failed int, err error) {
	// Ctrl-C (or SIGTERM) finishes cleanly: the half-written file is removed and
	// the log is written. A second Ctrl-C kills the process immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	if n := removeStaleTemp(p.staleTmp); n > 0 {
		fmt.Fprintf(os.Stderr, "Removed %d leftover temp file(s) from an earlier interrupted run\n", n)
	}

	logDir := filepath.Join(dest, organizeLogDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return 0, fmt.Errorf("create log folder: %w", err)
	}
	logPath := filepath.Join(logDir, "organize-"+time.Now().Format("20060102-150405")+".csv")
	lf, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("create log: %w", err)
	}
	defer lf.Close()
	lw := csv.NewWriter(lf)
	defer lw.Flush()
	lw.Write([]string{"source", "library_path", "size_bytes", "status"})

	prog := newProgressReporter("Copying", unitBytes, p.copyBytes, int64(len(p.copies)), showProgress)
	copied := 0
	var added []newCopy
	for _, c := range p.copies {
		if ctx.Err() != nil {
			break
		}
		prog.Note(filepath.Base(c.src))
		if err := safeCopyIntoLibrary(ctx, c, prog); err != nil {
			prog.Clear()
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(os.Stderr, "  failed: %s: %v\n", c.src, err)
			lw.Write([]string{c.src, c.dst, fmt.Sprint(c.size), "failed: " + err.Error()})
			failed++
			continue
		}
		lw.Write([]string{c.src, c.dst, fmt.Sprint(c.size), "copied"})
		if c.hash != "" { // photos and videos, not sidecars
			added = append(added, newCopy{dst: c.dst, size: c.size, mtime: c.mtime, partial: c.hash, from: c.src})
		}
		copied++
		prog.Add(0, 1)
	}
	// Remember what is now in the library so the next run does not re-read it.
	if p.hs != nil {
		if err := p.hs.saveCache(dest, added); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not save the library hash cache (next run will be slower): %v\n", err)
		}
	}
	if ctx.Err() != nil {
		prog.Clear()
		fmt.Fprintf(os.Stderr, "\nInterrupted after copying %s of %s files. The file being copied was discarded;\n"+
			"everything already copied is complete. Run the same command again to continue.\n",
			formatCount(copied), formatCount(len(p.copies)))
		fmt.Fprintf(os.Stderr, "Log: %s\n", logPath)
		return failed, errInterrupted
	}
	prog.Done(fmt.Sprintf("Copied %s files (%s)", formatCount(copied), formatSize(p.copyBytes)))
	fmt.Fprintf(os.Stderr, "Log: %s\n", logPath)
	return failed, nil
}

// safeCopyIntoLibrary copies c.src to c.dst without ever replacing an existing
// file: the bytes go to a temporary name first, are size-checked, and are then
// linked into place (which fails if the name is taken). The source is only read.
func safeCopyIntoLibrary(ctx context.Context, c plannedCopy, prog *progressReporter) (err error) {
	dir := filepath.Dir(c.dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src, err := os.Open(c.src)
	if err != nil {
		return err
	}
	defer src.Close()
	srcInfo, err := src.Stat()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, organizeTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	n, copyErr := io.Copy(ctxWriter{ctx, &progressWriter{tmp, prog}}, src)
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if n != c.size || n != srcInfo.Size() {
		return fmt.Errorf("size changed while copying (%d bytes read, expected %d)", n, c.size)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	// Keep the original modified time so date-based tools still see the truth.
	os.Chtimes(tmpName, srcInfo.ModTime(), srcInfo.ModTime())

	if err := os.Link(tmpName, c.dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s appeared during the run; not overwriting", c.dst)
		}
		// Filesystem without hard links: rename after confirming the name is free.
		if _, statErr := os.Lstat(c.dst); statErr == nil {
			return fmt.Errorf("%s appeared during the run; not overwriting", c.dst)
		}
		return os.Rename(tmpName, c.dst)
	}
	return os.Remove(tmpName)
}
