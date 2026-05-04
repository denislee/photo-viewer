package thumb

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// HEIC converts a HEIC/HEIF file to JPEG and resamples it. Tries
// `heif-convert` (libheif) first, falls back to `ffmpeg` if heif-convert is
// missing or produced no output.
func HEIC(src, dst string, size int) error {
	tmpDir, err := os.MkdirTemp("", "photo-viewer-heic-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	jpg, lastErr := decodeHEIC(src, tmpDir)
	if lastErr != nil {
		return lastErr
	}
	return Image(jpg, dst, size)
}

// HEICToJPEG decodes a HEIC/HEIF file to a full-resolution JPEG written under
// tmpDir. Returns the path of the produced JPEG; the caller is responsible for
// removing tmpDir.
func HEICToJPEG(src, tmpDir string) (string, error) {
	return decodeHEIC(src, tmpDir)
}

func decodeHEIC(src, tmpDir string) (string, error) {
	var firstErr error
	if _, err := exec.LookPath("heif-convert"); err == nil {
		intermediate := filepath.Join(tmpDir, "out.jpg")
		var stderr bytes.Buffer
		cmd := exec.Command("heif-convert", "-q", "90", "--quiet", src, intermediate)
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
		cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
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
