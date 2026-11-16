package main

// A remembered capture date per source file. Reading a date means opening the file (EXIF, or a
// video's container), which on a spinning disk costs a seek per file and dominates a preview of
// tens of thousands of photos. The date of a file cannot change unless the file does, so it is
// remembered by path, size and modified time and re-read only when one of those changes.
// The scan manifest is not used for this: its capture_date is a coarser guess (no
// DateTimeOriginal, no container formats) and would put files in different folders.

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type dateEntry struct {
	size    int64
	mtime   int64
	zone    string // --video-tz in effect when it was computed ("" = as written)
	when    time.Time
	source  dateSource
	shifted bool
}

type dateCache struct {
	mu    sync.Mutex
	path  string
	m     map[string]dateEntry
	dirty bool
	hits  int
}

func dateCachePath() string { return filepath.Join(defaultManifestRoot(), "organize-dates.csv") }

func loadDateCache() *dateCache {
	c := &dateCache{path: dateCachePath(), m: map[string]dateEntry{}}
	f, err := os.Open(c.path)
	if err != nil {
		return c
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return c
	}
	for _, r := range rows {
		if len(r) < 7 {
			continue
		}
		size, e1 := strconv.ParseInt(r[1], 10, 64)
		mtime, e2 := strconv.ParseInt(r[2], 10, 64)
		when, e3 := time.Parse(time.RFC3339Nano, r[4])
		src, e4 := strconv.Atoi(r[5])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		c.m[r[0]] = dateEntry{size: size, mtime: mtime, zone: r[3], when: when, source: dateSource(src), shifted: r[6] == "1"}
	}
	return c
}

// lookup returns the remembered date when the file is unchanged and the zone setting is the same.
func (c *dateCache) lookup(f sourceFile, zone string) (dateEntry, bool) {
	if c == nil {
		return dateEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[f.path]
	if !ok || e.size != f.size || e.mtime != f.mtime || e.zone != zone {
		return dateEntry{}, false
	}
	c.hits++
	return e, true
}

func (c *dateCache) store(f sourceFile, zone string, when time.Time, src dateSource, shifted bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m[f.path] = dateEntry{size: f.size, mtime: f.mtime, zone: zone, when: when, source: src, shifted: shifted}
	c.dirty = true
	c.mu.Unlock()
}

// save writes the cache atomically; a failure only costs speed next time.
func (c *dateCache) save() error {
	if c == nil || !c.dirty {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".organize-dates-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := csv.NewWriter(tmp)
	for p, e := range c.m {
		shifted := "0"
		if e.shifted {
			shifted = "1"
		}
		w.Write([]string{p, strconv.FormatInt(e.size, 10), strconv.FormatInt(e.mtime, 10), e.zone,
			e.when.Format(time.RFC3339Nano), strconv.Itoa(int(e.source)), shifted})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	c.dirty = false
	return os.Rename(tmp.Name(), c.path)
}
