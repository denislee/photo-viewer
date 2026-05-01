package thumb

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
)

// RAW extracts the embedded JPEG preview from a camera RAW file using
// exiftool, then resamples it to a thumbnail. Falls back to JpgFromRaw if
// PreviewImage is missing. Requires `exiftool` on PATH.
func RAW(src, dst string, size int) error {
	if _, err := exec.LookPath("exiftool"); err != nil {
		return errors.New("exiftool not installed")
	}
	jpgBytes, err := extractEmbedded(src, "-PreviewImage")
	if err != nil || len(jpgBytes) == 0 {
		jpgBytes, err = extractEmbedded(src, "-JpgFromRaw")
	}
	if err != nil {
		return err
	}
	if len(jpgBytes) == 0 {
		return errors.New("no embedded preview in RAW file")
	}
	img, err := jpeg.Decode(bytes.NewReader(jpgBytes))
	if err != nil {
		// Some cameras embed a non-JPEG preview; try the generic decoder.
		img, _, err = image.Decode(bytes.NewReader(jpgBytes))
		if err != nil {
			return err
		}
	}
	return writeThumb(img, dst, size)
}

func extractEmbedded(src, tag string) ([]byte, error) {
	cmd := exec.Command("exiftool", "-b", tag, src)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
