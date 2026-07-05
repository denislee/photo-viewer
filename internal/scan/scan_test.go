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
