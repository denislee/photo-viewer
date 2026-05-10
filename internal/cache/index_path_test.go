package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "pv-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	t.Run("ExistingLocal", func(t *testing.T) {
		libRoot := filepath.Join(tmpDir, "lib-existing")
		os.MkdirAll(libRoot, 0755)
		dbFile := filepath.Join(libRoot, ".photo-viewer.db")
		os.WriteFile(dbFile, []byte("fake db"), 0644)

		path := IndexPath(libRoot, cacheDir)
		if path != dbFile {
			t.Errorf("expected %s, got %s", dbFile, path)
		}
	})

	t.Run("WritableRoot", func(t *testing.T) {
		libRoot := filepath.Join(tmpDir, "lib-writable")
		os.MkdirAll(libRoot, 0755)
		expected := filepath.Join(libRoot, ".photo-viewer.db")

		path := IndexPath(libRoot, cacheDir)
		if path != expected {
			t.Errorf("expected %s, got %s", expected, path)
		}
	})

	t.Run("ReadOnlyRoot", func(t *testing.T) {
		libRoot := filepath.Join(tmpDir, "lib-readonly")
		os.MkdirAll(libRoot, 0555) // Read-only
		defer os.Chmod(libRoot, 0755) // So RemoveAll can clean it up

		path := IndexPath(libRoot, cacheDir)
		if !strings.HasPrefix(path, cacheDir) {
			t.Errorf("expected path in cacheDir %s, got %s", cacheDir, path)
		}
		if !strings.HasSuffix(path, ".db") {
			t.Errorf("expected .db suffix, got %s", path)
		}
		if strings.Contains(path, libRoot) {
			// It shouldn't be under libRoot
			if rel, err := filepath.Rel(libRoot, path); err == nil && !strings.HasPrefix(rel, "..") {
				t.Errorf("path %s should not be under libRoot %s", path, libRoot)
			}
		}
	})
}
