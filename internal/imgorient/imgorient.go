// Package imgorient reads the EXIF Orientation tag from an image file and
// bakes that orientation into the pixels of a decoded image. It exists because
// Go's standard image decoders (image/jpeg, and the x/image codecs) ignore the
// EXIF Orientation tag entirely: they hand back the pixels exactly as stored,
// so a portrait photo shot on a camera that recorded "rotate 90°" in EXIF
// decodes sideways. Anything that re-encodes such an image (export
// recompression and thumbnail generation) and does NOT copy the original EXIF
// forward would otherwise produce a permanently-rotated file with no tag left
// to correct it.
//
// The package is intentionally standalone and dependency-light (image + goexif
// only) so it can be reused wherever a decoded image needs to be made upright.
package imgorient

import (
	"encoding/binary"
	"image"
	"image/draw"
	"io"
	"os"
	"sync"

	"github.com/rwcarlsen/goexif/exif"
)

// ReadOrientation returns the EXIF Orientation of the image at path as a value
// in [1, 8]. It returns 1 (the "already upright, no transform" value) whenever
// the file can't be opened, has no EXIF, has no Orientation tag, or carries an
// out-of-range value — so callers can always treat the result as a safe no-op
// default and never need to handle an error for the common "no EXIF" case.
func ReadOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	return ReadOrientationFrom(f)
}

// ReadOrientationFrom is ReadOrientation for an already-open (or in-memory)
// image stream, so a caller that has the file's bytes loaded doesn't reopen it.
// The same "1 on any failure" contract applies.
//
// A TIFF-structured file (CR2, NEF, ARW, DNG, PEF, TIFF: "II*\0" / "MM\0*")
// read through an io.ReaderAt positioned at its start (*os.File and
// *bytes.Reader both qualify) has its IFD0 Orientation parsed directly (T4-01):
// goexif would buffer the whole stream twice to reach the same 12-byte entry,
// which on a 36 MB RAW costs ~76 ms and ~300 MB of allocation per call.
// Everything else goes through exif.Decode.
//
// A JPEG is different (H5-01): goexif finds APP1 by scanning the stream byte by
// byte, which costs ~17 ms and ~15.6 MB of allocation on a 14 MB JPEG with no
// EXIF at all, and it stops at the *first* APP1 — so an XMP block before the
// EXIF one makes it read no orientation. When r is an io.ReaderAt at offset 0,
// jpegOrientation walks the marker segments and parses the EXIF APP1's TIFF
// header in place instead; goexif is never used for a JPEG reachable that way.
func ReadOrientationFrom(r io.Reader) int {
	if ra, ok := readerAtStart(r); ok {
		if o, ok := jpegOrientation(ra); ok {
			return o
		}
		if o, ok := tiffOrientation(ra); ok {
			return o
		}
	}
	x, err := exif.Decode(r)
	if err != nil {
		return 1
	}
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return 1
	}
	o, err := tag.Int(0)
	if err != nil || o < 1 || o > 8 {
		return 1
	}
	return o
}

// readerAtStart returns r as an io.ReaderAt when the stream is positioned at
// its start, where ReadAt's absolute offsets match what exif.Decode would read.
// A non-ReaderAt, or a seeker past the start, returns false so the callers fall
// back to goexif. Checked once here rather than in each format probe, so a TIFF
// read pays one lseek, not two.
func readerAtStart(r io.Reader) (io.ReaderAt, bool) {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		return nil, false
	}
	if s, ok := r.(io.Seeker); ok {
		if pos, err := s.Seek(0, io.SeekCurrent); err != nil || pos != 0 {
			return nil, false
		}
	}
	return ra, true
}

// orientationTag is the EXIF/TIFF Orientation tag; tiffShort is the SHORT
// field type it is stored as.
const (
	orientationTag = 0x0112
	tiffShort      = 3
)

// tiffOrientation reads the Orientation entry of IFD0 straight from a TIFF
// header. ok is false when ra does not start with TIFF magic, in which case the
// caller falls back to goexif. Once the magic matches, every failure (truncated
// IFD, absent tag, wrong type, out-of-range value) yields (1, true): the file is
// TIFF and goexif would read the same IFD0.
func tiffOrientation(ra io.ReaderAt) (int, bool) {
	// One buffer serves the header, the entry count and the entry blocks: it
	// escapes through the ReaderAt interface, so this is the only allocation.
	var buf [64 * 12]byte
	head := buf[:8]
	if _, err := ra.ReadAt(head, 0); err != nil {
		return 0, false
	}
	var bo binary.ByteOrder
	switch {
	case head[0] == 'I' && head[1] == 'I' && head[2] == 42 && head[3] == 0:
		bo = binary.LittleEndian
	case head[0] == 'M' && head[1] == 'M' && head[2] == 0 && head[3] == 42:
		bo = binary.BigEndian
	default:
		return 0, false
	}
	off := int64(bo.Uint32(head[4:]))
	if _, err := ra.ReadAt(buf[:2], off); err != nil {
		return 1, true
	}
	n := int(bo.Uint16(buf[:2]))
	off += 2
	// Scan the n 12-byte entries in fixed-size blocks so a corrupt count can't
	// force a large allocation.
	for n > 0 {
		k := min(n, len(buf)/12)
		got, _ := ra.ReadAt(buf[:k*12], off)
		for e := buf[:got/12*12]; len(e) >= 12; e = e[12:] {
			if bo.Uint16(e[0:]) != orientationTag {
				continue
			}
			if bo.Uint16(e[2:]) != tiffShort {
				return 1, true
			}
			if o := int(bo.Uint16(e[8:])); o >= 1 && o <= 8 {
				return o, true
			}
			return 1, true
		}
		if got < k*12 {
			return 1, true // truncated IFD
		}
		n -= k
		off += int64(k * 12)
	}
	return 1, true
}

// jpegScratchPool holds the small buffer jpegOrientation reads its SOI marker,
// segment markers, lengths and APP1 identifier into. A stack array would be
// moved to the heap because it reaches io.ReaderAt through an interface, adding
// an allocation to every orientation read — including the TIFF fast path, which
// is guarded to a single fixed buffer (TestTIFFOrientationAllocs). The pool
// keeps the marker walk allocation-free after the first call.
var jpegScratchPool = sync.Pool{New: func() any { return new([6]byte) }}

// jpegOrientation walks the JPEG marker segments of ra looking for the EXIF APP1
// block and parses its embedded TIFF header directly (H5-01). ok is false when ra
// does not start with SOI (FF D8), in which case the caller falls back to
// tiffOrientation/goexif. Once SOI matches, every failure (truncated segment, no
// EXIF, absent Orientation tag, out-of-range value) yields (1, true): the file
// is a JPEG, and 1 is what goexif would produce for the same bytes. Skipping
// non-EXIF APP1 blocks (XMP) is the point: goexif stops at the first APP1 and
// would miss an EXIF block that follows one.
func jpegOrientation(ra io.ReaderAt) (int, bool) {
	bp := jpegScratchPool.Get().(*[6]byte)
	defer jpegScratchPool.Put(bp)
	buf := bp[:]
	soi := buf[:2]
	if _, err := ra.ReadAt(soi, 0); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 0, false
	}
	off := int64(2)
	// Cap the walk so a stream of tiny bogus segments can't spin forever.
	for range 1024 {
		mb := buf[:1]
		if _, err := ra.ReadAt(mb, off); err != nil || mb[0] != 0xFF {
			return 1, true
		}
		// Skip any run of 0xFF fill bytes; the byte after them is the marker.
		for mb[0] == 0xFF {
			off++
			if _, err := ra.ReadAt(mb, off); err != nil {
				return 1, true
			}
		}
		// markerPos is the 0xFF that starts the segment; the marker byte is at
		// markerPos+1 and, when present, the 2-byte length at markerPos+2.
		// marker must be captured before reusing the shared scratch buffer.
		markerPos := off - 1
		marker := mb[0]
		switch {
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			// TEM and RSTn carry no length field.
			off = markerPos + 2
			continue
		case marker == 0xD9 || marker == 0xDA:
			// EOI / start of scan: no more metadata segments.
			return 1, true
		}
		lb := buf[:2]
		if _, err := ra.ReadAt(lb, off+1); err != nil {
			return 1, true
		}
		l := int(binary.BigEndian.Uint16(lb))
		if l < 2 {
			return 1, true
		}
		if marker == 0xE1 && l >= 8 {
			id := buf[:6]
			if _, err := ra.ReadAt(id, markerPos+4); err == nil && string(id) == "Exif\x00\x00" {
				// Payload after the length is "<ident><TIFF header>": skip the
				// 6-byte identifier and hand the TIFF section to the same
				// parser the RAW path uses.
				if o, ok := tiffOrientation(io.NewSectionReader(ra, markerPos+4+6, int64(l)-8)); ok {
					return o, true
				}
				return 1, true
			}
		}
		off = markerPos + 2 + int64(l)
	}
	return 1, true
}

// Apply returns img transformed so its pixels are upright for the given EXIF
// orientation. Orientation 1 (and any out-of-range value) is the identity and
// returns img unchanged with no allocation. The four "transpose" orientations
// (5–8) swap width and height, so the returned image's bounds differ from the
// input's for those; the caller should re-read Bounds() afterwards.
//
// The eight EXIF orientation values and the transform each one needs to become
// upright:
//
//	1 = identity
//	2 = mirror horizontal
//	3 = rotate 180°
//	4 = mirror vertical
//	5 = transpose      (reflect across the main / top-left→bottom-right diagonal)
//	6 = rotate 90° CW
//	7 = transverse     (reflect across the anti-diagonal)
//	8 = rotate 90° CCW
//
// An *image.NRGBA (from the PNG decoder) is remapped in place through its Pix
// slice and stays non-premultiplied. Every other image is first made an
// *image.RGBA (an RGBA is used as is; anything else is converted with ToRGBA,
// which takes image/draw's vectorised YCbCr/Gray paths and is exact for every
// other type) and then remapped the same way. Both are O(w·h); callers should
// orient *after* downscaling so the remap runs at output resolution, not source
// resolution.
func Apply(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if src, ok := img.(*image.NRGBA); ok {
		dst := image.NewNRGBA(orientedRect(b, orientation))
		remapPix(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, orientation)
		return dst
	}
	src, ok := img.(*image.RGBA)
	if !ok {
		src = ToRGBA(img)
	}
	dst := image.NewRGBA(orientedRect(b, orientation))
	remapPix(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, orientation)
	return dst
}

// ToRGBA returns img as an origin-anchored *image.RGBA. An RGBA that already
// starts at the origin is returned as is; anything else is converted with
// draw.Src, which has fast paths for the YCbCr and Gray images the JPEG decoder
// returns. Gio's paint.NewImageOp uploads an origin-anchored *image.RGBA without
// copying it, so converting once here also saves that copy.
func ToRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Rect, img, b.Min, draw.Src)
	return dst
}

// orientedRect returns the origin-anchored bounds of the upright image an
// orientation produces from a source with bounds b. Orientations 5–8 reflect
// across a diagonal, swapping the axes.
func orientedRect(b image.Rectangle, orientation int) image.Rectangle {
	if orientation >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}

// remapTile is the edge of the square tiles remapPix walks for the transpose
// orientations: 32×32 RGBA pixels is 4 KiB per tile on each side, so a tile's
// source rows and destination columns both stay in L1.
const remapTile = 32

// remapPix copies 4-byte pixels from a packed source (spix/sstride, the top-left
// pixel at spix[0]) into a packed destination (dpix/dstride, sized per
// orientedRect and origin-anchored) under the given orientation. It handles the
// RGBA and NRGBA cases identically because both share the same 4-byte layout;
// copying raw bytes preserves the exact colour, so this is a pure spatial remap.
//
// Every orientation is affine in the source coordinates: source pixel (x, y)
// lands at byte base + x·xstep + y·ystep of dpix. The steps are derived once
// (H4-02) instead of switching per pixel; orientations that keep rows
// left-to-right (1, 4) copy whole rows, and the rest walk remapTile-square
// tiles so the column-wise destination writes of a transpose stay cache-local.
func remapPix(spix []uint8, sstride int, dpix []uint8, dstride, w, h, orientation int) {
	if w <= 0 || h <= 0 {
		return
	}
	base, xstep, ystep := 0, 4, dstride
	switch orientation {
	case 2: // mirror horizontal: (w-1-x, y)
		base, xstep, ystep = (w-1)*4, -4, dstride
	case 3: // rotate 180°: (w-1-x, h-1-y)
		base, xstep, ystep = (h-1)*dstride+(w-1)*4, -4, -dstride
	case 4: // mirror vertical: (x, h-1-y)
		base, xstep, ystep = (h-1)*dstride, 4, -dstride
	case 5: // transpose: (y, x)
		base, xstep, ystep = 0, dstride, 4
	case 6: // rotate 90° CW: (h-1-y, x)
		base, xstep, ystep = (h-1)*4, dstride, -4
	case 7: // transverse: (h-1-y, w-1-x)
		base, xstep, ystep = (w-1)*dstride+(h-1)*4, -dstride, -4
	case 8: // rotate 90° CCW: (y, w-1-x)
		base, xstep, ystep = (w-1)*dstride, -dstride, 4
	}
	if xstep == 4 {
		for y := range h {
			di := base + y*ystep
			copy(dpix[di:di+4*w], spix[y*sstride:y*sstride+4*w])
		}
		return
	}
	for ty := 0; ty < h; ty += remapTile {
		yEnd := min(ty+remapTile, h)
		for tx := 0; tx < w; tx += remapTile {
			xEnd := min(tx+remapTile, w)
			for y := ty; y < yEnd; y++ {
				so := y*sstride + tx*4
				di := base + tx*xstep + y*ystep
				for x := tx; x < xEnd; x++ {
					copy(dpix[di:di+4], spix[so:so+4])
					so += 4
					di += xstep
				}
			}
		}
	}
}
