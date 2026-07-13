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
	"time"
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

// TestGetMediaDateReadsMetadata asserts GetMediaDate returns the creation date
// exiftool writes into a JPEG, and that it returns exactly what readMetadataDate
// returns — the delegation invariant introduced by S-07. The file mtime is
// pushed far away so a metadata miss couldn't masquerade as a hit. Gated on
// exiftool.
func TestGetMediaDateReadsMetadata(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping metadata read test")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jpg")

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	cmd := exec.Command("exiftool", "-overwrite_original",
		"-DateTimeOriginal=2024:05:04 12:34:56",
		"-CreateDate=2024:05:04 12:34:56",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exiftool write failed: %v: %s", err, out)
	}

	// Push the mtime far from the metadata date: if the metadata read silently
	// failed, GetMediaDate would fall back to this mtime and the assert below
	// would catch it.
	other := time.Date(2000, 1, 1, 0, 0, 0, 0, time.Local)
	if err := os.Chtimes(path, other, other); err != nil {
		t.Fatal(err)
	}

	got := GetMediaDate(path)
	if g := got.Format("2006-01-02 15:04:05"); g != "2024-05-04 12:34:56" {
		t.Errorf("GetMediaDate = %q, want %q", g, "2024-05-04 12:34:56")
	}

	// Delegation invariant: GetMediaDate must return exactly readMetadataDate's date.
	rd, ok := readMetadataDate(path)
	if !ok || !rd.Equal(got) {
		t.Errorf("GetMediaDate = %v, readMetadataDate = %v (ok=%v); want identical", got, rd, ok)
	}
}

// TestSetMediaDateWritesReadableDate exercises S-14: SetMediaDate now routes
// its write through the pooled -stay_open daemon (runExiftool) instead of a
// fresh exec per file. It writes a date into a JPEG via SetMediaDate, then
// reads it back through readMetadataDate/GetMediaDate and asserts the round
// trip matches — proving the daemon-routed write both succeeded (SetMediaDate
// returned nil) and produced date tags the readers can see. The filesystem
// mtime is checked too, covering the os.Chtimes half of SetMediaDate's
// contract. Gated on exiftool.
func TestSetMediaDateWritesReadableDate(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping metadata write test")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jpg")

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// Whole-second time so the filesystem-mtime round-trip below isn't tripped
	// up by sub-second truncation.
	want := time.Date(2019, 8, 17, 6, 5, 4, 0, time.Local)
	if err := SetMediaDate(path, want); err != nil {
		t.Fatalf("SetMediaDate: %v", err)
	}

	// Read the metadata date back through the reader path.
	rd, ok := readMetadataDate(path)
	if !ok {
		t.Fatal("readMetadataDate found no date after SetMediaDate wrote one")
	}
	// Compare formatted wall-clock: exiftool stores the date without a zone, so
	// readMetadataDate parses it as UTC while want is Local; the rendered digits
	// match regardless (same approach as TestGetMediaDateReadsMetadata).
	if g, w := rd.Format("2006:01:02 15:04:05"), want.Format("2006:01:02 15:04:05"); g != w {
		t.Errorf("readMetadataDate = %q, want %q", g, w)
	}

	// GetMediaDate delegates to readMetadataDate and must agree exactly.
	if g := GetMediaDate(path); !g.Equal(rd) {
		t.Errorf("GetMediaDate = %v, readMetadataDate = %v; want identical", g, rd)
	}

	// SetMediaDate also syncs the filesystem mtime to the written instant.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), want)
	}
}

// TestGetMediaDateFallsBackToModTime verifies the mtime fallback S-07 preserves:
// when no metadata date is present, GetMediaDate returns the file's modification
// time. No exiftool needed — a non-media file has none of the requested date
// tags, so readMetadataDate reports a miss whether or not exiftool is installed.
func TestGetMediaDateFallsBackToModTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("no metadata here"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Whole-second time to avoid filesystem sub-second truncation surprises.
	want := time.Date(2021, 3, 14, 9, 26, 53, 0, time.Local)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}

	if got := GetMediaDate(path); !got.Equal(want) {
		t.Errorf("GetMediaDate = %v, want file mtime %v", got, want)
	}
}
