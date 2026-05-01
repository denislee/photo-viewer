package cache

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// FindDriveRoot walks up from p until the parent directory is on a different
// filesystem. The returned path is the topmost directory that still lives on
// the same mount point as p.
func FindDriveRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(abs, &st); err != nil {
		return "", err
	}
	dev := st.Dev
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, nil
		}
		var ps syscall.Stat_t
		if err := syscall.Stat(parent, &ps); err != nil {
			return cur, nil
		}
		if ps.Dev != dev {
			return cur, nil
		}
		cur = parent
	}
}

// CacheDir returns the directory where the index and thumbnails should be
// written. It first tries <drive-root>/.photo-viewer-cache and falls back to
// the user cache dir if the drive root is not writable.
func CacheDir(libraryRoot string) (string, error) {
	driveRoot, err := FindDriveRoot(libraryRoot)
	if err == nil {
		candidate := filepath.Join(driveRoot, ".photo-viewer-cache")
		if writable(candidate) {
			if err := os.MkdirAll(candidate, 0o755); err == nil {
				return candidate, nil
			}
		}
	}
	user, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	fallback := filepath.Join(user, "photo-viewer")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		return "", err
	}
	return fallback, nil
}

func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

var ErrNoRoot = errors.New("no library root configured")
