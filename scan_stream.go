package main

// Streaming scan: hash a folder and write its manifest as the walk goes, so memory depends on
// one batch of files, not on how many files the folder has.
//
// How it works:
//   - Manifests are kept in folder order (see pathCompare), the same order filepath.Walk
//     visits files in. That lets the old manifest be read one row at a time next to the walk.
//   - The walk is cut into batches. Each batch is looked up in the old manifest, hashed in
//     parallel when needed, and its finished rows are appended to a journal file
//     (<manifest>.partial.<n>) and flushed. Nothing else is kept.
//   - When the walk ends, the old manifest and the journal are merged into a new manifest,
//     which replaces the old one atomically. Old rows nobody saw are dropped (pruned) unless
//     the half-the-rows brake or an unreadable folder says not to.
//   - A scan that dies leaves its journal behind. The next scan uses leftover journals as a
//     hash cache, so hours of hashing are not lost.

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Manifest columns, in file order.
const (
	mcFilename = iota
	mcRel
	mcSize
	mcSizeMB
	mcModified
	mcCapture
	mcMake
	mcModel
	mcPartial
	mcFull
	mcExt
	mcScanDate
	mcScanPath
	mcMachine
)

var manifestColumns = []string{
	"filename",
	"relative_path",
	"file_size_bytes",
	"file_size_mb",
	"file_modified",
	"capture_date",
	"camera_make",
	"camera_model",
	"partial_hash",
	"full_hash",
	"extension",
	"scan_date",
	"scan_path",
	"machine_name",
}

const scanBatchSize = 2000

// pathCompare orders relative paths the way filepath.Walk visits them: folder by folder,
// names compared as whole components. (Plain string order differs: "a b/x" sorts before "a/x"
// as a string, but the walk visits a/x first.)
func pathCompare(a, b string) int {
	sep := byte(filepath.Separator)
	for {
		ai := strings.IndexByte(a, sep)
		bi := strings.IndexByte(b, sep)
		ac, bc := a, b
		if ai >= 0 {
			ac = a[:ai]
		}
		if bi >= 0 {
			bc = b[:bi]
		}
		if c := strings.Compare(ac, bc); c != 0 {
			return c
		}
		switch {
		case ai < 0 && bi < 0:
			return 0
		case ai < 0:
			return -1
		case bi < 0:
			return 1
		}
		a, b = a[ai+1:], b[bi+1:]
	}
}

// rowCursor reads a manifest one row at a time, remapped to the current columns (older
// manifests used other names and positions).
type rowCursor struct {
	f       *os.File
	r       *csv.Reader
	col     []int // for each current column, its index in the file, or -1
	lenient bool  // a read error ends the stream quietly (a journal cut off mid-write)
	row     []string
	rel     string
	err     error
}

// openRowCursor opens path. A missing file gives an empty cursor. The first row is loaded.
func openRowCursor(path string, lenient bool) *rowCursor {
	c := &rowCursor{lenient: lenient}
	if path == "" {
		return c
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.err = err
		}
		return c
	}
	c.f = f
	c.r = csv.NewReader(bufio.NewReaderSize(f, 1<<20))
	c.r.FieldsPerRecord = -1
	c.r.ReuseRecord = true
	header, err := c.r.Read()
	if err != nil {
		if err != io.EOF && !lenient {
			c.err = err
		}
		c.close()
		return c
	}
	src := make(map[string]int, len(header))
	for i, h := range header {
		src[h] = i
	}
	c.col = make([]int, len(manifestColumns))
	for i, name := range manifestColumns {
		idx, ok := src[name]
		if !ok && name == "partial_hash" {
			idx, ok = src["file_hash"] // very old manifests
		}
		if !ok {
			idx = -1
		}
		c.col[i] = idx
	}
	if c.col[mcRel] < 0 {
		fmt.Fprintf(os.Stderr, "Warning: %s: missing required column \"relative_path\" (not a valid manifest?)\n", path)
		c.close()
		return c
	}
	c.next()
	return c
}

func (c *rowCursor) close() {
	if c.f != nil {
		c.f.Close()
		c.f = nil
	}
	c.row = nil
}

// next moves to the following usable row; c.row is nil at the end.
func (c *rowCursor) next() {
	if c.r == nil {
		c.row = nil
		return
	}
	for {
		rec, err := c.r.Read()
		if err != nil {
			if err != io.EOF && !c.lenient {
				c.err = err
			}
			c.close()
			return
		}
		if len(rec) < 2 {
			continue
		}
		row := make([]string, len(manifestColumns))
		for i, si := range c.col {
			if si >= 0 && si < len(rec) {
				row[i] = rec[si]
			}
		}
		if row[mcRel] == "" {
			continue
		}
		if c.lenient && len(rec) != len(manifestColumns) {
			continue // a row cut short by a crash
		}
		c.row, c.rel = row, row[mcRel]
		return
	}
}

// take returns the row stored for key (nil if none) and moves past it, calling skipped for
// every row it passes that sorts before key. Keys must be asked for in folder order.
func (c *rowCursor) take(key string, skipped func(row []string)) []string {
	for c.row != nil && pathCompare(c.rel, key) < 0 {
		if skipped != nil {
			skipped(c.row)
		}
		c.next()
	}
	if c.row == nil || pathCompare(c.rel, key) != 0 {
		return nil
	}
	row := c.row
	c.next()
	for c.row != nil && pathCompare(c.rel, key) == 0 {
		row = c.row // a duplicate: the later row wins, as it did when manifests were maps
		c.next()
	}
	return row
}

// drain passes every remaining row, calling skipped for each.
func (c *rowCursor) drain(skipped func(row []string)) {
	for c.row != nil {
		if skipped != nil {
			skipped(c.row)
		}
		c.next()
	}
}

// orderedManifest returns a manifest file whose rows are in folder order: path itself when it
// already is, otherwise a sorted temporary copy (older versions sorted by plain string order,
// so this happens once per manifest). The sort holds that one manifest in memory. cleanup
// removes the copy.
func orderedManifest(path string) (use string, cleanup func(), err error) {
	cleanup = func() {}
	if _, serr := os.Stat(path); serr != nil {
		return "", cleanup, nil
	}
	c := openRowCursor(path, false)
	prev := ""
	ordered, n := true, 0
	for c.row != nil {
		if n > 0 && pathCompare(prev, c.rel) > 0 {
			ordered = false
			break
		}
		prev = c.rel
		n++
		c.next()
	}
	if c.err != nil {
		c.close()
		return "", cleanup, fmt.Errorf("read existing manifest %s: %w", path, c.err)
	}
	if ordered {
		c.close()
		return path, cleanup, nil
	}
	c.close()

	fmt.Fprintf(os.Stderr, "  Sorting %s into folder order (once; written by an older version)...\n", filepath.Base(path))
	all := make(map[string][]string)
	c = openRowCursor(path, false)
	for c.row != nil {
		all[c.rel] = c.row
		c.next()
	}
	if c.err != nil {
		return "", cleanup, fmt.Errorf("read existing manifest %s: %w", path, c.err)
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return pathCompare(keys[i], keys[j]) < 0 })
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-sorted-*")
	if err != nil {
		return "", cleanup, err
	}
	cleanup = func() { os.Remove(tmp.Name()) }
	w := csv.NewWriter(tmp)
	w.Write(manifestColumns)
	for _, k := range keys {
		w.Write(all[k])
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return "", cleanup, err
	}
	if err := tmp.Close(); err != nil {
		return "", cleanup, err
	}
	return tmp.Name(), cleanup, nil
}

func rowSize(row []string) int64 {
	n, err := strconv.ParseInt(row[mcSize], 10, 64)
	if err != nil || row[mcSize] == "" {
		return -1
	}
	return n
}

// scanIntoManifest scans dir and brings the manifest at manifestFile up to date, holding only
// one batch of files in memory. relBase is the folder the manifest describes (dir itself, or a
// parent when dir is only part of it); subtree is dir relative to relBase ("" for all of it) and
// limits pruning. noCache re-hashes everything. prune removes rows for files no longer there.
func scanIntoManifest(dir, relBase, subtree, manifestFile, machineName string, photoIgnore *PhotoIgnore, noCache, prune bool) (ScanStats, ManifestStats, error) {
	var stats ScanStats
	var mstats ManifestStats
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return stats, mstats, fmt.Errorf("directory not found: %s", dir)
	}
	if err := os.MkdirAll(filepath.Dir(manifestFile), 0755); err != nil {
		return stats, mstats, err
	}

	oldPath, cleanupOld, err := orderedManifest(manifestFile)
	if err != nil {
		return stats, mstats, err
	}
	defer cleanupOld()
	old := openRowCursor(oldPath, false)
	defer old.close()

	// Journals a killed scan left behind: rows already hashed, in folder order.
	leftovers, _ := filepath.Glob(manifestFile + ".partial.*")
	var resume []*rowCursor
	for _, lp := range leftovers {
		if noCache {
			os.Remove(lp)
			continue
		}
		resume = append(resume, openRowCursor(lp, true))
	}
	if len(resume) > 0 {
		fmt.Fprintf(os.Stderr, "  Resuming: using the hashes an interrupted scan already saved (%d file(s))\n", len(resume))
	}
	defer func() {
		for _, c := range resume {
			c.close()
		}
	}()

	journalPath := fmt.Sprintf("%s.partial.%d", manifestFile, time.Now().UnixNano())
	jf, err := os.Create(journalPath)
	if err != nil {
		return stats, mstats, err
	}
	jw := csv.NewWriter(jf)
	jw.Write(manifestColumns)

	inScope := func(rel string) bool {
		return subtree == "" || rel == subtree || strings.HasPrefix(rel, subtree+string(filepath.Separator))
	}
	var scoped, unseen int
	skipOld := func(row []string) {
		if inScope(row[mcRel]) {
			scoped++
			unseen++
		}
	}

	scannedAt := time.Now().Format("2006-01-02 15:04:05")
	numWorkers := runtime.NumCPU()
	if numWorkers > 4 {
		numWorkers = 4
	}

	// The walk runs ahead in its own goroutine, one batch at a time.
	var walkStats ScanStats
	var curDir atomic.Value
	curDir.Store("")
	var foundCount atomic.Int64
	batches := make(chan []rawFile, 50) // the walk may run up to ~100k files ahead of the hashing
	go func() {
		defer close(batches)
		batch := make([]rawFile, 0, scanBatchSize)
		walkFiles(dir, photoIgnore, &walkStats, func(rel string) { curDir.Store(rel) }, func(rf rawFile) {
			foundCount.Add(1)
			batch = append(batch, rf)
			if len(batch) == scanBatchSize {
				batches <- batch
				batch = make([]rawFile, 0, scanBatchSize)
			}
		})
		if len(batch) > 0 {
			batches <- batch
		}
	}()

	type found struct {
		old, cand []string
		hash      string
		capture   string
	}
	processed := 0
	var writeErr error
	for batch := range batches {
		items := make([]found, len(batch))
		var todo []int
		for i, rf := range batch {
			rel, _ := filepath.Rel(relBase, rf.path)
			o := old.take(rel, skipOld)
			if o != nil {
				scoped++
			}
			items[i].old = o
			if !noCache {
				var cand []string
				for _, c := range resume {
					if r := c.take(rel, nil); r != nil && rowSize(r) == rf.size {
						cand = r
					}
				}
				if cand == nil && o != nil && rowSize(o) == rf.size {
					cand = o
				}
				items[i].cand = cand
			}
			if items[i].cand == nil {
				todo = append(todo, i)
			} else {
				stats.Cached++
			}
		}

		// Hash what the manifests could not answer, a few files at a time.
		type hashed struct {
			hash    string
			capture time.Time
		}
		results := make([]hashed, len(batch))
		var wg sync.WaitGroup
		work := make(chan int)
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					h, cd := processFile(batch[i].path)
					results[i] = hashed{h, cd}
				}
			}()
		}
		for _, i := range todo {
			work <- i
		}
		close(work)
		wg.Wait()

		for i, rf := range batch {
			it := items[i]
			var partial, full, capture string
			if it.cand != nil {
				partial, full, capture = it.cand[mcPartial], it.cand[mcFull], it.cand[mcCapture]
			} else {
				partial = results[i].hash
				capture = results[i].capture.Format("2006:01:02 15:04:05")
			}
			var row []string
			if it.old != nil {
				row = it.old
				row[mcScanDate] = scannedAt
				if rowSize(row) != rf.size {
					row[mcSize] = fmt.Sprintf("%d", rf.size)
					row[mcSizeMB] = fmt.Sprintf("%.2f", float64(rf.size)/(1024*1024))
					row[mcModified] = rf.modTime.Format("2006-01-02 15:04:05")
					row[mcCapture] = capture
					row[mcPartial], row[mcFull] = partial, full
					mstats.Updated++
				} else if row[mcPartial] != partial || row[mcFull] != full {
					row[mcPartial], row[mcFull] = partial, full
					mstats.Updated++
				}
			} else {
				rel, _ := filepath.Rel(relBase, rf.path)
				row = []string{
					filepath.Base(rf.path),
					rel,
					fmt.Sprintf("%d", rf.size),
					fmt.Sprintf("%.2f", float64(rf.size)/(1024*1024)),
					rf.modTime.Format("2006-01-02 15:04:05"),
					capture,
					"", "", // camera make/model (not yet extracted)
					partial,
					full,
					strings.ToLower(filepath.Ext(rf.path)),
					scannedAt,
					relBase,
					machineName,
				}
				mstats.New++
			}
			if writeErr == nil {
				writeErr = jw.Write(row)
			}
			stats.TotalBytes += rf.size
		}
		processed += len(batch)
		jw.Flush()
		if writeErr == nil {
			writeErr = jw.Error()
		}
		label, _ := curDir.Load().(string)
		if len(label) > 50 {
			label = "..." + label[len(label)-47:]
		}
		fmt.Fprintf(os.Stderr, "\r  %-100s", fmt.Sprintf("%s found, %s processed...  %s", formatCount(int(foundCount.Load())), formatCount(processed), label))
	}
	fmt.Fprintf(os.Stderr, "\r  %-100s\n", fmt.Sprintf("%s files found, %s processed", formatCount(processed), formatCount(processed)))

	old.drain(skipOld)
	jw.Flush()
	if writeErr == nil {
		writeErr = jw.Error()
	}
	if cerr := jf.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		return stats, mstats, fmt.Errorf("write %s: %w (the rows saved so far are kept for the next scan)", journalPath, writeErr)
	}
	if old.err != nil {
		return stats, mstats, fmt.Errorf("read existing manifest %s: %w", manifestFile, old.err)
	}
	stats.Found = processed
	stats.Symlinks = walkStats.Symlinks
	stats.Unreadable = walkStats.Unreadable
	stats.FirstBad = walkStats.FirstBad

	// Decide on pruning now that every old row has been seen or passed over.
	prune = pruneAllowed(prune, stats)
	dropUnseen := false
	_, statErr := os.Stat(relBase)
	switch {
	case !prune:
	case statErr != nil:
		// Not readable here: a remote location whose manifest is kept as it is.
		fmt.Fprintf(os.Stderr, "⚠  Not pruning remote manifest (remote location %q not accessible)\n", relBase)
		fmt.Fprintf(os.Stderr, "   Remote backups are kept as-is for safety.\n")
	case scoped > 0 && unseen*100/scoped > 50 && !forcePrune:
		// Safety: pruning that much means something is wrong (folder moved, volume unmounted).
		fmt.Fprintf(os.Stderr, "⚠  Skipping prune: would remove %s of %s entries (>50%%).\n",
			formatCount(unseen), formatCount(scoped))
		fmt.Fprintf(os.Stderr, "   Is the folder still at %s? If you really removed that many files, re-run with --force-prune.\n", relBase)
	default:
		dropUnseen = true
		mstats.Pruned = unseen
	}

	total, err := mergeIntoManifest(manifestFile, oldPath, journalPath, inScope, dropUnseen)
	if err != nil {
		return stats, mstats, err
	}
	mstats.Entries = total
	if st, err := os.Stat(manifestFile); err == nil {
		mstats.Bytes = st.Size()
	}
	os.Remove(journalPath)
	for _, lp := range leftovers {
		os.Remove(lp)
	}
	return stats, mstats, nil
}

// mergeIntoManifest writes the new manifest: every journal row, plus the old rows nobody saw
// (except those in scope when dropUnseen), in folder order, then swaps it in atomically.
func mergeIntoManifest(manifestFile, oldPath, journalPath string, inScope func(string) bool, dropUnseen bool) (int, error) {
	fmt.Fprintf(os.Stderr, "Updating manifest: %s\n", filepath.Base(manifestFile))
	if err := backupManifest(manifestFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not back up manifest: %v\n", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(manifestFile), ".manifest-tmp-*")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once renamed

	old := openRowCursor(oldPath, false)
	defer old.close()
	jr := openRowCursor(journalPath, false)
	defer jr.close()
	w := csv.NewWriter(tmp)
	w.Write(manifestColumns)

	total := 0 // rows written, so dropped (pruned) and superseded old rows are not counted
	write := func(row []string) {
		w.Write(row)
		total++
	}
	keepOld := func(row []string) {
		if !(dropUnseen && inScope(row[mcRel])) {
			write(row)
		}
	}
	for old.row != nil || jr.row != nil {
		switch {
		case jr.row == nil:
			keepOld(old.row)
			old.next()
		case old.row == nil:
			write(jr.row)
			jr.next()
		default:
			c := pathCompare(old.rel, jr.rel)
			switch {
			case c < 0:
				keepOld(old.row)
				old.next()
			case c > 0:
				write(jr.row)
				jr.next()
			default:
				write(jr.row)
				key := jr.rel
				jr.next()
				for old.row != nil && pathCompare(old.rel, key) == 0 {
					old.next()
				}
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("write manifest %s: %w", manifestFile, err)
	}
	if old.err != nil || jr.err != nil {
		tmp.Close()
		return 0, fmt.Errorf("read while merging %s: %w", manifestFile, errors.Join(old.err, jr.err))
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close manifest %s: %w", manifestFile, err)
	}
	if err := os.Rename(tmpPath, manifestFile); err != nil {
		return 0, fmt.Errorf("finalize manifest %s: %w", manifestFile, err)
	}
	return total, nil
}
