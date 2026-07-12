package thumb

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// Video extracts a frame ~1 second into the file with ffmpeg and resamples it.
// Requires `ffmpeg` on PATH. ffmpeg writes directly to dst (which is the
// caller's .tmp path) to avoid a tmpdir + cross-filesystem copy.
func Video(ctx context.Context, src, dst string, size int) error {
	if !haveFfmpeg() {
		return errFfmpegNotInstalled
	}
	// -ss before -i is fast (keyframe seek); -frames:v 1 grabs a single frame.
	// -vf scale fits longest edge to size while preserving aspect ratio.
	vf := ffmpegScaleFilter(size)
	run := func(seek bool) error {
		cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegVideoFrameArgs(seek, src, dst, vf)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return ffmpegError(err, &stderr)
		}
		return nil
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

// ffmpegVideoFrameArgs builds the ffmpeg argument list for a single-frame grab.
// `-threads 1` sits before -i so it caps the *decoder* thread count (C-06): a
// one-frame grab needs no parallel decode, yet without the cap each child
// defaults to ~NumCPU decode threads. A video-heavy warm-up runs the store's
// extSem-worth of ffmpegs at once, so an uncapped child fan-out spawns hundreds
// of runnable threads that starve the Gio UI. When seek is set, -ss before -i
// does a fast keyframe seek to ~1s in.
func ffmpegVideoFrameArgs(seek bool, src, dst, vf string) []string {
	args := []string{"-loglevel", "error", "-y"}
	if seek {
		args = append(args, "-ss", "1")
	}
	return append(args,
		"-threads", "1",
		"-i", src,
		"-frames:v", "1",
		"-vf", vf,
		"-f", "image2",
		dst,
	)
}
