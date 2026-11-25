package webserver

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestCachedSubdirsTTLBackstop is the W6-B5 regression test. On FAT32 a parent
// directory's mtime has 2 s resolution, so a child created in the same tick
// leaves it unchanged and the mtime check alone kept serving the stale listing
// forever. The ttlCache's TTL is the backstop: once it expires the listing is
// re-read even though the mtime still matches.
func TestCachedSubdirsTTLBackstop(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.subdirs.ttl = 50 * time.Millisecond
	if got := s.cachedSubdirs(root); len(got) != 1 {
		t.Fatalf("initial listing = %v, want one child", got)
	}

	// A same-tick child: create it, then put the parent's mtime back.
	if err := os.Mkdir(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	got := s.cachedSubdirs(root)
	if !slices.Contains(got, filepath.Join(root, "b")) {
		t.Errorf("listing after the TTL = %v, want it to include the same-tick child b", got)
	}
}
