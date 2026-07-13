package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
)

// newTestController builds a Controller against the given library root and
// cacheDir, backed by a throwaway index in a writable temp location.
func newTestController(t *testing.T, root, cacheDir string) *Controller {
	t.Helper()
	idx, err := cache.Load(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		t.Fatalf("NewThumbStore: %v", err)
	}
	return NewController(root, idx, store, cacheDir)
}

// TestIndexStatusReportsResolvedPath verifies IndexStatus surfaces the path
// cache.IndexPath actually resolved rather than a re-derived guess. For a
// writable library root that path is <root>/.photo-viewer.db.
func TestIndexStatusReportsResolvedPath(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")

	c := newTestController(t, root, cacheDir)

	want := cache.IndexPath(root, cacheDir)
	if got := c.IndexStatus().DBPath; got != want {
		t.Fatalf("IndexStatus().DBPath = %q, want %q", got, want)
	}
	if local := filepath.Join(root, ".photo-viewer.db"); want != local {
		t.Fatalf("writable-root path = %q, want %q", want, local)
	}
}

// TestIndexStatusReportsFallbackPath verifies that when the library root is
// read-only, IndexStatus reports the cacheDir fallback path (index-<hash>.db)
// that cache.IndexPath selected, not the non-existent <root>/.photo-viewer.db.
func TestIndexStatusReportsFallbackPath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: filesystem permissions do not restrict writes")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "ro-lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the library root read-only so IndexPath must fall back to cacheDir.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })

	cacheDir := filepath.Join(dir, "cache")

	c := newTestController(t, root, cacheDir)

	want := cache.IndexPath(root, cacheDir)
	if got := c.IndexStatus().DBPath; got != want {
		t.Fatalf("IndexStatus().DBPath = %q, want %q", got, want)
	}
	// Sanity: the fallback path lives under cacheDir, not the read-only root.
	if local := filepath.Join(root, ".photo-viewer.db"); want == local {
		t.Fatalf("expected fallback under cacheDir, got library-root path %q", want)
	}
	if filepath.Dir(want) != cacheDir {
		t.Fatalf("fallback path %q not under cacheDir %q", want, cacheDir)
	}
}
