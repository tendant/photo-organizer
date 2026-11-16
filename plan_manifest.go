package main

// Checking a saved plan against manifests instead of the disks.
//
// `organize --plan` used to stat every source and every target. For a plan with thousands of
// files on a slow or busy disk that is a lot of reading just to learn what the manifests already
// say. Now the check uses the manifest of each source folder and of the library (and scans a
// folder first when it has no manifest). --check-files restores the look at the real files.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// planManifestRows returns the manifest rows covering dir, keyed by absolute path. A folder
// inside a scanned folder is covered by that folder's manifest. With none, dir is scanned.
func planManifestRows(dir string) (map[string]ManifestRow, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	load := func() (ManifestSource, bool) {
		if src, err := loadManifestForRoot(abs); err == nil {
			return src, true
		}
		machine := driveIDFor(abs) // a folder on a portable drive is recorded under the drive's ID
		if machine == "" {
			machine = resolveMachineID("")
		}
		if _, file := findParentManifest(defaultManifestRoot(), machine, abs); file != "" {
			if src, err := readManifest(file); err == nil {
				return src, true
			}
		}
		return ManifestSource{}, false
	}
	src, ok := load()
	if !ok {
		fmt.Fprintf(os.Stderr, "No manifest for %s yet; scanning it once\n", abs)
		if err := runScan([]string{abs, "--no-report"}); err != nil {
			return nil, fmt.Errorf("scan %s: %w", abs, err)
		}
		if src, ok = load(); !ok {
			return nil, fmt.Errorf("no manifest for %s after scanning", abs)
		}
	}
	fmt.Fprintf(os.Stderr, "Manifest for %s: %s files, scanned %s\n", abs, formatCount(len(src.Rows)), src.LastScanned)
	rows := make(map[string]ManifestRow, len(src.Rows))
	for _, r := range src.Rows {
		rows[rowPath(src, r)] = r
	}
	return rows, nil
}

// validatePlanFromManifests is validatePlan using manifest rows; it reads no photo or video
// folder. With apply, the files still to copy are checked on disk afterwards (they are about to
// be read anyway), so a source edited since its scan is never copied under a stale record.
func validatePlanFromManifests(pf planFile, plan *organizePlan, apply bool) (todo []plannedCopy, done int, problems []string) {
	dest := filepath.Clean(pf.Dest)
	var roots []string
	roots = append(roots, pf.Sources...)
	roots = append(roots, dest)
	all := map[string]ManifestRow{}
	for _, r := range roots {
		if r == dest {
			if _, err := os.Stat(dest); err != nil {
				continue // library not created yet: nothing in it
			}
		}
		rows, err := planManifestRows(r)
		if err != nil {
			problems = append(problems, err.Error())
			return
		}
		for k, v := range rows {
			all[k] = v
		}
	}
	seen := map[string]bool{}
	for _, c := range pf.Copies {
		dst := filepath.Clean(c.Dst)
		if !pathWithin(dst, dest) || dst == dest {
			problems = append(problems, fmt.Sprintf("target %s is outside the library", c.Dst))
			continue
		}
		if key := strings.ToLower(dst); seen[key] {
			problems = append(problems, fmt.Sprintf("target %s appears twice", c.Dst))
			continue
		} else {
			seen[key] = true
		}
		mtime := time.Unix(0, c.MTime).Format(manifestTimeLayout)

		if d, ok := all[dst]; ok {
			// A rename or copy keeps size and modified time; photos and videos also keep the hash.
			same := d.SizeBytes == c.Size && (c.Hash == "" || d.PartialHash == c.Hash)
			if same && c.Hash == "" {
				same = d.FileModified == mtime
			}
			if same {
				done++
				continue
			}
			problems = append(problems, fmt.Sprintf("target name now taken by other content: %s", c.Dst))
			continue
		}
		s, ok := all[filepath.Clean(c.Src)]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("source is gone (not in its manifest): %s", c.Src))
			continue
		case s.SizeBytes != c.Size || s.FileModified != mtime:
			problems = append(problems, fmt.Sprintf("source changed since the preview: %s", c.Src))
			continue
		}
		todo = append(todo, plannedCopy{src: c.Src, dst: dst, size: c.Size, hash: c.Hash, mtime: c.MTime})
		if rel, err := filepath.Rel(dest, dst); err == nil {
			plan.byFolder[strings.SplitN(rel, string(filepath.Separator), 2)[0]]++
		}
	}
	if apply {
		for _, c := range todo {
			info, err := os.Stat(c.src)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("source is gone: %s", c.src))
			case info.Size() != c.size || info.ModTime().UnixNano() != c.mtime:
				problems = append(problems, fmt.Sprintf("source changed since the preview: %s", c.src))
			}
		}
	}
	return
}
