package thumb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// HEIC converts a HEIC/HEIF file to JPEG via heif-convert (libheif), then
// resamples it. Requires `heif-convert` on PATH.
func HEIC(src, dst string, size int) error {
	if _, err := exec.LookPath("heif-convert"); err != nil {
		return errors.New("heif-convert not installed")
	}
	tmpDir, err := os.MkdirTemp("", "photo-viewer-heic-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	intermediate := filepath.Join(tmpDir, "out.jpg")
	cmd := exec.Command("heif-convert", "--quiet", "-q", "85", src, intermediate)
	if err := cmd.Run(); err != nil {
		return err
	}
	// For multi-image HEIC (Apple live photos, HDR pairs, image stacks),
	// heif-convert writes out-1.jpg, out-2.jpg, ... and never creates the
	// requested name. Pick whichever JPEG it actually produced.
	jpg, err := pickHeifOutput(tmpDir, intermediate)
	if err != nil {
		return err
	}
	return Image(jpg, dst, size)
}

func pickHeifOutput(dir, preferred string) (string, error) {
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if len(matches) == 0 {
		return "", fmt.Errorf("heif-convert produced no jpeg output")
	}
	return matches[0], nil
}
