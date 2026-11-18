package ui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestApplyMoveKeepsProbedMediaDate guards the "rename carries the cache" rule
// for the Organize flow: a move relocates the row in place, so the persisted
// media_date stays valid for the unchanged bytes and the follow-up scan uses the
// cache instead of re-probing (a rename bumps no mtime).
func TestApplyMoveKeepsProbedMediaDate(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	srcDir := filepath.Join(root, "2024-03-15")
	dstDir := filepath.Join(root, "2024-03-20")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const nClips = 3
	results := make([]scan.Result, 0, nClips)
	oldPaths := make([]string, 0, nClips)
	newPaths := make([]string, 0, nClips)
	for i := range nClips {
		p := filepath.Join(srcDir, "clip"+strconv.Itoa(i)+".mp4")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, scan.Result{
			Path:    p,
			Type:    scan.TypeVideo,
			Size:    1,
			ModTime: info.ModTime(),
		})
		oldPaths = append(oldPaths, p)
		newPaths = append(newPaths, filepath.Join(dstDir, "clip"+strconv.Itoa(i)+".mp4"))
	}

	c := newTestController(t, root, filepath.Join(dir, "cache"))
	c.index.ReconcileBatch(results)

	var probes atomic.Int64
	orig := mediaDateProber
	mediaDateProber = func(string) time.Time {
		probes.Add(1)
		return time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() { mediaDateProber = orig })

	v := NewOrganizeView(func() {})
	v.scanForMismatched(context.Background(), c.index)
	if got := probes.Load(); got != nClips {
		t.Fatalf("first pass probed %d clips, want %d", got, nClips)
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range oldPaths {
		if err := os.Rename(oldPaths[i], newPaths[i]); err != nil {
			t.Fatal(err)
		}
		if err := c.ApplyMove(oldPaths[i], newPaths[i]); err != nil {
			t.Fatalf("ApplyMove(%s, %s): %v", oldPaths[i], newPaths[i], err)
		}
	}

	v.scanForMismatched(context.Background(), c.index)
	if got := probes.Load(); got != nClips {
		t.Fatalf("after moves probe count = %d, want %d (moved rows keep their media_date)", got, nClips)
	}

	for i := range newPaths {
		if _, ok := c.index.GetEntry(newPaths[i]); !ok {
			t.Errorf("GetEntry(new) not found for %s", newPaths[i])
		}
		if _, ok := c.index.GetEntry(oldPaths[i]); ok {
			t.Errorf("GetEntry(old) still present for %s", oldPaths[i])
		}
	}
}

// TestApplyMoveFallbackWithoutRow verifies a move whose source was never indexed
// still succeeds: ApplyMove indexes the destination from a fresh stat instead of
// surfacing sql.ErrNoRows.
func TestApplyMoveFallbackWithoutRow(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, "b", "n.jpg")
	if err := os.WriteFile(newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newTestController(t, root, filepath.Join(dir, "cache"))

	oldPath := filepath.Join(root, "a", "n.jpg")
	if err := c.ApplyMove(oldPath, newPath); err != nil {
		t.Fatalf("ApplyMove: %v", err)
	}
	if _, ok := c.index.GetEntry(newPath); !ok {
		t.Errorf("GetEntry(new) not found after fallback reconcile")
	}
}
