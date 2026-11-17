package thumb

import (
	"context"
)

// Video extracts a frame ~1 second into the file with ffmpeg and resamples it.
// Requires `ffmpeg` on PATH. ffmpeg writes directly to dst (which is the
// caller's .tmp path) to avoid a tmpdir + cross-filesystem copy.
func Video(ctx context.Context, src, dst string, size int) error {
	if !haveFfmpeg() {
		return errFfmpegNotInstalled
	}
	// -ss before -i is a fast keyframe seek to ~1 s in.
	if err := runFfmpegThumb(ctx, dst, size, nil, "-ss", "1", "-i", src); err != nil {
		// Retry from the start in case the file is shorter than 1 s.
		return runFfmpegThumb(ctx, dst, size, nil, "-i", src)
	}
	return nil
}
