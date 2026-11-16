package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestWalkSkipDurationProbe verifies that WalkOptions.SkipDurationProbe
// short-circuits video duration handling entirely: no ffprobe is run and the
// emitted DurationMs is 0 even when a KnownDurationMs callback would otherwise
// supply a value. This is what lets pv-organize (which never reads durations)
// avoid one ffprobe fork per clip.
func TestWalkSkipDurationProbe(t *testing.T) {
	dir := t.TempDir()
	vid := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(vid, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}

	known := func(string) int64 { return 5000 } // would set a duration if consulted
	var got []Result
	for r := range WalkWith(context.Background(), dir, WalkOptions{
		SkipDurationProbe: true,
		KnownDurationMs:   known,
	}) {
		got = append(got, r)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].Type != TypeVideo {
		t.Fatalf("expected TypeVideo, got %v", got[0].Type)
	}
	if got[0].DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0 (SkipDurationProbe must win over KnownDurationMs)", got[0].DurationMs)
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
	for r := range Walk(context.Background(), dir) {
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
	for r := range Walk(context.Background(), walkDir) {
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
	for range WalkWith(context.Background(), dir, WalkOptions{SkipDurationProbe: true}) {
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
