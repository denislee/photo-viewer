package thumb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Video extracts a frame ~1 second into the file with ffmpeg and resamples it.
// Requires `ffmpeg` on PATH.
func Video(ctx context.Context, src, dst string, size int) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not installed")
	}
	tmpDir, err := os.MkdirTemp("", "photo-viewer-vid-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	frame := filepath.Join(tmpDir, "frame.jpg")
	// -ss before -i is fast (keyframe seek); -frames:v 1 grabs a single frame.
	// -vf scale fits longest edge to size while preserving aspect ratio.
	vf := fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", size, size)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-y",
		"-ss", "1",
		"-i", src,
		"-frames:v", "1",
		"-vf", vf,
		frame,
	)
	if err := cmd.Run(); err != nil {
		// Retry from start in case the file is shorter than 1s.
		cmd2 := exec.CommandContext(ctx, "ffmpeg",
			"-loglevel", "error",
			"-y",
			"-i", src,
			"-frames:v", "1",
			"-vf", vf,
			frame,
		)
		if err := cmd2.Run(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(frame); err != nil {
		return err
	}
	// frame is already sized via -vf scale; just copy it to dst.
	return moveFile(frame, dst)
}

func moveFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 32*1024)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			break
		}
	}
	return nil
}
