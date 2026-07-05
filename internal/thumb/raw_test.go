package thumb

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
