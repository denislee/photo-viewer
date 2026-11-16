package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocate(t *testing.T) {
	userCache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", userCache)
	wantUser := filepath.Join(userCache, "photo-viewer")

	t.Run("ExplicitRootStoresInLibrary", func(t *testing.T) {
		root := t.TempDir()
		cacheDir, dbPath, err := Locate(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, ".photo-viewer-cache"); cacheDir != want {
			t.Errorf("cacheDir = %q, want %q", cacheDir, want)
		}
		if want := filepath.Join(root, ".photo-viewer.db"); dbPath != want {
			t.Errorf("dbPath = %q, want %q", dbPath, want)
		}
	})

	t.Run("DefaultRootUsesUserCache", func(t *testing.T) {
		root := t.TempDir()
		cacheDir, dbPath, err := Locate(root, false)
		if err != nil {
			t.Fatal(err)
		}
		if cacheDir != wantUser {
			t.Errorf("cacheDir = %q, want %q", cacheDir, wantUser)
		}
		if filepath.Dir(dbPath) != wantUser || !strings.HasPrefix(filepath.Base(dbPath), "index-") {
			t.Errorf("dbPath = %q, want index-<hash>.db under %q", dbPath, wantUser)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Errorf("library root was written to: %v", entries)
		}
	})

	t.Run("DefaultRootKeepsExistingLibraryIndex", func(t *testing.T) {
		root := t.TempDir()
		local := filepath.Join(root, ".photo-viewer.db")
		if err := os.WriteFile(local, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, dbPath, err := Locate(root, false)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != local {
			t.Errorf("dbPath = %q, want existing %q", dbPath, local)
		}
	})

	t.Run("ReadOnlyExplicitRootFallsBack", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores permissions")
		}
		root := t.TempDir()
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(root, 0o755) })
		cacheDir, dbPath, err := Locate(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if cacheDir != wantUser || filepath.Dir(dbPath) != wantUser {
			t.Errorf("got cacheDir=%q dbPath=%q, want both under %q", cacheDir, dbPath, wantUser)
		}
	})
}
