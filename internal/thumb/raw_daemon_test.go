package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireRAWDaemonTools skips the tests below unless both tools the fixture and
// the extraction path need are installed.
func requireRAWDaemonTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"exiftool", "ffmpeg"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed; skipping RAW preview daemon test", tool)
		}
	}
}

// rawDaemonFixture writes a TIFF-structured "RAW" named x.nef carrying a 160x120
// JPEG preview in IFD1 and Orientation 6 in IFD0, and returns its path.
//
// ffmpeg renders the preview JPEG. The container is assembled directly rather
// than by the more obvious `exiftool -ThumbnailImage<=thumb.jpg base.tif`:
// exiftool only creates a thumbnail in IFD1 of certain file types ("may only
// create a thumbnail in IFD1 of certain types of files" — not a TIFF-format
// file), so on a TIFF it silently reports "1 image files unchanged" and leaves
// no preview behind. Writing the IFD1 ourselves gives the daemon extraction the
// exact layout a camera RAW has: an old-style JPEG thumbnail in IFD1.
func rawDaemonFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	thumbPath := filepath.Join(dir, "thumb.jpg")
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=size=160x120", "-frames:v", "1", thumbPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg preview: %v: %s", err, out)
	}
	thumb, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(dir, "base.tif")
	writeTIFFWithThumbnail(t, base, 320, 240, 6, thumb)

	// The extension only matters to scan; loadRAWPreview sniffs magic bytes.
	src := filepath.Join(dir, "x.nef")
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// tiffEntrySize is the on-disk size of one IFD entry.
const tiffEntrySize = 12

// writeTIFFWithThumbnail writes a minimal little-endian TIFF whose IFD0 records
// the stored dimensions and orientation, and whose IFD1 holds thumb as an
// old-style JPEG (Compression 6) thumbnail — the tags exiftool exposes as
// ThumbnailImage.
func writeTIFFWithThumbnail(t *testing.T, path string, width, height, orientation int, thumb []byte) {
	t.Helper()

	const (
		ifd0Entries = 3
		ifd1Entries = 3
		ifd0Off     = 8
	)
	ifd1Off := ifd0Off + 2 + ifd0Entries*tiffEntrySize + 4
	thumbOff := ifd1Off + 2 + ifd1Entries*tiffEntrySize + 4

	le := binary.LittleEndian
	b := make([]byte, thumbOff)
	copy(b, "II\x2a\x00") // little-endian TIFF magic
	le.PutUint32(b[4:], ifd0Off)

	entry := func(off int, tag, typ uint16, count, value uint32) {
		le.PutUint16(b[off:], tag)
		le.PutUint16(b[off+2:], typ)
		le.PutUint32(b[off+4:], count)
		le.PutUint32(b[off+8:], value)
	}

	o := ifd0Off
	le.PutUint16(b[o:], ifd0Entries)
	o += 2
	entry(o, 0x0100, 4, 1, uint32(width)) // ImageWidth (LONG)
	o += tiffEntrySize
	entry(o, 0x0101, 4, 1, uint32(height)) // ImageHeight (LONG)
	o += tiffEntrySize
	entry(o, 0x0112, 3, 1, uint32(orientation)) // Orientation (SHORT)
	o += tiffEntrySize
	le.PutUint32(b[o:], uint32(ifd1Off)) // IFD0.nextIFD -> IFD1

	o = ifd1Off
	le.PutUint16(b[o:], ifd1Entries)
	o += 2
	entry(o, 0x0103, 3, 1, 6) // Compression: old-style JPEG
	o += tiffEntrySize
	entry(o, 0x0201, 4, 1, uint32(thumbOff)) // ThumbnailOffset
	o += tiffEntrySize
	entry(o, 0x0202, 4, 1, uint32(len(thumb))) // ThumbnailLength
	o += tiffEntrySize
	le.PutUint32(b[o:], 0) // no further IFD

	b = append(b, thumb...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExtractEmbeddedMatchesOneShot checks that the T5-05 file-based extraction
// through the daemon (exiftool writing the tag with -W!) returns exactly the
// bytes a one-shot `exiftool -b` writes, and that an absent preview tag is an
// error rather than an empty preview.
func TestExtractEmbeddedMatchesOneShot(t *testing.T) {
	requireRAWDaemonTools(t)
	src := rawDaemonFixture(t)
	ctx := context.Background()

	got, err := extractEmbedded(ctx, src, "-ThumbnailImage")
	if err != nil {
		t.Fatalf("extractEmbedded: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("extractEmbedded returned no preview bytes")
	}
	want, err := extractEmbeddedOneShot(ctx, src, "-ThumbnailImage")
	if err != nil {
		t.Fatalf("extractEmbeddedOneShot: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("daemon extraction = %d bytes, one-shot = %d bytes; want identical", len(got), len(want))
	}

	if _, err := extractEmbedded(ctx, src, "-JpgFromRaw"); err == nil {
		t.Error("extractEmbedded(JpgFromRaw) = nil error, want an error for an absent tag")
	}
}

// TestLoadRAWPreviewOrientationFromProbe checks that the preview probe reports
// both the embedded preview and the RAW's Orientation, so loadRAWPreview
// returns the right orientation without a second open of the file.
func TestLoadRAWPreviewOrientationFromProbe(t *testing.T) {
	requireRAWDaemonTools(t)
	src := rawDaemonFixture(t)

	data, orientation, err := loadRAWPreview(context.Background(), src)
	if err != nil {
		t.Fatalf("loadRAWPreview: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("loadRAWPreview returned no preview bytes")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("preview is not decodable: %v", err)
	}
	if cfg.Width != 160 || cfg.Height != 120 {
		t.Errorf("preview is %dx%d (%s), want 160x120", cfg.Width, cfg.Height, format)
	}
	if orientation != 6 {
		t.Errorf("orientation = %d, want 6", orientation)
	}
}
