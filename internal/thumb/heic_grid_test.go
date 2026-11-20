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

// TestHeicViaFfmpegGrid covers both HEIC container layouts heicViaFfmpeg must
// handle: a tiled grid (what every iPhone writes, ffmpeg >= 7.1 exposes it as a
// stream group, so only -filter_complex "[0:g:0]…" works) and a single-image
// HEIC (no stream group, so -vf is the form that works).
func TestHeicViaFfmpegGrid(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "heif-enc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}

	dir := t.TempDir()
	run := func(what string, args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", what, err, out)
		}
	}

	src := filepath.Join(dir, "src.png")
	run("ffmpeg fixture", "ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1024x768", "-frames:v", "1", src)

	grid := filepath.Join(dir, "grid.heic")
	run("heif-enc grid", "heif-enc", "--cut-tiles", "256", "-o", grid, src)
	single := filepath.Join(dir, "single.heic")
	run("heif-enc single", "heif-enc", "-o", single, src)

	for _, tc := range []struct{ name, src string }{
		{"grid.heic", grid},
		{"single.heic", single},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(dir, "out-"+tc.name+".jpg")
			if err := heicViaFfmpeg(context.Background(), tc.src, dst, 256); err != nil {
				t.Fatalf("heicViaFfmpeg: %v", err)
			}
			f, err := os.Open(dst)
			if err != nil {
				t.Fatalf("open thumb: %v", err)
			}
			defer f.Close()
			cfg, _, err := image.DecodeConfig(f)
			if err != nil {
				t.Fatalf("decode thumb config: %v", err)
			}
			if got := max(cfg.Width, cfg.Height); got != 256 {
				t.Errorf("thumb longest edge = %d, want 256 (%dx%d)", got, cfg.Width, cfg.Height)
			}
		})
	}
}
