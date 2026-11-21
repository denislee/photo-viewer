package thumb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHEICMissingDecoderForksFfmpegOnce pins T4-03: on a machine whose ffmpeg
// lacks HEIC and that has no heif-convert, each HEIC must pay one doomed fork,
// not two. HEIC() tries imageViaFfmpeg first; HEICToJPEG then receives
// skipFfmpeg=true and must not fork its own ffmpeg fallback for the same file.
// The ffmpegNoHEIC latch only sets after a successful fallback decode, so it
// stays clear here and cannot mask a second fork.
func TestHEICMissingDecoderForksFfmpegOnce(t *testing.T) {
	oldFfmpeg := haveFfmpeg
	oldHeifConvert := haveHeifConvert
	haveFfmpeg = func() bool { return true }
	haveHeifConvert = func() bool { return false }
	t.Cleanup(func() {
		haveFfmpeg = oldFfmpeg
		haveHeifConvert = oldHeifConvert
	})
	oldNoHEIC := ffmpegNoHEIC.Load()
	ffmpegNoHEIC.Store(false)
	t.Cleanup(func() { ffmpegNoHEIC.Store(oldNoHEIC) })

	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> \"$PV_FAKE_FFMPEG_COUNT\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PV_FAKE_FFMPEG_COUNT", count)
	t.Setenv("PATH", dir)

	src := filepath.Join(t.TempDir(), "a.heic")
	if err := os.WriteFile(src, []byte("not really a heic"), 0o644); err != nil {
		t.Fatalf("write dummy heic: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out.jpg")

	for i := 0; i < 3; i++ {
		if err := HEIC(context.Background(), src, dst, 64); err == nil {
			t.Fatalf("HEIC() call %d: want an error with no decoder available", i+1)
		}
	}

	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("read fork count: %v", err)
	}
	forks := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			forks++
		}
	}
	if forks != 3 {
		t.Errorf("ffmpeg forked %d times for 3 HEIC files, want exactly 3 (one per file): %q", forks, data)
	}
	if ffmpegNoHEIC.Load() {
		t.Errorf("ffmpegNoHEIC latched without a successful fallback decode")
	}
}

// TestHEICToJPEGKeepsFfmpegReason covers S6-04: with heif-convert missing,
// the ffmpeg fallback's failure reason must reach the caller.
func TestHEICToJPEGKeepsFfmpegReason(t *testing.T) {
	oldFfmpeg, oldHeifConvert := haveFfmpeg, haveHeifConvert
	haveFfmpeg = func() bool { return true }
	haveHeifConvert = func() bool { return false }
	t.Cleanup(func() { haveFfmpeg, haveHeifConvert = oldFfmpeg, oldHeifConvert })
	oldNoHEIC := ffmpegNoHEIC.Load()
	ffmpegNoHEIC.Store(false)
	t.Cleanup(func() { ffmpegNoHEIC.Store(oldNoHEIC) })

	dir := t.TempDir()
	script := "#!/bin/sh\necho 'boom: no hevc decoder' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	_, err := HEICToJPEG(context.Background(), "/nonexistent.heic", t.TempDir(), false)
	if err == nil {
		t.Fatal("HEICToJPEG = nil error, want failure")
	}
	for _, want := range []string{"heif-convert not installed", "ffmpeg: boom: no hevc decoder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
