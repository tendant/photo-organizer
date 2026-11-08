package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tiffWithDateTime builds a minimal little-endian TIFF whose IFD0 holds a
// DateTime (0x0132) tag.
func tiffWithDateTime(stamp string) []byte {
	val := append([]byte(stamp), 0) // 20 bytes incl. NUL
	var b bytes.Buffer
	b.WriteString("II*\x00")
	binary.Write(&b, binary.LittleEndian, uint32(8)) // IFD0 offset
	binary.Write(&b, binary.LittleEndian, uint16(1)) // entries
	binary.Write(&b, binary.LittleEndian, uint16(0x0132))
	binary.Write(&b, binary.LittleEndian, uint16(2)) // ASCII
	binary.Write(&b, binary.LittleEndian, uint32(len(val)))
	binary.Write(&b, binary.LittleEndian, uint32(8+2+12+4)) // value offset
	binary.Write(&b, binary.LittleEndian, uint32(0))        // next IFD
	b.Write(val)
	return b.Bytes()
}

func box(kind string, payload []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(8+len(payload)))
	b.WriteString(kind)
	b.Write(payload)
	return b.Bytes()
}

func mp4WithCreation(t time.Time) []byte {
	var mvhd bytes.Buffer
	mvhd.Write([]byte{0, 0, 0, 0}) // version 0, flags
	binary.Write(&mvhd, binary.BigEndian, uint32(t.Unix()+secondsFrom1904To1970))
	mvhd.Write(make([]byte, 90))
	out := box("ftyp", []byte("isom\x00\x00\x00\x00isom"))
	out = append(out, box("mdat", bytes.Repeat([]byte{1}, 5000))...) // moov after mdat, as in camera files
	out = append(out, box("moov", box("mvhd", mvhd.Bytes()))...)
	return out
}

func TestCaptureDateFromContainerEXIF(t *testing.T) {
	dir := t.TempDir()
	// HEIC-like: junk, then an Exif item holding a bare TIFF block.
	heic := append(bytes.Repeat([]byte{0x11}, 3000), []byte("Exif\x00\x00")...)
	heic = append(heic, tiffWithDateTime("2021:07:04 09:08:07")...)
	p := filepath.Join(dir, "IMG_9999.HEIC")
	os.WriteFile(p, heic, 0o644)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(p, old, old)

	got, src := captureDateFor(p, nil)
	if src != dateFromEXIF || got.Format("2006-01-02 15:04:05") != "2021-07-04 09:08:07" {
		t.Errorf("HEIC date = %v from %v, want 2021-07-04 09:08:07 from EXIF", got, src)
	}
}

func TestCaptureDateFromVideoMetadata(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2023, 8, 19, 17, 30, 0, 0, time.UTC)
	p := filepath.Join(dir, "clip.mov")
	os.WriteFile(p, mp4WithCreation(want), 0o644)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(p, old, old)

	got, src := captureDateFor(p, nil)
	if src != dateFromVideo || !got.Equal(want) {
		t.Errorf("video date = %v from %v, want %v from video metadata", got, src, want)
	}
}

func TestCaptureDateVideoWithUnsetCreationFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "20240102_clip.mp4")
	os.WriteFile(p, mp4WithCreation(time.Unix(-secondsFrom1904To1970, 0)), 0o644) // creation = 0
	got, src := captureDateFor(p, nil)
	if src != dateFromFilename || got.Format("2006-01-02") != "2024-01-02" {
		t.Errorf("got %v from %v, want filename date", got, src)
	}
}

func TestCaptureDateFallbackOrder(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(dir, "IMG_20240315_101010.jpg")
	os.WriteFile(named, []byte("not really a jpeg"), 0o644)
	if got, src := captureDateFor(named, nil); src != dateFromFilename || got.Format("2006-01-02") != "2024-03-15" {
		t.Errorf("named: %v from %v", got, src)
	}

	plain := filepath.Join(dir, "holiday.jpg")
	os.WriteFile(plain, []byte("x"), 0o644)
	mt := time.Date(2019, 5, 6, 7, 8, 9, 0, time.Local)
	os.Chtimes(plain, mt, mt)
	if got, src := captureDateFor(plain, nil); src != dateFromModTime || got.Format("2006-01-02") != "2019-05-06" {
		t.Errorf("plain: %v from %v", got, src)
	}
}

func TestFilenameDateIgnoresSequenceNumbers(t *testing.T) {
	good := map[string]string{
		"IMG_20240315_101010.jpg": "2024-03-15",
		"DJI_20231201123456.mp4":  "2023-12-01",
		"trip 2022-07-09.jpg":     "2022-07-09",
		"20190102.png":            "2019-01-02",
	}
	for name, want := range good {
		got, ok := filenameDate(name)
		if !ok || got.Format("2006-01-02") != want {
			t.Errorf("filenameDate(%q) = %v, %v; want %s", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"IMG_120240315999.jpg", // digits run on: a counter, not a date
		"scan_00012345.jpg",    // not a valid date
		"19700101.jpg",         // before any plausible camera
		"photo_20991231.jpg",   // future
		"DSC_123456789012.jpg", // long id
	} {
		if got, ok := filenameDate(name); ok {
			t.Errorf("filenameDate(%q) = %v, want no date", name, got)
		}
	}
}

func TestOrganizeSummaryShowsDateSourcesAndWarnings(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "src")
	lib := filepath.Join(home, "Library")
	writeFile(t, filepath.Join(src, "IMG_20240315_101010.jpg"), "named")
	writeFile(t, filepath.Join(src, "mystery.jpg"), "no date anywhere")
	// Twelve distinct photos dated New Year's Day.
	for i := 0; i < 12; i++ {
		p := filepath.Join(src, "ny", strings.Repeat("a", i+1)+".jpg")
		writeFile(t, p, "ny"+strings.Repeat("x", i))
		os.WriteFile(p, append([]byte("junk"), tiffWithDateTime("2022:01:01 12:00:00")...), 0o644)
		// make content distinct
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Repeat("z", i+1))
		f.Close()
	}
	out, code := runCLIWithHome(t, home, "organize", src, "--dest", lib, "--no-progress")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Dates taken from:", "filename 1", "file modified time 1", "placed by file modified time", "January 1st", "2022-01-01 (12 files)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCaptureDateVideoTimeZone(t *testing.T) {
	dir := t.TempDir()
	// 02:30 UTC on the 20th is the evening of the 19th in New York.
	p := filepath.Join(dir, "clip.mov")
	os.WriteFile(p, mp4WithCreation(time.Date(2023, 8, 20, 2, 30, 0, 0, time.UTC)), 0o644)

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	got, src := captureDateFor(p, ny)
	if src != dateFromVideo || got.Format("2006-01-02 15:04") != "2023-08-19 22:30" {
		t.Errorf("shifted = %v (%v), want 2023-08-19 22:30", got, src)
	}
	if got, _ := captureDateFor(p, nil); got.Format("2006-01-02") != "2023-08-20" {
		t.Errorf("nil zone must keep the date as written, got %v", got)
	}
}

func TestCaptureDateVideoTimeZoneKeepsPlausibleDate(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	// Plausible as written, but 1989-12-31 in New York: keep the written time
	// rather than filing the clip under an implausible year.
	p := filepath.Join(t.TempDir(), "clip.mov")
	os.WriteFile(p, mp4WithCreation(time.Date(1990, 1, 1, 0, 30, 0, 0, time.UTC)), 0o644)
	got, src := captureDateFor(p, ny)
	if src != dateFromVideo || got.Format("2006-01-02") != "1990-01-01" {
		t.Errorf("got %v (%v), want the as-written 1990-01-01", got, src)
	}
}
