package fsutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMoveUniqueDeferredSameDevice pins the rename half of the contract:
// a same-filesystem move does not "copy", leaves no source behind, and returns
// the final path.
func TestMoveUniqueDeferredSameDevice(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "photo.jpg")
	writeFile(t, src, "NEW")

	dst, copied, err := MoveUniqueDeferred(context.Background(), src, destDir, "photo.jpg")
	if err != nil {
		t.Fatalf("MoveUniqueDeferred: %v", err)
	}
	if copied {
		t.Error("copied = true for a same-filesystem rename, want false")
	}
	if want := filepath.Join(destDir, "photo.jpg"); dst != want {
		t.Errorf("dst = %q, want %q", dst, want)
	}
	if readFile(t, dst) != "NEW" {
		t.Error("moved file has the wrong bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after a rename, stat err = %v", err)
	}
}

// TestMoveUniqueStillMoves keeps the wrapper honest: MoveUnique must still do
// the whole move (rename) after being implemented on top of
// MoveUniqueDeferred.
func TestMoveUniqueStillMoves(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "clip.mov")
	writeFile(t, src, "payload")

	dst, err := MoveUnique(context.Background(), src, destDir, "clip.mov")
	if err != nil {
		t.Fatalf("MoveUnique: %v", err)
	}
	if want := filepath.Join(destDir, "clip.mov"); dst != want {
		t.Errorf("dst = %q, want %q", dst, want)
	}
	if readFile(t, dst) != "payload" {
		t.Error("moved file has the wrong bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after MoveUnique, stat err = %v", err)
	}
}

// TestMoveUniqueDeferredCrossDevice drives the EXDEV copy branch for real when
// /dev/shm is a different filesystem: MoveUniqueDeferred must copy and report
// copied=true but leave src in place, so the caller can fsync the destination
// directory before unlinking the only other copy.
func TestMoveUniqueDeferredCrossDevice(t *testing.T) {
	destDir := t.TempDir()
	srcDir, err := os.MkdirTemp("/dev/shm", "pv-fsutil-deferred-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(srcDir) })
	if SameDevice(srcDir, destDir) {
		t.Skip("/dev/shm and TempDir share a filesystem; EXDEV path not reachable")
	}

	src := filepath.Join(srcDir, "clip.mov")
	writeFile(t, src, "from the card")

	dst, copied, err := MoveUniqueDeferred(context.Background(), src, destDir, "clip.mov")
	if err != nil {
		t.Fatalf("MoveUniqueDeferred: %v", err)
	}
	if !copied {
		t.Fatal("copied = false across devices, want true")
	}
	if readFile(t, dst) != "from the card" {
		t.Errorf("dest content = %q", readFile(t, dst))
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive until the caller syncs: %v", err)
	}

	// The caller's half of the deferred contract.
	if err := SyncDir(destDir); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be gone after SyncDir + Remove, stat err = %v", err)
	}
	noTemps(t, destDir)
}
