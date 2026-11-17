package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestScanUnchangedSkipsRefresh is the C4-01 UI guard: a rescan whose flushes
// change no index row must not schedule a refreshFromIndex, while the first
// scan (which inserts rows) must.
func TestScanUnchangedSkipsRefresh(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jpg", "b.jpg", "c.png"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestController(t, root, filepath.Join(dir, "cache"))

	c.scanInto(context.Background(), root)
	waitRefreshSettled(t, c)
	first := c.refreshCalls.Load()
	if first == 0 {
		t.Fatal("first scan inserted rows but scheduled no refresh")
	}
	if n := c.index.Count(); n != 3 {
		t.Fatalf("index has %d rows after first scan, want 3", n)
	}

	c.scanInto(context.Background(), root)
	waitRefreshSettled(t, c)
	if got := c.refreshCalls.Load(); got != first {
		t.Fatalf("unchanged rescan ran %d extra refreshes, want 0", got-first)
	}
}
