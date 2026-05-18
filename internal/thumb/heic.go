package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
)

// HEIC converts a HEIC/HEIF file to JPEG and resamples it.
//
// Strategy: prefer ffmpeg with `-vf scale` (one subprocess does the HEIC
// decode + downscale + JPEG encode in a single pass — same trick the video
// thumbnailer uses). Fall back to `heif-convert` + an in-process scale when
// ffmpeg can't open the file (HEIC support depends on how ffmpeg was built).
// The previous implementation always wrote a full-resolution JPEG then
// re-decoded + re-scaled + re-encoded it, paying for two JPEG codec passes.
func HEIC(ctx context.Context, src, dst string, size int) error {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		if err := imageViaFfmpeg(ctx, src, dst, size); err == nil {
			return nil
		}
		// fall through — ffmpeg either lacks HEIC or refused this file.
	}

	tmpDir, err := os.MkdirTemp("", "photo-viewer-heic-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	jpg, lastErr := decodeHEIC(ctx, src, tmpDir)
	if lastErr != nil {
		return lastErr
	}
	// jpg is a freshly written JPEG of unknown size. Skip the os.Stat /
	// ffmpeg-threshold branch inside Image() (we know exactly where the
	// file came from) and just decode + writeThumb directly.
	in, err := os.Open(jpg)
	if err != nil {
		return err
	}
	defer in.Close()
	img, _, err := image.Decode(in)
	if err != nil {
		return err
	}
	return writeThumb(img, dst, size)
}

// HEICToJPEG decodes a HEIC/HEIF file to a full-resolution JPEG written under
// tmpDir. Returns the path of the produced JPEG; the caller is responsible for
// removing tmpDir.
func HEICToJPEG(ctx context.Context, src, tmpDir string) (string, error) {
	return decodeHEIC(ctx, src, tmpDir)
}

func decodeHEIC(ctx context.Context, src, tmpDir string) (string, error) {
	var firstErr error
	if _, err := exec.LookPath("heif-convert"); err == nil {
		intermediate := filepath.Join(tmpDir, "out.jpg")
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "heif-convert", "-q", "90", "--quiet", src, intermediate)
		cmd.Stderr = &stderr
		errRun := cmd.Run()
		// For multi-image HEIC (Apple live photos, HDR pairs, image stacks)
		// heif-convert writes out-1.jpg, out-2.jpg, ... and never creates the
		// requested name. Pick whichever JPEG it actually produced; ignore
		// errRun if any usable output landed since heif-convert sometimes
		// exits non-zero after successfully writing the primary image.
		if jpg, err := pickHeifOutput(tmpDir, intermediate); err == nil {
			return jpg, nil
		}
		if errRun != nil {
			msg := stderr.String()
			if msg == "" {
				msg = errRun.Error()
			}
			firstErr = fmt.Errorf("heif-convert: %s", msg)
		} else {
			firstErr = errors.New("heif-convert produced no jpeg output")
		}
	} else {
		firstErr = errors.New("heif-convert not installed")
	}

	// Fallback: ffmpeg can decode HEIC via libheif/HEVC and write a JPEG.
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		out := filepath.Join(tmpDir, "ff-out.jpg")
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-loglevel", "error",
			"-i", src, "-frames:v", "1", "-update", "1", out)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			if _, statErr := os.Stat(out); statErr == nil {
				return out, nil
			}
		} else if firstErr == nil {
			msg := stderr.String()
			if msg == "" {
				msg = err.Error()
			}
			firstErr = fmt.Errorf("ffmpeg: %s", msg)
		}
	}
	if firstErr == nil {
		firstErr = errors.New("no HEIC decoder available")
	}
	return "", firstErr
}

func pickHeifOutput(dir, preferred string) (string, error) {
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}
	// Handle sequences and potential extension differences (case/jpeg vs jpg)
	for _, ext := range []string{"*.jpg", "*.jpeg", "*.JPG", "*.JPEG", "*.png"} {
		matches, _ := filepath.Glob(filepath.Join(dir, ext))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("heif-convert produced no jpeg output")
}
