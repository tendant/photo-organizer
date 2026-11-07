package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// =============================================================================
// Fast, safe duplicate confirmation for organize
// =============================================================================
//
// A file is only called a duplicate after its content matches a library file
// beyond the scan's size + 64KB hash. That check used to read both files in full
// on every run. It now:
//
//   - reads each file at most once (digests are memoised),
//   - remembers library digests between runs in <library>/_organize/index.csv,
//     keyed by size and modified time, so the library is not re-read,
//   - hashes in parallel (--jobs),
//   - compares large files (videos) by sampling spread across the whole file
//     instead of reading gigabytes, unless --verify full is given.

type verifyMode int

const (
	verifyAuto  verifyMode = iota // full for files up to autoFullLimit, sampled above
	verifyFull                    // every byte of every candidate
	verifyQuick                   // sampled for anything over sampleBudget
)

const (
	autoFullLimit = 64 << 20
	sampleChunk   = 64 << 10
	sampleChunks  = 16
	sampleBudget  = sampleChunk * sampleChunks // 1 MiB; at or below this a "sample" is the whole file
	cacheName     = "index.csv"
)

// libEntry is what is known about one file's content.
type libEntry struct {
	size    int64
	mtime   int64  // UnixNano
	partial string // the scan's size+first/last 32KB hash
	full    string // SHA-256 of the whole file, once computed
	sampled string // SHA-256 of spread samples, once computed
}

type hashStore struct {
	mu    sync.Mutex
	mode  verifyMode
	jobs  int
	dirty bool                 // library digests were computed that the cache file does not have
	lib   map[string]*libEntry // library files by absolute path
	memo  map[string]*libEntry // source files by absolute path (this run only)
}

func newHashStore(mode verifyMode, jobs int) *hashStore {
	if jobs < 1 {
		jobs = 1
	}
	return &hashStore{mode: mode, jobs: jobs, lib: map[string]*libEntry{}, memo: map[string]*libEntry{}}
}

// wantsFull reports whether files of this size are compared byte-for-byte.
func (h *hashStore) wantsFull(size int64) bool {
	switch h.mode {
	case verifyFull:
		return true
	case verifyQuick:
		return size <= sampleBudget
	}
	return size <= autoFullLimit
}

func (h *hashStore) entryLocked(path string) *libEntry {
	if e, ok := h.lib[path]; ok {
		return e
	}
	e, ok := h.memo[path]
	if !ok {
		e = &libEntry{}
		h.memo[path] = e
	}
	return e
}

// digest returns the comparison digest for path (empty if unreadable). Two
// files of the same size are the same content iff their digests are equal.
func (h *hashStore) digest(path string, size int64) string {
	full := h.wantsFull(size)

	h.mu.Lock()
	e := h.entryLocked(path)
	v := e.sampled
	if full {
		v = e.full
	}
	h.mu.Unlock()
	if v != "" {
		return v
	}

	if full {
		v = fullSHA(path)
	} else {
		v = sampledSHA(path, size)
	}
	if v == "" {
		return ""
	}
	h.mu.Lock()
	e = h.entryLocked(path)
	if full {
		e.full = v
	} else {
		e.sampled = v
	}
	if _, inLibrary := h.lib[path]; inLibrary {
		h.dirty = true
	}
	h.mu.Unlock()
	return v
}

// same reports whether two files of the given size have the same content.
func (h *hashStore) same(a, b string, size int64) bool {
	if a == b {
		return true
	}
	da := h.digest(a, size)
	return da != "" && da == h.digest(b, size)
}

func fullSHA(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.CopyBuffer(sum, f, make([]byte, 1<<20)); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// sampledSHA hashes the size plus sampleChunks pieces spread evenly from the
// first byte to the last. Files no bigger than the sample budget are hashed whole.
func sampledSHA(path string, size int64) string {
	if size <= sampleBudget {
		return fullSHA(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	fmt.Fprintf(sum, "%d:", size)
	buf := make([]byte, sampleChunk)
	for i := 0; i < sampleChunks; i++ {
		off := int64(i) * (size - sampleChunk) / (sampleChunks - 1)
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return ""
		}
		sum.Write(buf[:n])
	}
	return "s" + hex.EncodeToString(sum.Sum(nil))
}

// warm computes digests for the given files in parallel so the sequential
// planning pass finds them ready.
func (h *hashStore) warm(files []sourceFile) {
	parallelDo(h.jobs, len(files), func(i int) { h.digest(files[i].path, files[i].size) })
}

// parallelDo runs fn(0..n-1) on up to jobs goroutines.
func parallelDo(jobs, n int, fn func(i int)) {
	if jobs <= 1 || n <= 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	if jobs > n {
		jobs = n
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

// -----------------------------------------------------------------------------
// Persistent library cache
// -----------------------------------------------------------------------------

func libraryCachePath(dest string) string { return filepath.Join(dest, organizeLogDir, cacheName) }

type libCacheRow struct {
	size, mtime            int64
	partial, full, sampled string
}

// loadLibraryCache reads the library cache. A missing or damaged file just means
// "nothing cached"; it never fails the run. Paths are stored relative to the
// library so the library can be moved.
func loadLibraryCache(dest string) map[string]libCacheRow {
	rows := map[string]libCacheRow{}
	f, err := os.Open(libraryCachePath(dest))
	if err != nil {
		return rows
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return map[string]libCacheRow{}
		}
		if first {
			first = false
			if len(rec) == 0 || rec[0] != "path" {
				return map[string]libCacheRow{}
			}
			continue
		}
		if len(rec) < 6 {
			continue
		}
		size, e1 := strconv.ParseInt(rec[1], 10, 64)
		mtime, e2 := strconv.ParseInt(rec[2], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		rows[filepath.Join(dest, filepath.FromSlash(rec[0]))] = libCacheRow{size, mtime, rec[3], rec[4], rec[5]}
	}
	return rows
}

// newCopy is a file just copied into the library, with what is already known
// about its content (it is byte-identical to its source).
type newCopy struct {
	dst     string
	size    int64
	mtime   int64
	partial string
	from    string // source path whose digests apply
}

// saveCache writes the digests of every library file seen plus the new copies.
// It writes a temporary file and renames it, so a crash never leaves a damaged cache.
func (h *hashStore) saveCache(dest string, added []newCopy) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	dir := filepath.Join(dest, organizeLogDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".index-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed

	w := csv.NewWriter(tmp)
	w.Write([]string{"path", "size", "mtime_ns", "partial", "full", "sampled"})
	write := func(path string, e *libEntry) {
		rel, err := filepath.Rel(dest, path)
		if err != nil || e.partial == "" {
			return
		}
		w.Write([]string{filepath.ToSlash(rel), strconv.FormatInt(e.size, 10), strconv.FormatInt(e.mtime, 10),
			e.partial, e.full, e.sampled})
	}
	for path, e := range h.lib {
		write(path, e)
	}
	for _, c := range added {
		e := &libEntry{size: c.size, mtime: c.mtime, partial: c.partial}
		if src, ok := h.memo[c.from]; ok {
			e.full, e.sampled = src.full, src.sampled
		}
		write(c.dst, e)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), libraryCachePath(dest))
}

// -----------------------------------------------------------------------------
// Library index
// -----------------------------------------------------------------------------

type pendingHash struct {
	path  string
	size  int64
	mtime int64
}

// indexLibrary lists the library and records each media file's content key.
// Files unchanged since the cache was written cost one stat, not a read.
func indexLibrary(dest string, hs *hashStore, showProgress bool) (libraryIndex, map[string]bool, []string, error) {
	idx := libraryIndex{}
	names := map[string]bool{} // lowercased library paths, for collision checks
	var staleTmp []string
	if _, err := os.Stat(dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return idx, names, nil, nil
		}
		return nil, nil, nil, err
	}

	cached := loadLibraryCache(dest)
	prog := newProgressReporter("Indexing library", unitFiles, 0, 0, showProgress)

	var order []string // media files in walk order, for a deterministic index
	var pending []pendingHash
	reused := 0
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
		if strings.HasPrefix(d.Name(), organizeTmpPrefix) {
			staleTmp = append(staleTmp, path)
			return nil
		}
		if shouldSkipFile(path) || !isMediaFile(filepath.Ext(path)) || isSidecarExt(filepath.Ext(path)) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}
		order = append(order, path)
		mtime := info.ModTime().UnixNano()
		if row, ok := cached[path]; ok && row.size == info.Size() && row.mtime == mtime && row.partial != "" {
			hs.lib[path] = &libEntry{size: row.size, mtime: row.mtime, partial: row.partial, full: row.full, sampled: row.sampled}
			reused++
			prog.Add(1, 1)
		} else {
			pending = append(pending, pendingHash{path, info.Size(), mtime})
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// Hash what the cache could not vouch for, in parallel.
	hashes := make([]string, len(pending))
	parallelDo(hs.jobs, len(pending), func(i int) {
		hashes[i], _ = processFile(pending[i].path)
		prog.Add(1, 1)
	})
	if len(pending) > 0 {
		hs.dirty = true
	}
	for i, p := range pending {
		if hashes[i] != "" {
			hs.lib[p.path] = &libEntry{size: p.size, mtime: p.mtime, partial: hashes[i]}
		}
	}

	for _, path := range order {
		if e, ok := hs.lib[path]; ok {
			k := contentKey(e.size, e.partial)
			idx[k] = append(idx[k], path)
		}
	}
	prog.Done(fmt.Sprintf("Indexed %s library files (%s from cache)", formatCount(len(order)), formatCount(reused)))
	return idx, names, staleTmp, nil
}
