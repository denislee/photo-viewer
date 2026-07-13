package thumb

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeJPEG encodes a tiny solid JPEG at path.
func writeJPEG(t *testing.T, path string, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, c)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
}

// TestPresentPreviewTags checks the single-daemon-call probe that replaced the
// old "fork exiftool -b once per candidate tag" loop: it must report only the
// preview tags the file actually carries, in the caller's priority order.
func TestPresentPreviewTags(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping preview-tag probe test")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	thumbSrc := filepath.Join(dir, "thumb.jpg")
	writeJPEG(t, src, color.RGBA{10, 20, 30, 255})
	writeJPEG(t, thumbSrc, color.RGBA{90, 90, 90, 255})

	// Graft an embedded EXIF ThumbnailImage (a binary preview tag) onto src.
	cmd := exec.Command("exiftool", "-overwrite_original",
		"-ThumbnailImage<="+thumbSrc, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exiftool embed failed: %v: %s", err, out)
	}

	tags := []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage", "PreviewTIFF", "OtherImage"}
	got := presentPreviewTags(src, tags)
	want := []string{"ThumbnailImage"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("presentPreviewTags = %v, want %v", got, want)
	}

	// A file with no embedded previews yields an empty set (caller then falls
	// back to trying every tag).
	plain := filepath.Join(dir, "plain.jpg")
	writeJPEG(t, plain, color.RGBA{5, 5, 5, 255})
	if got := presentPreviewTags(plain, tags); len(got) != 0 {
		t.Errorf("presentPreviewTags(plain) = %v, want empty", got)
	}
}

// TestExtractEmbeddedCapturesStderr checks that a failed embedded-preview
// extraction (S-17) surfaces exiftool's stderr detail instead of a bare
// "exit status 1".
func TestExtractEmbeddedCapturesStderr(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping stderr-capture test")
	}

	missing := filepath.Join(t.TempDir(), "nope.cr2")
	_, err := extractEmbedded(context.Background(), missing, "-PreviewImage")
	if err == nil {
		t.Fatal("expected an error for a nonexistent RAW file")
	}
	if !strings.Contains(err.Error(), "exiftool:") {
		t.Errorf("error should carry the exiftool prefix, got: %v", err)
	}
	// The stderr detail must survive into the error, not be dropped for a bare
	// "exit status 1".
	if !strings.Contains(err.Error(), "File not found") {
		t.Errorf("error should carry exiftool's stderr detail, got: %v", err)
	}
}
