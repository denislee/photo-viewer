package fsutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCopyToKeepsModeAndMtime locks the exact-copy contract: the destination
// keeps src's permission bits and mtime as well as its bytes, and src is left
// untouched.
func TestCopyToKeepsModeAndMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "dst.jpg")
	writeFile(t, src, "PIXELS")
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	captured := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(src, captured, captured); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := CopyTo(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("dst mode = %o, want 640", perm)
	}
	if !info.ModTime().Equal(captured) {
		t.Errorf("dst mtime = %v, want %v", info.ModTime(), captured)
	}
	if got := readFile(t, dst); got != "PIXELS" {
		t.Errorf("dst bytes = %q, want PIXELS", got)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	if got := readFile(t, src); got != "PIXELS" {
		t.Errorf("src bytes = %q, want PIXELS", got)
	}
	if perm := srcInfo.Mode().Perm(); perm != 0o640 {
		t.Errorf("src mode = %o, want 640", perm)
	}
	noTemps(t, dir)
}

// TestCopyToRefusesExistingDst: an occupied destination is never replaced —
// the copy fails with fs.ErrExist, leaves dst alone, and drops its temp.
func TestCopyToRefusesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "dst.jpg")
	writeFile(t, src, "NEW")
	writeFile(t, dst, "OLD")

	err := CopyTo(context.Background(), src, dst)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	if got := readFile(t, dst); got != "OLD" {
		t.Errorf("dst bytes = %q, want OLD (unchanged)", got)
	}
	noTemps(t, dir)
}

// TestMoveToSameFilesystem exercises the fast path: within one filesystem the
// rename succeeds, dst holds the bytes, and src is gone.
func TestMoveToSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	dst := filepath.Join(dir, "clip-moved.mp4")
	writeFile(t, src, "BYTES")

	if err := MoveTo(context.Background(), src, dst); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	if got := readFile(t, dst); got != "BYTES" {
		t.Errorf("dst bytes = %q, want BYTES", got)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still present after move: %v", err)
	}
	noTemps(t, dir)
}

// TestMoveToRefusesExistingDst: a same-filesystem rename must not clobber an
// existing destination, and both files must survive untouched.
func TestMoveToRefusesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	dst := filepath.Join(dir, "dst.mp4")
	writeFile(t, src, "NEW")
	writeFile(t, dst, "KEEP")

	if err := MoveTo(context.Background(), src, dst); err == nil {
		t.Fatal("MoveTo onto an existing dst returned nil, want error")
	}
	if got := readFile(t, dst); got != "KEEP" {
		t.Errorf("dst bytes = %q, want KEEP (unchanged)", got)
	}
	if got := readFile(t, src); got != "NEW" {
		t.Errorf("src bytes = %q, want NEW (still present)", got)
	}
	noTemps(t, dir)
}

// TestWriteFileAtomicReplaces: the second write replaces the first, applies the
// requested mode (creating parent dirs), and leaves no temp behind.
func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "config.json")

	if err := WriteFileAtomic(p, []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(one): %v", err)
	}
	if err := WriteFileAtomic(p, []byte("two"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(two): %v", err)
	}

	if got := readFile(t, p); got != "two" {
		t.Errorf("content = %q, want two", got)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
	noTemps(t, filepath.Dir(p))
}
