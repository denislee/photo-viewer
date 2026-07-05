package scan

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
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

// GetMediaInfo returns extended metadata found in the file's metadata.
func GetMediaInfo(path string) MediaInfo {
	var info MediaInfo
	dateFound := false

	// Try exiftool first for everything. The reader goes through a shared
	// -stay_open daemon (see exiftool.go) so bulk reads avoid one fork per
	// file; first-failure falls back to per-call exec under the hood.
	if _, err := exec.LookPath("exiftool"); err == nil {
		// Read the settings tags as JSON. The old "-s -S" + "Tag: value" parse
		// was a silent no-op: "-s -S" is short-format level 3 (bare values, no
		// tag names), so every line failed the "Tag: value" split and nothing
		// was ever extracted — camera info came only from the goexif fallback
		// below (JPEG/TIFF only, never LensModel; nothing for HEIC/RAW/video).
		// "-j" gives an unambiguous tag→value map that covers all those formats.
		// (The date readers below deliberately keep the values-only short format
		// and must not be switched — don't "fix" them to match this.)
		out, err := runExiftool("-j",
			"-Model", "-LensModel",
			"-CreateDate", "-DateTimeOriginal", "-MediaCreateDate",
			"-FNumber", "-ExposureTime", "-ISO", "-FocalLength",
			path)
		if err == nil {
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
				for _, tag := range []string{"CreateDate", "DateTimeOriginal", "MediaCreateDate"} {
					v := exifJSONString(rec, tag)
					if v == "" {
						continue
					}
					for _, layout := range dateLayouts {
						if t, err := time.Parse(layout, v); err == nil {
							info.Created = t
							dateFound = true
							break
						}
					}
					if dateFound {
						break
					}
				}
			}
		}
	}

	// Fallback 1: Manual EXIF for photos if info missing.
	if !dateFound || info.Camera == "" || info.Aperture == "" {
		if f, err := os.Open(path); err == nil {
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
		recover()
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
	if _, err := exec.LookPath("exiftool"); err != nil {
		return fmt.Errorf("exiftool not available: %w", err)
	}
	stamp := t.Format("2006:01:02 15:04:05")
	cmd := exec.Command("exiftool",
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
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("exiftool: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_ = os.Chtimes(path, t, t)
	return nil
}

// readMetadataDate is the shared implementation used by GetMediaDate and
// GetOldestMediaDate. It returns the metadata creation date (if any) and a
// flag indicating whether one was found.
func readMetadataDate(path string) (time.Time, bool) {
	if _, err := exec.LookPath("exiftool"); err == nil {
		out, err := runExiftool("-s", "-S", "-CreateDate", "-DateTimeOriginal", "-MediaCreateDate", path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				for _, layout := range dateLayouts {
					if t, err := time.Parse(layout, line); err == nil {
						return t, true
					}
				}
			}
		}
	}
	if f, err := os.Open(path); err == nil {
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
// It tries exiftool first (for videos, RAW, and photos), then falls back to
// manual EXIF decoding (photos), and finally to the file's modification time.
func GetMediaDate(path string) time.Time {
	// First try exiftool for any media type (it handles photos, RAW, and videos).
	if _, err := exec.LookPath("exiftool"); err == nil {
		// Use -s -S for short tag names and no spaces/headers.
		out, err := runExiftool("-s", "-S", "-CreateDate", "-DateTimeOriginal", "-MediaCreateDate", path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// Try common formats.
				for _, layout := range dateLayouts {
					t, err := time.Parse(layout, line)
					if err == nil {
						return t
					}
				}
			}
		}
	}

	// Fallback 1: Manual EXIF for photos.
	if f, err := os.Open(path); err == nil {
		if x, err := exif.Decode(f); err == nil {
			if tm, err := x.DateTime(); err == nil {
				f.Close()
				return tm
			}
		}
		f.Close()
	}

	// Fallback 2: File modification time.
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}

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
