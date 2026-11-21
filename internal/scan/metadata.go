package scan

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
)

// MediaInfo holds extended metadata for a file.
type MediaInfo struct {
	Created      time.Time
	Camera       string
	Lens         string
	Aperture     string
	ShutterSpeed string
	ISO          string
	FocalLength  string
}

// dateLayouts are the timestamp formats exiftool emits for creation-date tags:
// naive local time and the two zoned variants. Shared by every date reader.
var dateLayouts = []string{
	"2006:01:02 15:04:05",
	"2006:01:02 15:04:05-07:00",
	"2006:01:02 15:04:05Z",
}

// metaExiftool and openExifFile are seams so tests can prove a successful
// exiftool read never opens the file for the goexif fallback; production never
// reassigns them.
var metaExiftool = RunExiftool
var openExifFile = os.Open

// parseExifDate parses one exiftool creation-date value in any of the
// dateLayouts. Shared by GetMediaInfo and readMetadataDate.
func parseExifDate(s string) (time.Time, bool) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// GetMediaInfo returns extended metadata found in the file's metadata.
func GetMediaInfo(path string) MediaInfo {
	var info MediaInfo
	dateFound := false

	// Try exiftool first for everything. The reader goes through a shared
	// -stay_open daemon (see exiftool.go) so bulk reads avoid one fork per
	// file; first-failure falls back to per-call exec under the hood, and it
	// errors out cleanly when exiftool is absent — so no exec.LookPath guard is
	// needed here (S-06).
	//
	// Read the settings tags as JSON. The old "-s -S" + "Tag: value" parse
	// was a silent no-op: "-s -S" is short-format level 3 (bare values, no
	// tag names), so every line failed the "Tag: value" split and nothing
	// was ever extracted — camera info came only from the goexif fallback
	// below (JPEG/TIFF only, never LensModel; nothing for HEIC/RAW/video).
	// "-j" gives an unambiguous tag→value map that covers all those formats.
	// (readMetadataDate below deliberately keeps the values-only short format
	// and must not be switched — don't "fix" it to match this.)
	out, exifErr := metaExiftool("-j",
		"-Model", "-LensModel",
		"-CreateDate", "-DateTimeOriginal", "-MediaCreateDate",
		"-EXIF:ModifyDate",
		"-FNumber", "-ExposureTime", "-ISO", "-FocalLength",
		path)
	if exifErr == nil {
		var records []map[string]json.RawMessage
		if json.Unmarshal(out, &records) == nil && len(records) > 0 {
			rec := records[0]
			info.Camera = exifJSONString(rec, "Model")
			info.Lens = exifJSONString(rec, "LensModel")
			if v := exifJSONString(rec, "FNumber"); v != "" {
				info.Aperture = "f/" + v
			}
			if v := exifJSONString(rec, "ExposureTime"); v != "" {
				info.ShutterSpeed = v + "s"
			}
			info.ISO = exifJSONString(rec, "ISO")
			info.FocalLength = exifJSONString(rec, "FocalLength")
			for _, tag := range []string{"CreateDate", "DateTimeOriginal", "MediaCreateDate", "ModifyDate"} {
				v := exifJSONString(rec, tag)
				if v == "" {
					continue
				}
				if t, ok := parseExifDate(v); ok {
					info.Created = t
					dateFound = true
					break
				}
			}
		}
	}

	// Fallback 1: goexif, only when exiftool itself failed (absent, or the
	// request errored). A successful exiftool read already asked for a superset
	// of these tags — -EXIF:ModifyDate covers goexif's DateTime fallback — so
	// re-reading the file would only repeat the miss (every video, most PNGs)
	// and, on TIFF-family RAWs, cost a whole-file ReadAll (S4-04).
	if exifErr != nil {
		if f, err := openExifFile(path); err == nil {
			if x, err := exif.Decode(f); err == nil {
				if !dateFound {
					if tm, err := x.DateTime(); err == nil {
						info.Created = tm
						dateFound = true
					}
				}
				if info.Camera == "" {
					if cam, err := x.Get(exif.Model); err == nil {
						info.Camera = strings.Trim(cam.String(), "\"")
					}
				}
				// Basic manual extraction for settings if exiftool missed them.
				if info.Aperture == "" {
					if fnum, err := x.Get(exif.FNumber); err == nil {
						if rat := safeRat(fnum); rat != nil {
							f, _ := rat.Float64()
							info.Aperture = fmt.Sprintf("f/%.1f", f)
						}
					}
				}
				if info.ShutterSpeed == "" {
					if exp, err := x.Get(exif.ExposureTime); err == nil {
						info.ShutterSpeed = strings.Trim(exp.String(), "\"") + "s"
					}
				}
				if info.ISO == "" {
					if iso, err := x.Get(exif.ISOSpeedRatings); err == nil {
						info.ISO = strings.Trim(iso.String(), "\"")
					}
				}
				if info.FocalLength == "" {
					if fl, err := x.Get(exif.FocalLength); err == nil {
						if rat := safeRat(fl); rat != nil {
							f, _ := rat.Float64()
							info.FocalLength = fmt.Sprintf("%.1f mm", f)
						}
					}
				}
			}
			f.Close()
		}
	}

	// Fallback 2: File modification time if no date found yet.
	if !dateFound {
		if fi, err := os.Stat(path); err == nil {
			info.Created = fi.ModTime()
		} else {
			info.Created = time.Now()
		}
	}

	return info
}

// exifJSONString returns rec[key] as a plain string whether exiftool encoded it
// as a JSON string ("1/250", "50.0 mm") or a bare JSON number (FNumber 2.8,
// ISO 400). A missing key yields "".
func exifJSONString(rec map[string]json.RawMessage, key string) string {
	raw, ok := rec[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Non-string JSON scalar (number/bool): the raw token is the value.
	return strings.TrimSpace(string(raw))
}

func safeRat(tag *tiff.Tag) (rat *big.Rat) {
	defer func() {
		_ = recover()
	}()
	if r, err := tag.Rat(0); err == nil {
		return r
	}
	return nil
}

// GetOldestMediaDate returns the oldest of the file's metadata creation date
// and its filesystem modification time, alongside the metadata date itself.
// mtimeOlder is true when mtime predates the EXIF/metadata date (in which
// case the caller may want to rewrite the metadata date back to mtime). When
// no metadata date is available, the metadata return value is the zero time
// and mtimeOlder is false.
func GetOldestMediaDate(path string) (oldest time.Time, metaDate time.Time, mtimeOlder bool) {
	metaDate, hasMeta := readMetadataDate(path)
	var mtime time.Time
	if fi, err := os.Stat(path); err == nil {
		mtime = fi.ModTime()
	}
	switch {
	case hasMeta && !mtime.IsZero():
		if mtime.Before(metaDate) {
			return mtime, metaDate, true
		}
		return metaDate, metaDate, false
	case hasMeta:
		return metaDate, metaDate, false
	case !mtime.IsZero():
		return mtime, time.Time{}, false
	default:
		return time.Now(), time.Time{}, false
	}
}

// SetMediaDate writes the given time into the file's EXIF/QuickTime
// creation tags via exiftool. It also updates the filesystem mtime so the
// two stay in sync. Returns an error if exiftool is missing or the write
// fails.
func SetMediaDate(path string, t time.Time) error {
	if !haveExiftool() {
		return errors.New("exiftool not available")
	}
	stamp := t.Format("2006:01:02 15:04:05")
	// Route the write through the pooled -stay_open daemon (RunExiftool) rather
	// than a fresh exec per file (S-14): a "fix dates" pass over hundreds of
	// files otherwise pays exiftool's ~50–150 ms fork+Perl startup each time,
	// while every metadata READ already rides the daemon. Writes are safe on the
	// daemon protocol — the update summary is plain text on stdout (only "-b"
	// binary reads are excluded). RunExiftool applies the argsUnsafeForDaemon
	// gate, so a path carrying a newline (which would corrupt the newline-framed
	// argfile stream) transparently falls back to a one-shot exec; it also
	// short-circuits to one-shot when the daemon spawn is backing off.
	out, err := RunExiftool(
		"-overwrite_original",
		"-P",
		"-DateTimeOriginal="+stamp,
		"-CreateDate="+stamp,
		"-ModifyDate="+stamp,
		"-MediaCreateDate="+stamp,
		"-MediaModifyDate="+stamp,
		"-TrackCreateDate="+stamp,
		"-TrackModifyDate="+stamp,
		path)
	if err != nil {
		// One-shot fallbacks surface a non-zero exit via err (and carry stderr
		// in the returned bytes); daemon I/O errors and "exiftool not installed"
		// also arrive here.
		return fmt.Errorf("exiftool: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// The daemon reports write success/failure only through its stdout summary:
	// the "{ready}" sentinel that frames the response fires whether the write
	// succeeded or not, and exiftool's human-readable "Error:" line goes to the
	// daemon's discarded stderr. Detect a failed write from the stdout summary
	// (this is what catches the daemon path — the one-shot path already failed
	// the err check above).
	if exiftoolWriteFailed(out) {
		return fmt.Errorf("exiftool: write failed: %s", strings.TrimSpace(string(out)))
	}
	_ = os.Chtimes(path, t, t)
	return nil
}

// exiftoolWriteFailed reports whether exiftool's stdout write summary indicates
// the update did not complete. On success exiftool prints "N image files
// updated"; on failure it prints "0 image files updated" plus "M files weren't
// updated due to errors" — both on stdout, which the -stay_open daemon
// captures. The absence of any "files updated" acknowledgement is treated as a
// failure too, guarding against an empty or unexpected response.
func exiftoolWriteFailed(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "weren't updated") || !strings.Contains(s, "files updated")
}

// readMetadataDate is the shared implementation used by GetMediaDate and
// GetOldestMediaDate. It returns the metadata creation date (if any) and a
// flag indicating whether one was found.
func readMetadataDate(path string) (time.Time, bool) {
	// No exec.LookPath guard: RunExiftool errors out cleanly when exiftool is
	// absent, so the err != nil path below skips straight to the goexif
	// fallback (S-06), which only runs when exiftool errored. -EXIF:ModifyDate
	// is requested last so it only wins when no capture date exists — it mirrors
	// goexif's DateTime fallback to IFD0 DateTime.
	out, err := metaExiftool("-s", "-S", "-CreateDate", "-DateTimeOriginal", "-MediaCreateDate", "-EXIF:ModifyDate", path)
	if err == nil {
		lines := strings.SplitSeq(strings.TrimSpace(string(out)), "\n")
		for line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if t, ok := parseExifDate(line); ok {
				return t, true
			}
		}
		return time.Time{}, false
	}
	if f, err := openExifFile(path); err == nil {
		defer f.Close()
		if x, err := exif.Decode(f); err == nil {
			if tm, err := x.DateTime(); err == nil {
				return tm, true
			}
		}
	}
	return time.Time{}, false
}

// GetMediaDate returns the best creation date found in the file's metadata.
// It delegates to readMetadataDate — exiftool first (for videos, RAW, and
// photos), then goexif decoding when exiftool is unavailable — and, when no
// metadata date is found, falls back to the file's modification time and
// finally the current time.
func GetMediaDate(path string) time.Time {
	if t, ok := readMetadataDate(path); ok {
		return t
	}
	// Fallback 1: File modification time.
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	// Fallback 2: current time.
	return time.Now()
}

// SameDateFolder returns true if the expected date matches the file's parent
// directory's name (expected format: YYYY-MM-DD).
func SameDateFolder(path string, date time.Time) bool {
	parent := filepath.Base(filepath.Dir(path))
	// We only check if the parent matches a YYYY-MM-DD pattern.
	if len(parent) != 10 || parent[4] != '-' || parent[7] != '-' {
		return false
	}

	return date.Format("2006-01-02") == parent
}
