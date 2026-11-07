package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	sources  []string
	dest     string
	apply    bool
	byMonth  bool
	progress progressMode
}

// plannedCopy is one file that will be copied into the library.
type plannedCopy struct {
	src     string
	dst     string
	size    int64
	hash    string
	date    time.Time
	hasDate bool
}

// organizePlan is everything a dry run reports and an apply run executes.
type organizePlan struct {
	copies         []plannedCopy
	copyBytes      int64
	duplicates     int // content already in the library or earlier in this run
	duplicateBytes int64
	orphanSidecars int // .xmp/.aae/... with no photo or video beside them
	unreadable     []string
	undated        int
	byFolder       map[string]int // top-level year folder -> files
}

func parseOrganizeArgs(args []string) (organizeOptions, error) {
	opts := organizeOptions{progress: progressAuto}
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
}

func runOrganize(args []string) error {
	opts, err := parseOrganizeArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printOrganizeUsage()
		return errFailed
	}
	if len(opts.sources) == 0 || opts.dest == "" {
		printOrganizeUsage()
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
	for _, s := range sources {
		if pathWithin(dest, s) || pathWithin(s, dest) {
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

	if !opts.apply {
		if len(plan.copies) > 0 {
			fmt.Fprintf(os.Stderr, "\nDry run — nothing was written. Re-run with --apply to copy.\n")
		}
		return nil
	}
	if len(plan.copies) == 0 {
		return nil
	}

	failed, err := applyOrganizePlan(plan, dest, showProgress)
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

// libraryIndex maps content to the library files that hold it, so a source
// file can be recognised as already present.
type libraryIndex map[string][]string

func indexLibrary(dest string, showProgress bool) (libraryIndex, map[string]bool, error) {
	idx := libraryIndex{}
	names := map[string]bool{} // lowercased library paths, for collision checks
	if _, err := os.Stat(dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return idx, names, nil
		}
		return nil, nil, err
	}

	prog := newProgressReporter("Indexing library", unitFiles, 0, 0, showProgress)
	var count int64
	err := filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dest && d.Name() == organizeLogDir {
				return filepath.SkipDir
			}
			return nil
		}
		names[strings.ToLower(path)] = true
		if shouldSkipFile(path) || !isMediaFile(filepath.Ext(path)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		hash, _ := processFile(path)
		if hash != "" {
			k := contentKey(info.Size(), hash)
			idx[k] = append(idx[k], path)
		}
		count++
		prog.Add(1, 1)
		return nil
	})
	prog.Done(fmt.Sprintf("Indexed %s existing library files", formatCount(int(count))))
	return idx, names, err
}

type sourceFile struct {
	path string
	size int64
}

// groupSources walks every source (read-only) and groups files that belong
// together: same folder and same name apart from the extension, so IMG_1.CR3,
// IMG_1.JPG and IMG_1.xmp are handled as one unit.
func groupSources(sources []string, showProgress bool) map[string][]sourceFile {
	groups := map[string][]sourceFile{}
	prog := newProgressReporter("Finding files", unitFiles, 0, 0, showProgress)
	var n int64
	for _, root := range sources {
		ignore := newPhotoIgnore(root)
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == organizeLogDir {
					return filepath.SkipDir
				}
				return nil
			}
			// "._name" files are macOS AppleDouble metadata, not photos.
			if strings.HasPrefix(d.Name(), "._") || shouldSkipFile(path) ||
				!isMediaFile(filepath.Ext(path)) || ignore.ShouldSkip(path) {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			ext := filepath.Ext(path)
			key := filepath.Join(filepath.Dir(path), strings.ToLower(strings.TrimSuffix(filepath.Base(path), ext)))
			groups[key] = append(groups[key], sourceFile{path: path, size: info.Size()})
			n++
			prog.Add(1, 1)
			return nil
		})
	}
	prog.Done(fmt.Sprintf("Found %s files", formatCount(int(n))))
	return groups
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

func buildOrganizePlan(sources []string, dest string, opts organizeOptions, showProgress bool) (*organizePlan, error) {
	idx, taken, err := indexLibrary(dest, showProgress)
	if err != nil {
		return nil, fmt.Errorf("read library %s: %w", dest, err)
	}

	groups := groupSources(sources, showProgress)
	keys := make([]string, 0, len(groups))
	var totalFiles int64
	for k, files := range groups {
		keys = append(keys, k)
		totalFiles += int64(len(files))
	}
	sort.Strings(keys) // deterministic: the same inputs always produce the same plan

	plan := &organizePlan{byFolder: map[string]int{}}
	prog := newProgressReporter("Examining", unitFiles, totalFiles, totalFiles, showProgress)

	for _, key := range keys {
		files := groups[key]
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

		// The first photo (else video) in the group sets the date for all of it,
		// so sidecars and RAW+JPG pairs land together.
		lead := -1
		for i, f := range files {
			if photoExts[strings.ToLower(filepath.Ext(f.path))] {
				lead = i
				break
			}
		}
		if lead < 0 {
			for i, f := range files {
				if !isSidecarExt(filepath.Ext(f.path)) {
					lead = i
					break
				}
			}
		}
		if lead < 0 {
			plan.orphanSidecars += len(files)
			prog.Add(0, int64(len(files)))
			continue
		}

		type hashed struct {
			hash string
			date time.Time
		}
		results := make([]hashed, len(files))
		for i, f := range files {
			h, d := processFile(f.path)
			results[i] = hashed{h, d}
		}
		date := results[lead].date
		hasDate := plausibleDate(date)
		folder := captureFolder(date, hasDate, opts.byMonth)

		for i, f := range files {
			prog.Add(0, 1)
			if results[i].hash == "" {
				plan.unreadable = append(plan.unreadable, f.path)
				continue
			}
			k := contentKey(f.size, results[i].hash)

			if dupOf := firstIdentical(idx[k], f.path); dupOf != "" {
				plan.duplicates++
				plan.duplicateBytes += f.size
				continue
			}

			dst := uniqueDestination(filepath.Join(dest, folder), filepath.Base(f.path), taken)
			taken[strings.ToLower(dst)] = true
			idx[k] = append(idx[k], f.path) // later copies of this content in the run are duplicates

			plan.copies = append(plan.copies, plannedCopy{
				src: f.path, dst: dst, size: f.size, hash: results[i].hash,
				date: date, hasDate: hasDate,
			})
			plan.copyBytes += f.size
			if !hasDate {
				plan.undated++
			}
			plan.byFolder[strings.SplitN(folder, string(filepath.Separator), 2)[0]]++
		}
	}
	prog.Done(fmt.Sprintf("Examined %s files", formatCount(int(totalFiles))))
	return plan, nil
}

// firstIdentical returns the first candidate whose bytes equal those of path.
// The manifest hash only samples the start and end of a file, so a matching
// hash is confirmed byte-for-byte before a file is called a duplicate — a
// near-identical but different photo is never silently dropped.
func firstIdentical(candidates []string, path string) string {
	for _, c := range candidates {
		if c == path {
			return c
		}
		if sameContent(c, path) {
			return c
		}
	}
	return ""
}

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

func printOrganizePlan(p *organizePlan, dest string, apply bool) {
	verb := "Would copy"
	if apply {
		verb = "Will copy"
	}
	fmt.Fprintf(os.Stderr, "\n%s %s files (%s) into %s\n", verb, formatCount(len(p.copies)), formatSize(p.copyBytes), dest)
	fmt.Fprintf(os.Stderr, "Skipping %s files (%s) already in the library or duplicated within the sources\n",
		formatCount(p.duplicates), formatSize(p.duplicateBytes))
	if p.orphanSidecars > 0 {
		fmt.Fprintf(os.Stderr, "Ignoring %s sidecar files with no photo or video beside them\n", formatCount(p.orphanSidecars))
	}
	if p.undated > 0 {
		fmt.Fprintf(os.Stderr, "%s files have no usable date and go to %s/\n", formatCount(p.undated), undatedFolder)
	}
	if len(p.unreadable) > 0 {
		fmt.Fprintf(os.Stderr, "⚠  %d unreadable file(s) skipped, e.g. %s\n", len(p.unreadable), p.unreadable[0])
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

func applyOrganizePlan(p *organizePlan, dest string, showProgress bool) (failed int, err error) {
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
	for _, c := range p.copies {
		prog.Note(filepath.Base(c.src))
		if err := safeCopyIntoLibrary(c, prog); err != nil {
			prog.Clear()
			fmt.Fprintf(os.Stderr, "  failed: %s: %v\n", c.src, err)
			lw.Write([]string{c.src, c.dst, fmt.Sprint(c.size), "failed: " + err.Error()})
			failed++
			continue
		}
		lw.Write([]string{c.src, c.dst, fmt.Sprint(c.size), "copied"})
		copied++
		prog.Add(0, 1)
	}
	prog.Done(fmt.Sprintf("Copied %s files (%s)", formatCount(copied), formatSize(p.copyBytes)))
	fmt.Fprintf(os.Stderr, "Log: %s\n", logPath)
	return failed, nil
}

// safeCopyIntoLibrary copies c.src to c.dst without ever replacing an existing
// file: the bytes go to a temporary name first, are size-checked, and are then
// linked into place (which fails if the name is taken). The source is only read.
func safeCopyIntoLibrary(c plannedCopy, prog *progressReporter) (err error) {
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

	n, copyErr := io.Copy(&progressWriter{tmp, prog}, src)
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
