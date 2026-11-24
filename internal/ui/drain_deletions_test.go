package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestDrainDeletionsWaitsForIndexWrite pins I-19 item 6: once DeletePaths
// returns, DrainDeletions must not report done until the background
// goroutine's RemoveEntries has landed, so closing the index on exit can't
// strand a ghost row for a file that already left the library.
func TestDrainDeletionsWaitsForIndexWrite(t *testing.T) {
	root := t.TempDir()
	c := newTestController(t, root, filepath.Join(t.TempDir(), "cache"))
	victim := filepath.Join(root, "photo.jpg")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.index.ReconcileBatch([]scan.Result{{Path: victim, Type: scan.TypePhoto, Size: 1, ModTime: time.Now()}})
	if _, ok := c.index.GetEntry(victim); !ok {
		t.Fatal("seed row missing")
	}

	c.DeletePaths([]string{victim})
	if !c.DrainDeletions(5 * time.Second) {
		t.Fatal("DrainDeletions timed out")
	}
	if _, ok := c.index.GetEntry(victim); ok {
		t.Error("index row still present after DrainDeletions returned")
	}
}

// TestDrainDeletionsTimesOut checks the bounded wait: a deletion that never
// finishes must not hang the exit path.
func TestDrainDeletionsTimesOut(t *testing.T) {
	c := newTestController(t, t.TempDir(), filepath.Join(t.TempDir(), "cache"))
	release := make(chan struct{})
	c.deletions.Go(func() { <-release })
	if c.DrainDeletions(20 * time.Millisecond) {
		t.Error("DrainDeletions reported done while a deletion was still running")
	}
	close(release)
	if !c.DrainDeletions(5 * time.Second) {
		t.Error("DrainDeletions timed out after the deletion finished")
	}
}
