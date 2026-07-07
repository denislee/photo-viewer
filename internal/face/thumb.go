package face

import (
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

// FaceThumbSize is the longest edge of a cached face crop. 96px is large
// enough for a sidebar avatar without ballooning disk use.
const FaceThumbSize = 96

// ThumbPath returns the on-disk location of the cached crop for faceID,
// using the same first-2-hex-char sharding as cache.ThumbStore.
func ThumbPath(cacheDir string, faceID int64) string {
	id := fmt.Sprintf("%016x", faceID)
	return filepath.Join(cacheDir, "faces", id[:2], id+".jpg")
}

// EnsureThumb crops the face bbox out of srcThumb (the source-image
// thumbnail) and writes a small JPEG at the canonical face-thumb path.
// Returns the path. Idempotent: a cached file is reused if present.
//
// bbox is x, y, w, h in srcThumb pixel space (i.e. the same pixel space
// face_recognition reported).
func EnsureThumb(cacheDir, srcThumb string, faceID int64, bbox [4]int) (string, error) {
	dst := ThumbPath(cacheDir, faceID)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	in, err := os.Open(srcThumb)
	if err != nil {
		return "", err
	}
	defer in.Close()
	src, _, err := image.Decode(in)
	if err != nil {
		return "", err
	}

	x, y, w, h := bbox[0], bbox[1], bbox[2], bbox[3]
	if w <= 0 || h <= 0 {
		return "", errors.New("face: bbox has zero dimension")
	}
	rect := image.Rect(x, y, x+w, y+h).Intersect(src.Bounds())
	if rect.Empty() {
		return "", errors.New("face: bbox does not intersect source image")
	}

	// Scale straight from the sub-rect of src into the final RGBA — the
	// scaler accepts a source rectangle, so the intermediate "cropped"
	// RGBA the older code allocated and copied into was unnecessary.
	tw, th := fitWithin(rect.Dx(), rect.Dy(), FaceThumbSize)
	scaled := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), src, rect, draw.Src, nil)

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if err := jpeg.Encode(out, scaled, &jpeg.Options{Quality: 82}); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

func fitWithin(w, h, m int) (int, int) {
	if w <= m && h <= m {
		return w, h
	}
	if w >= h {
		return m, m * h / w
	}
	return m * w / h, m
}
