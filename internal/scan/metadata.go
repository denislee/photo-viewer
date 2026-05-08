package scan

import (
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

// GetMediaInfo returns extended metadata found in the file's metadata.
func GetMediaInfo(path string) MediaInfo {
	var info MediaInfo
	dateFound := false

	// Try exiftool first for everything.
	if _, err := exec.LookPath("exiftool"); err == nil {
		// Target tags for settings.
		cmd := exec.Command("exiftool", "-s", "-S",
			"-Model", "-LensModel",
			"-CreateDate", "-DateTimeOriginal", "-MediaCreateDate",
			"-FNumber", "-ExposureTime", "-ISO", "-FocalLength",
			path)
		out, err := cmd.Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for _, line := range lines {
				parts := strings.SplitN(line, ": ", 2)
				if len(parts) < 2 {
					continue
				}
				tag, val := parts[0], parts[1]
				switch tag {
				case "Model":
					info.Camera = val
				case "LensModel":
					info.Lens = val
				case "FNumber":
					info.Aperture = "f/" + val
				case "ExposureTime":
					info.ShutterSpeed = val + "s"
				case "ISO":
					info.ISO = val
				case "FocalLength":
					info.FocalLength = val
				case "CreateDate", "DateTimeOriginal", "MediaCreateDate":
					if !dateFound {
						for _, layout := range []string{"2006:01:02 15:04:05", "2006:01:02 15:04:05-07:00", "2006:01:02 15:04:05Z"} {
							t, err := time.Parse(layout, val)
							if err == nil {
								info.Created = t
								dateFound = true
								break
							}
						}
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

func safeRat(tag *tiff.Tag) (rat *big.Rat) {
	defer func() {
		recover()
	}()
	if r, err := tag.Rat(0); err == nil {
		return r
	}
	return nil
}

// GetMediaDate returns the best creation date found in the file's metadata.
// It tries exiftool first (for videos, RAW, and photos), then falls back to
// manual EXIF decoding (photos), and finally to the file's modification time.
func GetMediaDate(path string) time.Time {
	// First try exiftool for any media type (it handles photos, RAW, and videos).
	if _, err := exec.LookPath("exiftool"); err == nil {
		// Use -s -S for short tag names and no spaces/headers.
		cmd := exec.Command("exiftool", "-s", "-S", "-CreateDate", "-DateTimeOriginal", "-MediaCreateDate", path)
		out, err := cmd.Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// Try common formats.
				for _, layout := range []string{"2006:01:02 15:04:05", "2006:01:02 15:04:05-07:00", "2006:01:02 15:04:05Z"} {
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
