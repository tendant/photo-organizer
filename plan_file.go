package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Saved plans: preview once, then copy exactly what was previewed
// =============================================================================
//
//	organize <folders> --dest LIB --save-plan plan.json     (preview; writes only plan.json)
//	organize --plan plan.json --apply                       (copies exactly that)
//
// Applying re-checks the world first. If a source file changed (size or
// modified time) or a target name has been taken by something else, nothing is
// copied and the user is told to preview again. Targets that already hold the
// planned content (an earlier, interrupted apply) are skipped, so re-running the
// same plan command resumes.

const planFileVersion = 1

type planFile struct {
	Version int        `json:"version"`
	Created string     `json:"created"`
	Dest    string     `json:"dest"`
	Sources []string   `json:"sources"`
	Copies  []planCopy `json:"copies"`
}

type planCopy struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Size  int64  `json:"size"`
	Hash  string `json:"hash,omitempty"` // scan hash for photos/videos; empty for sidecars
	MTime int64  `json:"mtime_ns"`
}

// savePlanFile writes the plan atomically. It refuses a location inside a
// source folder (sources are never written to) or inside the library (a preview
// must not create it).
func savePlanFile(path string, sources []string, dest string, p *organizePlan) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid plan path %q", path)
	}
	real := resolveThroughSymlinks(abs)
	for _, s := range append([]string{dest}, sources...) {
		if pathWithin(real, resolveThroughSymlinks(s)) {
			return fmt.Errorf("plan file %s is inside %s; save it somewhere else", path, s)
		}
	}

	pf := planFile{Version: planFileVersion, Created: time.Now().Format(time.RFC3339), Dest: dest, Sources: sources}
	for _, c := range p.copies {
		pf.Copies = append(pf.Copies, planCopy{Src: c.src, Dst: c.dst, Size: c.size, Hash: c.hash, MTime: c.mtime})
	}
	data, err := json.MarshalIndent(pf, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".plan-*")
	if err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write plan: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// runSavedPlan loads a plan, checks it is still valid, shows it, and with
// --apply executes it.
func runSavedPlan(opts organizeOptions) error {
	if len(opts.sources) > 0 || opts.savePlan != "" {
		fmt.Fprintf(os.Stderr, "Error: --plan replays a saved plan; do not combine it with folders or --save-plan\n")
		return errFailed
	}
	if opts.videoTZSet {
		fmt.Fprintf(os.Stderr, "Error: --video-tz has no effect with --plan; the saved plan already fixes every target folder. Pass it when you create the plan with --save-plan\n")
		return errFailed
	}
	data, err := os.ReadFile(opts.planFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot read plan: %v\n", err)
		return errFailed
	}
	var pf planFile
	if err := json.Unmarshal(data, &pf); err != nil || pf.Version != planFileVersion || pf.Dest == "" {
		fmt.Fprintf(os.Stderr, "Error: %s is not a plan written by this version of photo-organizer\n", opts.planFile)
		return errFailed
	}
	dest := pf.Dest
	if opts.dest != "" {
		given, err := filepath.Abs(opts.dest)
		if err != nil || filepath.Clean(given) != filepath.Clean(dest) {
			fmt.Fprintf(os.Stderr, "Error: the plan targets %s, not %s\n", dest, opts.dest)
			return errFailed
		}
	}

	realDest := resolveThroughSymlinks(dest)
	for _, s := range pf.Sources {
		realSrc := resolveThroughSymlinks(s)
		if pathWithin(realDest, realSrc) || pathWithin(realSrc, realDest) {
			fmt.Fprintf(os.Stderr, "Error: %s and the library %s overlap\n", s, dest)
			return errFailed
		}
	}

	plan := &organizePlan{byFolder: map[string]int{}}
	todo, done, problems := validatePlan(pf, plan)
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "\nThe plan is out of date; nothing was copied. %d problem(s):\n", len(problems))
		for i, pr := range problems {
			if i == 8 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(problems)-8)
				break
			}
			fmt.Fprintf(os.Stderr, "  %s\n", pr)
		}
		fmt.Fprintf(os.Stderr, "Preview again with --save-plan to make a fresh plan.\n")
		return errFailed
	}

	plan.copies = todo
	for _, c := range todo {
		plan.copyBytes += c.size
	}
	plan.freeBytes, plan.freeKnown = freeSpaceFor(dest)

	fmt.Fprintf(os.Stderr, "\nPlan from %s (saved %s)\n", opts.planFile, pf.Created)
	fmt.Fprintf(os.Stderr, "Library: %s\n", dest)
	if done > 0 {
		fmt.Fprintf(os.Stderr, "%s files from the plan are already in place (an earlier run)\n", formatCount(done))
	}
	verb := "Would copy"
	if opts.apply {
		verb = "Will copy"
	}
	fmt.Fprintf(os.Stderr, "%s %s files (%s)\n", verb, formatCount(len(todo)), formatSize(plan.copyBytes))
	if len(plan.byFolder) > 0 {
		folders := make([]string, 0, len(plan.byFolder))
		for f := range plan.byFolder {
			folders = append(folders, f)
		}
		sort.Strings(folders)
		fmt.Fprintf(os.Stderr, "By year:\n")
		for _, f := range folders {
			fmt.Fprintf(os.Stderr, "  %-8s %s files\n", f, formatCount(plan.byFolder[f]))
		}
	}
	if plan.freeKnown {
		if plan.lacksSpace() {
			fmt.Fprintf(os.Stderr, "⚠  Not enough free space: need %s, only %s available\n",
				formatSize(plan.copyBytes), formatSize(int64(plan.freeBytes)))
		} else {
			fmt.Fprintf(os.Stderr, "Free space at the library: %s\n", formatSize(int64(plan.freeBytes)))
		}
	}

	if !opts.apply {
		if len(todo) > 0 {
			fmt.Fprintf(os.Stderr, "\nNothing was written. Add --apply to copy.\n")
		}
		return nil
	}
	if plan.lacksSpace() {
		fmt.Fprintf(os.Stderr, "\nError: not enough free space for the library; nothing was copied.\n")
		return errFailed
	}
	if len(todo) == 0 {
		return nil
	}
	return finishApply(plan, dest, progressEnabled(opts.progress))
}

// validatePlan checks every planned copy against the current state of the disk.
// It returns the copies still to do, how many are already in place, and any
// reasons the plan can no longer be trusted. It only reads.
func validatePlan(pf planFile, plan *organizePlan) (todo []plannedCopy, done int, problems []string) {
	dest := filepath.Clean(pf.Dest)
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

		info, err := os.Stat(c.Src)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("source is gone: %s", c.Src))
			continue
		case info.Size() != c.Size || info.ModTime().UnixNano() != c.MTime:
			problems = append(problems, fmt.Sprintf("source changed since the preview: %s", c.Src))
			continue
		}

		if _, err := os.Lstat(dst); err == nil {
			if alreadyCopied(c, dst) {
				done++
				continue
			}
			problems = append(problems, fmt.Sprintf("target name now taken by other content: %s", c.Dst))
			continue
		}

		todo = append(todo, plannedCopy{src: c.Src, dst: dst, size: c.Size, hash: c.Hash, mtime: c.MTime})
		if rel, err := filepath.Rel(dest, dst); err == nil {
			plan.byFolder[strings.SplitN(rel, string(filepath.Separator), 2)[0]]++
		}
	}
	return
}

// alreadyCopied reports whether dst already holds the planned content.
func alreadyCopied(c planCopy, dst string) bool {
	info, err := os.Stat(dst)
	if err != nil || info.Size() != c.Size {
		return false
	}
	if c.Hash != "" {
		h, _ := processFile(dst)
		return h == c.Hash
	}
	return sameContent(c.Src, dst)
}
