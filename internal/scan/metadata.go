package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

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
