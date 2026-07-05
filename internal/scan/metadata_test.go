package scan

import (
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExifJSONString covers the number/string coercion the "-j" reader relies
// on: exiftool encodes some tags as JSON strings ("1/250", "50.0 mm") and some
// as bare numbers (FNumber 2.8, ISO 400), and both must come back as plain
// strings. This is a pure unit test — no exiftool needed.
func TestExifJSONString(t *testing.T) {
	rec := map[string]json.RawMessage{
		"Model":        json.RawMessage(`"Canon EOS R5"`),
		"FNumber":      json.RawMessage(`2.8`),
		"ISO":          json.RawMessage(`400`),
		"ExposureTime": json.RawMessage(`"1/250"`),
		"FocalLength":  json.RawMessage(`"50.0 mm"`),
	}
	cases := map[string]string{
		"Model":        "Canon EOS R5",
		"FNumber":      "2.8",
		"ISO":          "400",
		"ExposureTime": "1/250",
		"FocalLength":  "50.0 mm",
		"Missing":      "",
	}
	for key, want := range cases {
		if got := exifJSONString(rec, key); got != want {
			t.Errorf("exifJSONString(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestGetMediaInfoParsesCameraAndLens is the regression guard for the bug this
// initiative fixes: GetMediaInfo used to parse a "Tag: value" format that
// "-s -S" never emits, so nothing was extracted and the exiftool branch was a
// silent no-op. It writes known tags into a JPEG with exiftool, then asserts
// GetMediaInfo reads them back. Crucially it asserts on Lens — the goexif
// fallback never fills Lens, so a correct Lens value can ONLY come from the
// exiftool JSON path, proving that path actually runs.
func TestGetMediaInfoParsesCameraAndLens(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping metadata read test")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jpg")

	// A minimal JPEG for exiftool to graft EXIF onto.
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	const (
		wantCamera = "Canon EOS R5"
		wantLens   = "RF24-70mm F2.8 L IS USM"
	)
	cmd := exec.Command("exiftool", "-overwrite_original",
		"-Model="+wantCamera,
		"-LensModel="+wantLens,
		"-FNumber=2.8",
		"-ExposureTime=1/250",
		"-ISO=400",
		"-FocalLength=50",
		"-DateTimeOriginal=2024:05:04 12:34:56",
		"-CreateDate=2024:05:04 12:34:56",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exiftool write failed: %v: %s", err, out)
	}

	info := GetMediaInfo(path)

	// Lens is the discriminator: only the exiftool JSON path fills it.
	if info.Lens != wantLens {
		t.Errorf("Lens = %q, want %q (exiftool JSON path did not run)", info.Lens, wantLens)
	}
	if info.Camera != wantCamera {
		t.Errorf("Camera = %q, want %q", info.Camera, wantCamera)
	}
	if info.Aperture != "f/2.8" {
		t.Errorf("Aperture = %q, want %q", info.Aperture, "f/2.8")
	}
	if info.ShutterSpeed != "1/250s" {
		t.Errorf("ShutterSpeed = %q, want %q", info.ShutterSpeed, "1/250s")
	}
	if info.ISO != "400" {
		t.Errorf("ISO = %q, want %q", info.ISO, "400")
	}
	if info.FocalLength != "50.0 mm" {
		t.Errorf("FocalLength = %q, want %q", info.FocalLength, "50.0 mm")
	}
	if got := info.Created.Format("2006-01-02 15:04:05"); got != "2024-05-04 12:34:56" {
		t.Errorf("Created = %q, want %q", got, "2024-05-04 12:34:56")
	}
}
