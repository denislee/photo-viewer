package cache

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CacheDir returns the directory where thumbnails and other generated files
// for an explicitly chosen library (--root) are written: <libraryRoot>/
// .photo-viewer-cache, so the cache travels with the library. Falls back to
// the per-user cache dir when the library root is not writable.
func CacheDir(libraryRoot string) (string, error) {
	candidate := filepath.Join(libraryRoot, ".photo-viewer-cache")
	if writable(candidate) {
		return candidate, nil
	}
	return UserCacheDir()
}

// UserCacheDir returns (creating it if needed) this computer's per-user cache
// directory, e.g. ~/.cache/photo-viewer on Linux.
func UserCacheDir() (string, error) {
	user, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(user, "photo-viewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Locate picks where the cache directory and SQLite index live.
//
// With inRoot (the user passed --root) both are stored inside the library:
// <root>/.photo-viewer-cache and <root>/.photo-viewer.db, falling back to the
// user cache dir when the root is read-only.
//
// Without it (the library defaulted to the working directory) nothing is
// written into the library: the cache is the user cache dir and the index is
// index-<hash>.db inside it. An index that already exists at
// <root>/.photo-viewer.db is still used, since it holds favorites and other
// user data that would otherwise silently vanish.
func Locate(libraryRoot string, inRoot bool) (cacheDir, dbPath string, err error) {
	if inRoot {
		cacheDir, err = CacheDir(libraryRoot)
		if err != nil {
			return "", "", err
		}
		return cacheDir, IndexPath(libraryRoot, cacheDir), nil
	}
	cacheDir, err = UserCacheDir()
	if err != nil {
		return "", "", err
	}
	if local := filepath.Join(libraryRoot, ".photo-viewer.db"); fileExists(local) {
		return cacheDir, local, nil
	}
	return cacheDir, hashedIndexPath(libraryRoot, cacheDir), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// IndexPath returns the path to the SQLite database for the given library. It
// prefers <libraryRoot>/.photo-viewer.db if writable (to keep the index with
// the media), but falls back to a hashed name inside cacheDir if the library
// root is read-only.
func IndexPath(libraryRoot, cacheDir string) string {
	local := filepath.Join(libraryRoot, ".photo-viewer.db")
	if _, err := os.Stat(local); err == nil {
		// Existing database found in library root.
		return local
	}

	if info, err := os.Stat(libraryRoot); err == nil && info.IsDir() && writable(libraryRoot) {
		return local
	}

	// Library root is read-only or doesn't exist. Use cacheDir with a
	// deterministic name unique to this library path.
	return hashedIndexPath(libraryRoot, cacheDir)
}

// hashedIndexPath is the index location inside cacheDir, unique per library.
func hashedIndexPath(libraryRoot, cacheDir string) string {
	sum := sha1.Sum([]byte(libraryRoot))
	return filepath.Join(cacheDir, fmt.Sprintf("index-%x.db", sum[:8]))
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
