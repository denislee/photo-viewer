package thumb

import (
	"context"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFfmpegVideoFrameArgsThreadCap pins C-06: the single-frame grab must carry
// `-threads 1` and place it before `-i` so it caps the *decoder* thread count
// (a decoder-context option). Placed after -i it would size only the trivial
// single-frame encoder and leave decode uncapped — the whole point being to keep
// a video-heavy warm-up from spawning NumCPU decode threads per concurrent
// ffmpeg. Both the fast-seek and the shorter-than-1s retry paths must cap.
func TestFfmpegVideoFrameArgsThreadCap(t *testing.T) {
	for _, seek := range []bool{true, false} {
		args := ffmpegVideoFrameArgs(seek, "in.mp4", "out.jpg", "scale=w:h")

		ti := indexOfThreadsOne(args)
		if ti < 0 {
			t.Fatalf("seek=%v: args missing `-threads 1`: %v", seek, args)
		}
		ii := indexOf(args, "-i")
		if ii < 0 {
			t.Fatalf("seek=%v: args missing `-i`: %v", seek, args)
		}
		if ti > ii {
			t.Errorf("seek=%v: `-threads 1` at %d must precede `-i` at %d to cap decode: %v", seek, ti, ii, args)
		}
	}
}

// indexOfThreadsOne returns the index of "-threads" when immediately followed by
// "1", or -1 if no such pair exists.
func indexOfThreadsOne(args []string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-threads" && args[i+1] == "1" {
			return i
		}
	}
	return -1
}

func indexOf(args []string, want string) int {
	for i := range args {
		if args[i] == want {
			return i
		}
	}
	return -1
}

// TestVideoFrameGrabWithThreadCap confirms the frame grab still succeeds end to
// end with the C-06 thread cap: it synthesizes a short clip, thumbnails it, and
// checks a bounded, decodable JPEG lands. Skips cleanly when ffmpeg (or the
// lavfi/mjpeg pieces it needs to build the clip) is unavailable.
func TestVideoFrameGrabWithThreadCap(t *testing.T) {
	if !haveFfmpeg() {
		t.Skip("ffmpeg not installed; skipping video frame-grab test")
	}
	dir := t.TempDir()
	// mjpeg-in-AVI uses only ffmpeg's built-in encoder (no libx264 dependency)
	// and is all-keyframe, so the -ss 1 seek in the grab resolves cleanly.
	src := filepath.Join(dir, "clip.avi")
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-c:v", "mjpeg", "-q:v", "5", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize test clip (lavfi/mjpeg unavailable): %v: %s", err, out)
	}

	dst := filepath.Join(dir, "thumb.jpg")
	if err := Video(context.Background(), src, dst, 128); err != nil {
		t.Fatalf("Video() frame grab failed: %v", err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open thumb: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("thumb is not a decodable image: %v", err)
	}
	if cfg.Width > 128 || cfg.Height > 128 {
		t.Errorf("thumb %dx%d exceeds the 128px bound", cfg.Width, cfg.Height)
	}
}
