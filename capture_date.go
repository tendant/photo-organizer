package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// =============================================================================
// Capture date for organize
// =============================================================================
//
// The date decides which folder a photo lands in, so it matters where it came
// from. In order of trust:
//
//	EXIF            written by the camera (JPEG, TIFF-based RAW, HEIC, CR3, ...)
//	video metadata  the creation time stored in an MP4/MOV container
//	filename        20240315_101010 / 2024-03-15 style names
//	modified time   last resort: copying or restoring a folder changes it

type dateSource int

const (
	dateFromEXIF dateSource = iota
	dateFromVideo
	dateFromFilename
	dateFromModTime
)

func (d dateSource) String() string {
	switch d {
	case dateFromEXIF:
		return "EXIF"
	case dateFromVideo:
		return "video metadata"
	case dateFromFilename:
		return "filename"
	}
	return "file modified time"
}

// containerSearchBytes bounds how much of a non-JPEG file is searched for an
// embedded EXIF block. HEIC and CR3 keep theirs near the start.
const containerSearchBytes = 1 << 20

// captureDateFor returns the best available capture date for path and where
// it came from. It only reads the file. videoLoc, when set, is the time zone a
// video's creation time is shown in (phones record UTC); nil uses it as written.
func captureDateFor(path string, videoLoc *time.Location) (time.Time, dateSource) {
	t, src, _ := captureDateShifted(path, videoLoc)
	return t, src
}

// captureDateShifted is captureDateFor that also reports whether videoLoc moved
// the video to a different calendar day than its time as written.
func captureDateShifted(path string, videoLoc *time.Location) (time.Time, dateSource, bool) {
	ext := strings.ToLower(filepath.Ext(path))

	if photoExts[ext] {
		if t, ok := exifDate(path); ok {
			return t, dateFromEXIF, false
		}
	}
	if videoExts[ext] {
		if t, ok := videoCreationTime(path); ok {
			if videoLoc != nil {
				// Keep the as-written time if the shift would push it out of the
				// plausible range (e.g. 1990-01-01 00:30 UTC -> 1989-12-31).
				if shifted := t.In(videoLoc); plausibleDate(shifted) {
					moved := shifted.Format("2006-01-02") != t.Format("2006-01-02")
					return shifted, dateFromVideo, moved
				}
			}
			return t, dateFromVideo, false
		}
	}
	if t, ok := filenameDate(filepath.Base(path)); ok {
		return t, dateFromFilename, false
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime(), dateFromModTime, false
	}
	return time.Time{}, dateFromModTime, false
}

// exifTime prefers the moment the shutter fired over the file-modified field.
func exifTime(x *exif.Exif) (original, fallback time.Time, haveOriginal, haveFallback bool) {
	if tag, err := x.Get(exif.DateTimeOriginal); err == nil {
		if s, err := tag.StringVal(); err == nil {
			if t, err := time.Parse("2006:01:02 15:04:05", strings.TrimRight(strings.TrimSpace(s), "\x00")); err == nil && plausibleDate(t) {
				return t, time.Time{}, true, false
			}
		}
	}
	if t, err := x.DateTime(); err == nil && plausibleDate(t) {
		return time.Time{}, t, false, true
	}
	return
}

func decodeExifSafely(r io.Reader) (x *exif.Exif) {
	defer func() {
		if recover() != nil {
			x = nil
		}
	}()
	x, err := exif.Decode(r)
	if err != nil {
		return nil
	}
	return x
}

// exifDate reads EXIF from the file itself (JPEG and TIFF-based RAW), and
// otherwise looks for an EXIF block embedded in the container (HEIC, CR3,
// RAF, ORF, RW2 ...).
func exifDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	if x := decodeExifSafely(f); x != nil {
		if o, fb, haveO, haveFB := exifTime(x); haveO {
			return o, true
		} else if haveFB {
			return fb, true
		}
	}

	// Container formats: find TIFF headers near the start and try each.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return time.Time{}, false
	}
	data, _ := io.ReadAll(io.LimitReader(f, containerSearchBytes))

	var fallback time.Time
	haveFallback := false
	rest := data
	base := 0
	for tries := 0; tries < 8; tries++ {
		i := nextTIFFHeader(rest)
		if i < 0 {
			break
		}
		start := base + i
		if x := decodeExifSafely(bytes.NewReader(data[start:])); x != nil {
			o, fb, haveO, haveFB := exifTime(x)
			if haveO {
				return o, true
			}
			if haveFB && !haveFallback {
				fallback, haveFallback = fb, true
			}
		}
		base = start + 4
		rest = data[base:]
	}
	return fallback, haveFallback
}

// nextTIFFHeader returns the index of the next little- or big-endian TIFF
// header in b, or -1.
func nextTIFFHeader(b []byte) int {
	le := bytes.Index(b, []byte{'I', 'I', 0x2a, 0x00})
	be := bytes.Index(b, []byte{'M', 'M', 0x00, 0x2a})
	switch {
	case le < 0:
		return be
	case be < 0:
		return le
	case le < be:
		return le
	}
	return be
}

// -----------------------------------------------------------------------------
// MP4 / MOV / M4V / 3GP creation time
// -----------------------------------------------------------------------------

// secondsFrom1904To1970 converts the QuickTime epoch to the Unix epoch.
const secondsFrom1904To1970 = 2082844800

// videoCreationTime reads the movie header's creation time. It is recorded in
// UTC by phones; some cameras store their local clock time instead, so the
// value is used as written.
func videoCreationTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}

	moovStart, moovEnd, ok := findBox(f, 0, info.Size(), "moov")
	if !ok {
		return time.Time{}, false
	}
	start, end, ok := findBox(f, moovStart, moovEnd, "mvhd")
	if !ok || end-start < 8 {
		return time.Time{}, false
	}

	var head [12]byte
	if _, err := f.ReadAt(head[:], start); err != nil {
		return time.Time{}, false
	}
	var secs uint64
	if head[0] == 1 {
		var wide [8]byte
		if _, err := f.ReadAt(wide[:], start+4); err != nil {
			return time.Time{}, false
		}
		secs = binary.BigEndian.Uint64(wide[:])
	} else {
		secs = uint64(binary.BigEndian.Uint32(head[4:8]))
	}
	if secs <= secondsFrom1904To1970 { // 0 means "not set"
		return time.Time{}, false
	}
	t := time.Unix(int64(secs)-secondsFrom1904To1970, 0).UTC()
	if !plausibleDate(t) {
		return time.Time{}, false
	}
	return t, true
}

// findBox walks sibling boxes in [from, to) and returns the payload range of
// the first one called name.
func findBox(f *os.File, from, to int64, name string) (payloadStart, payloadEnd int64, ok bool) {
	off := from
	for off+8 <= to {
		var hdr [16]byte
		n, _ := f.ReadAt(hdr[:], off)
		if n < 8 {
			return 0, 0, false
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		kind := string(hdr[4:8])
		headerLen := int64(8)
		switch {
		case size == 1 && n >= 16:
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case size == 0:
			size = to - off
		}
		if size < headerLen || off+size > to {
			return 0, 0, false
		}
		if kind == name {
			return off + headerLen, off + size, true
		}
		off += size
	}
	return 0, 0, false
}

// -----------------------------------------------------------------------------
// Dates in filenames
// -----------------------------------------------------------------------------

// filenameDate extracts a date from a filename. Unlike the looser matcher used
// by scan, a bare run of digits only counts when it stands alone, so a sequence
// number such as IMG_120240315 or a 12-digit id is not mistaken for a date.
func filenameDate(name string) (time.Time, bool) {
	for i, p := range datePatterns {
		loc := p.regex.FindStringSubmatchIndex(name)
		if len(loc) < 4 {
			continue
		}
		start, end := loc[2], loc[3]
		// Patterns with a recognisable prefix (DJI_, 20240315_101010) are
		// specific enough; only the bare 8-digit one needs to stand alone.
		if i == len(datePatterns)-1 {
			if start > 0 && isDigit(name[start-1]) {
				continue
			}
			if end < len(name) && isDigit(name[end]) {
				continue
			}
		}
		if t, err := time.Parse(p.layout, name[start:end]); err == nil && plausibleDate(t) {
			return t, true
		}
	}
	return time.Time{}, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
