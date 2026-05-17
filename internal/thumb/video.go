package thumb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Video extracts a frame ~1 second into the file with ffmpeg and resamples it.
// Requires `ffmpeg` on PATH. ffmpeg writes directly to dst (which is the
// caller's .tmp path) to avoid a tmpdir + cross-filesystem copy.
func Video(ctx context.Context, src, dst string, size int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not installed")
	}
	// -ss before -i is fast (keyframe seek); -frames:v 1 grabs a single frame.
	// -vf scale fits longest edge to size while preserving aspect ratio.
	vf := fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", size, size)
	run := func(seek bool) error {
		args := []string{"-loglevel", "error", "-y"}
		if seek {
			args = append(args, "-ss", "1")
		}
		args = append(args,
			"-i", src,
			"-frames:v", "1",
			"-vf", vf,
			"-f", "image2",
			dst,
		)
		return exec.CommandContext(ctx, "ffmpeg", args...).Run()
	}
	if err := run(true); err != nil {
		// Retry from start in case the file is shorter than 1s.
		os.Remove(dst)
		if err := run(false); err != nil {
			return err
		}
	}
	if _, err := os.Stat(dst); err != nil {
		return err
	}
	return nil
}
