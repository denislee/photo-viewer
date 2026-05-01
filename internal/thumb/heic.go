package thumb

import (
	"errors"
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
	cmd := exec.Command("heif-convert", "-q", "85", src, intermediate)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return Image(intermediate, dst, size)
}
