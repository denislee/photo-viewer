package imgorient

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// exifTIFFPayload builds an APP1 payload of "Exif\0\0" followed by a minimal
// little-endian TIFF whose IFD0 holds Orientation=orientation.
func exifTIFFPayload(orientation uint16) []byte {
	tiff := make([]byte, 8+2+12+4)
	copy(tiff, []byte{'I', 'I', 42, 0})
	binary.LittleEndian.PutUint32(tiff[4:], 8) // IFD0 offset
	binary.LittleEndian.PutUint16(tiff[8:], 1) // one entry
	e := tiff[10:]
	binary.LittleEndian.PutUint16(e[0:], 0x0112) // Orientation
	binary.LittleEndian.PutUint16(e[2:], 3)      // SHORT
	binary.LittleEndian.PutUint32(e[4:], 1)      // count
	binary.LittleEndian.PutUint16(e[8:], orientation)
	// e[12:] (next-IFD offset) stays 0.
	return append([]byte("Exif\x00\x00"), tiff...)
}

// app1Segment wraps payload in a JPEG APP1 segment (FF E1 + big-endian length).
func app1Segment(payload []byte) []byte {
	seg := make([]byte, 4+len(payload))
	seg[0], seg[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	copy(seg[4:], payload)
	return seg
}

// jfifApp0 is a minimal APP0/JFIF segment, the first segment in most encoded
// JPEGs.
func jfifApp0() []byte {
	return []byte{
		0xFF, 0xE0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, // version 1.1
		0x00,       // no units
		0x00, 0x01, // x density
		0x00, 0x01, // y density
		0x00, 0x00, // no thumbnail
	}
}

var jpegSOS = []byte{0xFF, 0xDA, 0x00, 0x02}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestJPEGOrientation covers the marker walk directly through
// ReadOrientationFrom: EXIF after an APP0, XMP-before-EXIF (which goexif would
// misread), no EXIF at all, truncation, and a real encoder's output.
func TestJPEGOrientation(t *testing.T) {
	soi := []byte{0xFF, 0xD8}
	exif6 := app1Segment(exifTIFFPayload(6))
	xmp := app1Segment(append([]byte("http://ns.adobe.com/xap/1.0/\x00"), []byte("<x:xmpmeta/>")...))

	// A segment that advertises 0x64 payload bytes but only carries the 6-byte
	// EXIF identifier, so reading its TIFF section runs off the end.
	truncated := []byte{0xFF, 0xE1, 0x00, 0x64, 'E', 'x', 'i', 'f', 0x00, 0x00}

	// A real encoder emits no EXIF; its output ends in SOS + entropy data.
	var realJPEG bytes.Buffer
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			src.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	if err := jpeg.Encode(&realJPEG, src, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		data []byte
		want int
	}{
		{
			name: "exif orientation 6",
			data: concat(soi, jfifApp0(), exif6, jpegSOS, []byte{0x00, 0x01, 0x02}),
			want: 6,
		},
		{
			name: "no exif",
			data: concat(soi, jfifApp0(), jpegSOS, []byte("entropy data")),
			want: 1,
		},
		{
			name: "xmp app1 before exif app1",
			data: concat(soi, xmp, exif6, jpegSOS),
			want: 6,
		},
		{
			name: "truncated exif segment",
			data: concat(soi, truncated),
			want: 1,
		},
		{
			name: "real jpeg no exif",
			data: realJPEG.Bytes(),
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReadOrientationFrom(bytes.NewReader(c.data)); got != c.want {
				t.Fatalf("ReadOrientationFrom = %d, want %d", got, c.want)
			}
		})
	}
}

// TestJPEGOrientationNotAtStart checks that a reader that has already consumed
// bytes falls back instead of walking from the wrong offset. Whatever goexif
// makes of the remainder, the contract is only "a value in [1,8], no panic".
func TestJPEGOrientationNotAtStart(t *testing.T) {
	data := concat([]byte{0xFF, 0xD8}, app1Segment(exifTIFFPayload(6)), jpegSOS)
	r := bytes.NewReader(data)
	if _, err := r.Read(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	got := ReadOrientationFrom(r)
	if got < 1 || got > 8 {
		t.Fatalf("ReadOrientationFrom after a partial read = %d, want 1..8", got)
	}
}

// BenchmarkJPEGOrientationNoExif measures the marker walk on a 4 MB JPEG-shaped
// buffer with no EXIF: the walk must stop at SOS instead of scanning the pixel
// payload byte by byte as goexif does.
func BenchmarkJPEGOrientationNoExif(b *testing.B) {
	data := make([]byte, 4<<20)
	copy(data, []byte{0xFF, 0xD8})
	n := copy(data[2:], jfifApp0())
	copy(data[2+n:], jpegSOS)
	r := bytes.NewReader(data)
	b.ReportAllocs()
	for b.Loop() {
		ReadOrientationFrom(r)
	}
}
