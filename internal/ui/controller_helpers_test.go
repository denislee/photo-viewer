package ui

import (
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

// captureWarnings swaps warnUser for a recorder for the test's duration.
func captureWarnings(t *testing.T) <-chan string {
	t.Helper()
	ch := make(chan string, 16)
	orig := warnUser
	warnUser = func(title, msg string) { ch <- title + ": " + msg }
	t.Cleanup(func() { warnUser = orig })
	return ch
}
