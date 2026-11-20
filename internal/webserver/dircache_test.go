package webserver

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// countListSubdirs installs a listSubdirsHook that counts reads of dir and
// returns a function yielding the total. The hook is global, so the test must
// not run in parallel with other tests that touch it.
func countListSubdirs(t *testing.T, dir string) func() int {
	t.Helper()
	var calls int
	listSubdirsHook = func(d string) {
		if d == dir {
			calls++
		}
	}
	t.Cleanup(func() { listSubdirsHook = nil })
	return func() int { return calls }
}

// TestCachedSubdirsReusesUntilDirChanges guards W5-02: a repeated render of the
// same directory must not re-read its children, and a new child must show up as
// soon as the parent's mtime moves (which is what creating/removing/renaming a
// child does).
func TestCachedSubdirsReusesUntilDirChanges(t *testing.T) {
	s, _, cleanup := fixture(t, 0)
	defer cleanup()

	dir := filepath.Join(s.libraryRoot, "album")
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	reads := countListSubdirs(t, dir)

	first := s.cachedSubdirs(dir)
	if got := reads(); got != 1 {
		t.Fatalf("listSubdirs reads after first call = %d, want 1", got)
	}
	if len(first) != 1 || first[0] != filepath.Join(dir, "a") {
		t.Fatalf("first listing = %v, want [%s]", first, filepath.Join(dir, "a"))
	}

	second := s.cachedSubdirs(dir)
	if got := reads(); got != 1 {
		t.Fatalf("listSubdirs reads after second call = %d, want 1 (cached)", got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cached listing changed: first=%v second=%v", first, second)
	}

	// A new child moves the parent's mtime, but filesystem mtime granularity
	// can round the two updates together, so bump it explicitly into the future.
	if err := os.Mkdir(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}

	third := s.cachedSubdirs(dir)
	if got := reads(); got != 2 {
		t.Fatalf("listSubdirs reads after child created = %d, want 2 (mtime changed)", got)
	}
	want := []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}
	if !reflect.DeepEqual(third, want) {
		t.Fatalf("listing after new child = %v, want %v", third, want)
	}
}

// TestCachedChildCountsComputesOnce guards W5-02: the filtered child counts for
// one (dir, filter, raw) key are computed once per TTL/generation and the same
// map is handed back, so the sidebar and the chip strip share one scan. Map
// identity is asserted with Pointer because the returned map must be treated as
// read-only shared state.
func TestCachedChildCountsComputesOnce(t *testing.T) {
	s, _, cleanup := fixture(t, 0)
	defer cleanup()

	dir := filepath.Join(s.libraryRoot, "album")
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	s.index.ReconcileBatch([]scan.Result{{
		Path:    filepath.Join(sub, "000001.jpg"),
		Type:    scan.TypePhoto,
		Size:    1000,
		ModTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}})

	first := s.cachedChildCounts(dir, "All", true)
	second := s.cachedChildCounts(dir, "All", true)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("counts differ between calls: first=%v second=%v", first, second)
	}
	if got := first[sub]; got != 1 {
		t.Errorf("count for %s = %d, want 1", sub, got)
	}
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() {
		t.Errorf("second call returned a different map, want the cached one")
	}
}
