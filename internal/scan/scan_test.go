package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// walk is the zero-options WalkWith the tests use.
func walk(ctx context.Context, root string) <-chan Result {
	return WalkWith(ctx, root, WalkOptions{})
}

// TestWalkWorkersProbeCap pins S3-04: the ffprobe stage is capped at
// maxProbeWorkers no matter how many CPUs the meta stage uses.
func TestWalkWorkersProbeCap(t *testing.T) {
	meta, probe := walkWorkers()
	if meta < 4 {
		t.Fatalf("meta = %d, want >= 4", meta)
	}
	if probe != min(meta, maxProbeWorkers) || probe > maxProbeWorkers {
		t.Fatalf("probe = %d, want min(%d, %d)", probe, meta, maxProbeWorkers)
	}
}

// TestWalkKnownProbeFailureSkipsProbe verifies that a KnownDurationMs result of
// DurationProbeFailed is carried through to the emitted Result and does not
// hand the video to the ffprobe stage: a corrupt file's failed probe is
// persisted once instead of being re-forked on every scan.
func TestWalkKnownProbeFailureSkipsProbe(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(vid, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got []Result
	for r := range WalkWith(context.Background(), dir, WalkOptions{
		KnownDurationMs: func(string, int64, time.Time) int64 { return DurationProbeFailed },
	}) {
		got = append(got, r)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].DurationMs != DurationProbeFailed {
		t.Errorf("DurationMs = %d, want %d (a known probe failure must skip the probe)", got[0].DurationMs, DurationProbeFailed)
	}
}

// TestWalkKnownDurationGetsSizeAndMtime verifies the callback receives the
// file's real size and mtime, which is what lets the cache trust a stored
// duration only for the same file version (U5-05).
func TestWalkKnownDurationGetsSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "a.mp4")
	content := make([]byte, 321)
	if err := os.WriteFile(vid, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(vid)
	if err != nil {
		t.Fatal(err)
	}

	var gotSize int64
	var gotMtime time.Time
	called := false
	for range WalkWith(context.Background(), dir, WalkOptions{
		KnownDurationMs: func(_ string, size int64, mtime time.Time) int64 {
			called = true
			gotSize, gotMtime = size, mtime
			return 5000
		},
	}) {
	}

	if !called {
		t.Fatal("KnownDurationMs callback was never called")
	}
	if gotSize != want.Size() {
		t.Errorf("callback size = %d, want %d", gotSize, want.Size())
	}
	if !gotMtime.Equal(want.ModTime()) {
		t.Errorf("callback mtime = %v, want %v", gotMtime, want.ModTime())
	}
}

// TestWalkSkipsHiddenFiles verifies dot-prefixed files are ignored, so macOS
// AppleDouble junk (._*) and other hidden files never enter the index even
// though their extensions match real media (S-01).
func TestWalkSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"._IMG_0001.jpg", ".hidden.jpg", "b.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for r := range walk(context.Background(), dir) {
		got = append(got, filepath.Base(r.Path))
	}

	if len(got) != 1 || got[0] != "b.jpg" {
		t.Fatalf("walk returned %v, want [b.jpg] (hidden files must be skipped)", got)
	}
}

// TestWalkSymlinkFollowsTarget verifies a symlinked media file is emitted with
// the *target's* size (not the link's tiny lstat size), and a dangling symlink
// is skipped rather than indexed with bogus metadata (S-16).
func TestWalkSymlinkFollowsTarget(t *testing.T) {
	targetDir := t.TempDir()
	walkDir := t.TempDir()

	// Real file lives outside the walked dir so only the symlink is under test.
	target := filepath.Join(targetDir, "real.jpg")
	content := make([]byte, 500)
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(walkDir, "link.jpg")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := os.Symlink(filepath.Join(targetDir, "gone.jpg"), filepath.Join(walkDir, "dangling.jpg")); err != nil {
		t.Fatal(err)
	}

	var got []Result
	for r := range walk(context.Background(), walkDir) {
		got = append(got, r)
	}

	if len(got) != 1 {
		t.Fatalf("walk returned %d results, want 1 (dangling symlink must be skipped): %v", len(got), got)
	}
	if base := filepath.Base(got[0].Path); base != "link.jpg" {
		t.Fatalf("emitted %s, want link.jpg", base)
	}
	if got[0].Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d (must follow the symlink to the target, not the link's lstat size)", got[0].Size, len(content))
	}
}

// TestCountMatchesWalk verifies Count applies exactly the walk's filters
// (hidden files and dirs, unknown extensions, nested dirs), so the indexing
// progress total lines up with what the scan actually emits.
func TestCountMatchesWalk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"a.jpg", "b.mp4", "notes.txt", "._c.jpg", ".hidden.png",
		"sub/d.png", "sub/deeper/e.mov", ".cache/f.jpg",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	n, err := Count(context.Background(), dir)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	walked := 0
	for range WalkWith(context.Background(), dir, WalkOptions{}) {
		walked++
	}
	if n != 4 || walked != 4 {
		t.Fatalf("Count = %d, walk emitted %d; want 4 and 4 (a.jpg, b.mp4, sub/d.png, sub/deeper/e.mov)", n, walked)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Count(ctx, dir); err == nil {
		t.Fatal("Count with a cancelled context returned nil error")
	}
}
