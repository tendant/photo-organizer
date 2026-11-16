package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// srcPruneFixture builds source/, library/, backup/ and fakes distinct devices.
type srcPruneFixture struct {
	src, lib, bak, hold string
}

func newSrcPruneFixture(t *testing.T) srcPruneFixture {
	t.Helper()
	root := t.TempDir()
	f := srcPruneFixture{
		src: filepath.Join(root, "src"), lib: filepath.Join(root, "lib"),
		bak: filepath.Join(root, "bak"), hold: filepath.Join(root, "hold"),
	}
	for _, d := range []string{f.src, f.lib, f.bak} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := deviceOf
	deviceOf = func(p string) (uint64, bool) {
		if pathWithin(p, filepath.Join(root, "bak")) {
			return 2, true
		}
		return 1, true
	}
	t.Cleanup(func() { deviceOf = old })
	return f
}

func (f srcPruneFixture) args(extra ...string) []string {
	return append([]string{f.src, "--library", f.lib, "--backup", f.bak, "--no-progress", "--verify", "bytes"}, extra...)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneMovesOnlyFilesVerifiedInLibraryAndBackup(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.src, "a/libonly.jpg"), "photo-libonly")
	writeFile(t, filepath.Join(f.src, "a/none.jpg"), "photo-none")
	writeFile(t, filepath.Join(f.lib, "2024/2024-01-01/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.lib, "2024/2024-01-01/libonly.jpg"), "photo-libonly")
	writeFile(t, filepath.Join(f.bak, "x/both.jpg"), "photo-both")
	writeFile(t, filepath.Join(f.bak, "x/other.jpg"), "unrelated")

	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "a/both.jpg")) {
		t.Error("verified file should have left the source")
	}
	if !exists(filepath.Join(f.src, "a/libonly.jpg")) || !exists(filepath.Join(f.src, "a/none.jpg")) {
		t.Error("unverified files must stay")
	}
	moved := holdingPath(f.hold, f.src, filepath.Join(f.src, "a/both.jpg"))
	if b, _ := os.ReadFile(moved); string(b) != "photo-both" {
		t.Errorf("moved file missing or changed at %s", moved)
	}
	// library and backup copies untouched
	if !exists(filepath.Join(f.lib, "2024/2024-01-01/both.jpg")) || !exists(filepath.Join(f.bak, "x/both.jpg")) {
		t.Error("library/backup must not change")
	}
}

func TestPrunePreviewChangesNothing(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "same")
	before := snapshotTree(t, filepath.Dir(f.src))
	if err := runPruneSources(f.args()); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, filepath.Dir(f.src))
	if len(before) != len(after) {
		t.Fatalf("preview changed the tree: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed", k)
		}
	}
}

func TestPruneSameSizeDifferentBytesIsNotACopy(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "AAAAAAAA")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "AAAAAAAB") // same size, differs at the end
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "AAAAAAAA")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("a file whose library copy differs must stay")
	}
}

func TestPruneNeedsABackupCopyNotJustLibrary(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "other.jpg"), "different")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("file with no backup copy must stay")
	}
}

func TestPruneKeepsSidecarsAndOtherType(t *testing.T) {
	f := newSrcPruneFixture(t)
	for _, root := range []string{f.src, f.lib, f.bak} {
		writeFile(t, filepath.Join(root, "p.jpg"), "photo")
		writeFile(t, filepath.Join(root, "p.xmp"), "edits")
		writeFile(t, filepath.Join(root, "v.mp4"), "video")
	}
	if err := runPruneSources(f.args("--type", "photo", "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "p.jpg")) {
		t.Error("photo should be moved")
	}
	if !exists(filepath.Join(f.src, "p.xmp")) {
		t.Error("sidecar must stay")
	}
	if !exists(filepath.Join(f.src, "v.mp4")) {
		t.Error("video must stay in a --type photo run")
	}
}

func TestPruneRefusesBackupOnLibraryDevice(t *testing.T) {
	f := newSrcPruneFixture(t)
	deviceOf = func(string) (uint64, bool) { return 1, true }
	writeFile(t, filepath.Join(f.src, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "x")
	err := runPruneSources(f.args())
	if err == nil || !strings.Contains(err.Error(), "same device") {
		t.Fatalf("want same-device refusal, got %v", err)
	}
}

func TestPruneRefusesOverlappingFolders(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "x")
	// library inside the source: its files would count as their own copy
	insideLib := filepath.Join(f.src, "library")
	os.MkdirAll(insideLib, 0o755)
	writeFile(t, filepath.Join(insideLib, "a.jpg"), "x")
	err := runPruneSources([]string{f.src, "--library", insideLib, "--backup", f.bak, "--no-progress"})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("want overlap refusal, got %v", err)
	}
	// holding inside the library
	err = runPruneSources(f.args("--holding", filepath.Join(f.lib, "h"), "--apply"))
	if err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("want holding refusal, got %v", err)
	}
}

func TestPruneRefusesEmptyBackup(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "x")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "x")
	if err := runPruneSources(f.args("--holding", f.hold, "--apply")); err == nil {
		t.Fatal("an empty backup must stop the run")
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("source must be untouched")
	}
}

func TestPruneArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"--library", "l", "--backup", "b"}, "source"},
		{"no library", []string{"s", "--backup", "b"}, "--library"},
		{"no backup", []string{"s", "--library", "l"}, "--backup"},
		{"apply without holding", []string{"s", "--library", "l", "--backup", "b", "--apply"}, "--holding"},
		{"bad type", []string{"s", "--library", "l", "--backup", "b", "--type", "x"}, "unknown type"},
		{"bad option", []string{"s", "--library", "l", "--backup", "b", "--nope"}, "unknown option"},
	} {
		_, err := parsePruneArgs(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestPruneWritesReportAndKeepsExistingHoldingFiles(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "a.jpg"), "same")
	rep := filepath.Join(filepath.Dir(f.src), "report.csv")
	// an earlier file already sits at the holding destination: do not overwrite it
	dst := holdingPath(f.hold, f.src, filepath.Join(f.src, "a.jpg"))
	writeFile(t, dst, "older")
	err := runPruneSources(f.args("--report", rep, "--holding", f.hold, "--apply"))
	if err == nil {
		t.Fatal("a refused move should be reported as a failure")
	}
	if b, _ := os.ReadFile(dst); string(b) != "older" {
		t.Error("existing holding file was overwritten")
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("source must stay when the move is refused")
	}
	rows := readCSV(t, rep)
	if len(rows) != 2 || rows[1][2] != "safe" {
		t.Errorf("unexpected report: %v", rows)
	}
}

// previewReport runs a preview that writes a report and returns its path.
func previewReport(t *testing.T, f srcPruneFixture) string {
	t.Helper()
	rep := filepath.Join(filepath.Dir(f.src), "report.csv")
	if err := runPruneSources(f.args("--report", rep)); err != nil {
		t.Fatal(err)
	}
	return rep
}

func seedSame(t *testing.T, f srcPruneFixture, names ...string) {
	t.Helper()
	for _, n := range names {
		writeFile(t, filepath.Join(f.src, n), "content-"+n)
		writeFile(t, filepath.Join(f.lib, n), "content-"+n)
		writeFile(t, filepath.Join(f.bak, n), "content-"+n)
	}
}

func TestPruneFromReportMovesWhatThePreviewVerified(t *testing.T) {
	f := newSrcPruneFixture(t)
	seedSame(t, f, "a.jpg", "b.jpg")
	writeFile(t, filepath.Join(f.src, "keep.jpg"), "only here")
	rep := previewReport(t, f)
	if err := runPruneSources(f.args("--from-report", rep, "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jpg", "b.jpg"} {
		if exists(filepath.Join(f.src, n)) {
			t.Errorf("%s should have moved", n)
		}
		if !exists(holdingPath(f.hold, f.src, filepath.Join(f.src, n))) {
			t.Errorf("%s missing from holding", n)
		}
	}
	if !exists(filepath.Join(f.src, "keep.jpg")) {
		t.Error("unverified file must stay")
	}
}

func TestPruneFromReportDoesNotReadFileContents(t *testing.T) {
	f := newSrcPruneFixture(t)
	seedSame(t, f, "a.jpg")
	rep := previewReport(t, f)
	// Same size, same (old) mtime, different bytes: only a re-read could notice.
	// A cheap re-check must therefore trust the report - this documents the trade-off.
	old := mustModTime(t, filepath.Join(f.lib, "a.jpg"))
	writeFile(t, filepath.Join(f.lib, "a.jpg"), "content-X.jpg") // same length as "content-a.jpg"
	os.Chtimes(filepath.Join(f.lib, "a.jpg"), old, old)
	if err := runPruneSources(f.args("--from-report", rep, "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("from-report trusts size+mtime and should have moved the file")
	}
}

func TestPruneFromReportRejectsFilesChangedSinceThePreview(t *testing.T) {
	f := newSrcPruneFixture(t)
	seedSame(t, f, "src-edited.jpg", "lib-edited.jpg", "bak-gone.jpg", "lib-gone.jpg", "fine.jpg")
	rep := previewReport(t, f)
	past := mustModTime(t, rep).Add(-10 * 60 * 1e9)
	os.Chtimes(rep, past, past) // pretend the report is older than the edits below
	future := past.Add(5 * 60 * 1e9)
	os.WriteFile(filepath.Join(f.src, "src-edited.jpg"), []byte("content-src-edited.jpg"), 0o644)
	os.Chtimes(filepath.Join(f.src, "src-edited.jpg"), future, future)
	os.Chtimes(filepath.Join(f.lib, "lib-edited.jpg"), future, future)
	os.Chtimes(filepath.Join(f.src, "fine.jpg"), past.Add(-1e9), past.Add(-1e9))
	os.Chtimes(filepath.Join(f.lib, "fine.jpg"), past.Add(-1e9), past.Add(-1e9))
	os.Chtimes(filepath.Join(f.bak, "fine.jpg"), past.Add(-1e9), past.Add(-1e9))
	os.Remove(filepath.Join(f.bak, "bak-gone.jpg"))
	os.Remove(filepath.Join(f.lib, "lib-gone.jpg"))
	writeFile(t, filepath.Join(f.bak, "padding.jpg"), "keeps the backup non-empty")

	err := runPruneSources(f.args("--from-report", rep, "--holding", f.hold, "--apply"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"src-edited.jpg", "lib-edited.jpg", "bak-gone.jpg", "lib-gone.jpg"} {
		if !exists(filepath.Join(f.src, n)) {
			t.Errorf("%s changed since the report and must stay", n)
		}
	}
	if exists(filepath.Join(f.src, "fine.jpg")) {
		t.Error("untouched file should have moved")
	}
}

func TestPruneFromReportIgnoresCopiesOutsideLibraryAndBackup(t *testing.T) {
	f := newSrcPruneFixture(t)
	writeFile(t, filepath.Join(f.src, "a.jpg"), "same")
	writeFile(t, filepath.Join(f.bak, "pad.jpg"), "pad")
	writeFile(t, filepath.Join(f.lib, "pad.jpg"), "pad2")
	elsewhere := filepath.Join(filepath.Dir(f.src), "elsewhere.jpg")
	writeFile(t, elsewhere, "same")
	rep := filepath.Join(filepath.Dir(f.src), "forged.csv")
	writeFile(t, rep, "source_path,size_bytes,verdict,library_copy,backup_copy\n"+
		filepath.Join(f.src, "a.jpg")+",4,safe,"+elsewhere+","+elsewhere+"\n")
	if err := runPruneSources(f.args("--from-report", rep, "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("copies outside the named library/backup must not count")
	}
}

func TestPruneFromReportArgsAndBadFiles(t *testing.T) {
	if _, err := parsePruneArgs([]string{"s", "--library", "l", "--backup", "b", "--from-report", "r", "--type", "photo"}); err == nil {
		t.Error("--type with --from-report should be rejected")
	}
	f := newSrcPruneFixture(t)
	seedSame(t, f, "a.jpg")
	notReport := filepath.Join(filepath.Dir(f.src), "x.csv")
	writeFile(t, notReport, "a,b,c\n1,2,3\n")
	if err := runPruneSources(f.args("--from-report", notReport)); err == nil {
		t.Error("a file that is not a report must be refused")
	}
	if err := runPruneSources(f.args("--from-report", filepath.Join(f.src, "nope.csv"))); err == nil {
		t.Error("a missing report must be refused")
	}
}

func mustModTime(t *testing.T, p string) time.Time {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}

// ---- manifest-hash mode -------------------------------------------------------------

func scanQuiet(t *testing.T, dir string) {
	t.Helper()
	if err := runScan([]string{dir, "--no-report"}); err != nil {
		t.Fatal(err)
	}
}

func (f srcPruneFixture) mArgs(extra ...string) []string {
	return append([]string{f.src, "--library", f.lib, "--backup", f.bak, "--no-progress"}, extra...)
}

func manifestFixture(t *testing.T) srcPruneFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return newSrcPruneFixture(t)
}

func scanAll(t *testing.T, f srcPruneFixture) {
	t.Helper()
	scanQuiet(t, f.src)
	scanQuiet(t, f.lib)
	scanQuiet(t, f.bak)
}

func TestPruneManifestModeMatchesByHashWithoutReadingContent(t *testing.T) {
	f := manifestFixture(t)
	big := strings.Repeat("x", 200<<10) // bigger than the two 32 KB hash windows
	writeFile(t, filepath.Join(f.src, "a.jpg"), big)
	writeFile(t, filepath.Join(f.lib, "2024/a.jpg"), big)
	writeFile(t, filepath.Join(f.bak, "a.jpg"), big)
	writeFile(t, filepath.Join(f.src, "other.jpg"), "only here")
	scanAll(t, f)

	// Change one byte in the middle of the library copy, keep size and mtime.
	lp := filepath.Join(f.lib, "2024/a.jpg")
	old := mustModTime(t, lp)
	data := []byte(big)
	data[100<<10] = 'Y'
	os.WriteFile(lp, data, 0o644)
	os.Chtimes(lp, old, old)

	// bytes mode sees the difference ...
	if err := runPruneSources(f.mArgs("--verify", "bytes", "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Fatal("--verify bytes must keep a file whose library copy differs")
	}
	// ... manifest mode does not (documented trade-off), and reads no photo data.
	if err := runPruneSources(f.mArgs("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("manifest mode should have matched a.jpg by hash")
	}
	if !exists(filepath.Join(f.src, "other.jpg")) {
		t.Error("a file with no copy must stay")
	}
}

func TestPruneManifestModeRequiresManifests(t *testing.T) {
	f := manifestFixture(t)
	seedSame(t, f, "a.jpg")
	scanQuiet(t, f.src)
	scanQuiet(t, f.lib) // no backup manifest
	err := runPruneSources(f.mArgs())
	if err == nil || !strings.Contains(err.Error(), "scan") {
		t.Fatalf("want a 'run scan first' error, got %v", err)
	}
	if !exists(filepath.Join(f.src, "a.jpg")) {
		t.Error("source must be untouched")
	}
}

func TestPruneManifestModeDistrustsFilesChangedSinceTheScan(t *testing.T) {
	f := manifestFixture(t)
	seedSame(t, f, "src-edited.jpg", "lib-edited.jpg", "bak-gone.jpg", "lib-gone.jpg", "fine.jpg")
	scanAll(t, f)
	later := time.Now().Add(2 * time.Hour)
	os.Chtimes(filepath.Join(f.src, "src-edited.jpg"), later, later)
	os.Chtimes(filepath.Join(f.lib, "lib-edited.jpg"), later, later)
	os.Remove(filepath.Join(f.bak, "bak-gone.jpg"))
	os.Remove(filepath.Join(f.lib, "lib-gone.jpg"))
	writeFile(t, filepath.Join(f.src, "new.jpg"), "never scanned") // not in the manifest at all

	if err := runPruneSources(f.mArgs("--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"src-edited.jpg", "lib-edited.jpg", "bak-gone.jpg", "lib-gone.jpg", "new.jpg"} {
		if !exists(filepath.Join(f.src, n)) {
			t.Errorf("%s is not backed by a fresh manifest row and must stay", n)
		}
	}
	if exists(filepath.Join(f.src, "fine.jpg")) {
		t.Error("an untouched, matching file should have moved")
	}
}

func TestPruneManifestModeFeedsFromReport(t *testing.T) {
	f := manifestFixture(t)
	seedSame(t, f, "a.jpg", "b.jpg")
	scanAll(t, f)
	rep := filepath.Join(filepath.Dir(f.src), "m-report.csv")
	if err := runPruneSources(f.mArgs("--report", rep)); err != nil {
		t.Fatal(err)
	}
	if err := runPruneSources(f.mArgs("--from-report", rep, "--holding", f.hold, "--apply")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.src, "a.jpg")) || exists(filepath.Join(f.src, "b.jpg")) {
		t.Error("report from manifest mode should be appliable with --from-report")
	}
}

func TestRowsMatchUsesFullHashWhenBothHaveOne(t *testing.T) {
	a := ManifestRow{SizeBytes: 10, PartialHash: "p"}
	b := ManifestRow{SizeBytes: 10, PartialHash: "p"}
	if !rowsMatch(a, b) {
		t.Error("same size and partial hash should match")
	}
	a.FullHash, b.FullHash = "f1", "f2"
	if rowsMatch(a, b) {
		t.Error("different full hashes must not match")
	}
	b.FullHash = ""
	if !rowsMatch(a, b) {
		t.Error("a missing full hash falls back to the partial hash")
	}
	if rowsMatch(a, ManifestRow{SizeBytes: 11, PartialHash: "p"}) || rowsMatch(ManifestRow{SizeBytes: 10}, ManifestRow{SizeBytes: 10}) {
		t.Error("different size or empty hash must not match")
	}
}

func TestPruneVerifyArg(t *testing.T) {
	o, err := parsePruneArgs([]string{"s", "--library", "l", "--backup", "b"})
	if err != nil || o.byBytes {
		t.Fatalf("default should be manifest mode: %v %v", o.byBytes, err)
	}
	o, _ = parsePruneArgs([]string{"s", "--library", "l", "--backup", "b", "--verify", "bytes"})
	if !o.byBytes {
		t.Error("--verify bytes not parsed")
	}
	if _, err := parsePruneArgs([]string{"s", "--library", "l", "--backup", "b", "--verify", "nope"}); err == nil {
		t.Error("unknown verify mode should be rejected")
	}
}
