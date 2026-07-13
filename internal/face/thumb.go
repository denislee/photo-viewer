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

	"github.com/dns/photo-viewer/internal/imgfit"
)

// FaceThumbSize is the longest edge of a cached face crop. 96px is large
// enough for a sidebar avatar without ballooning disk use.
const FaceThumbSize = 96

// FaceCrop names one face crop to materialize: the face-row ID (which names
// the output file, via ThumbPath) and its bbox in srcThumb pixel space
// (x, y, w, h — the same pixel space face_recognition reported).
type FaceCrop struct {
	ID   int64
	BBox [4]int
}

// decodeSourceHook is a test-only seam (see thumb_test.go) invoked once per
// actual source decode, letting a test assert that EnsureThumbs decodes the
// source thumbnail exactly once no matter how many faces it crops (S-15).
var decodeSourceHook func(srcThumb string)

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
//
// It is a single-face convenience wrapper around EnsureThumbs; prefer
// EnsureThumbs when materializing several faces of the same source thumb so
// the source JPEG is decoded only once.
func EnsureThumb(cacheDir, srcThumb string, faceID int64, bbox [4]int) (string, error) {
	paths, errs := EnsureThumbs(cacheDir, srcThumb, []FaceCrop{{ID: faceID, BBox: bbox}})
	return paths[0], errs[0]
}

// EnsureThumbs materializes the face crops for every entry in faces, decoding
// srcThumb at most once regardless of how many faces are requested. A group
// photo with N faces crops N times from a single decode instead of re-decoding
// the same JPEG N times (S-15).
//
// The returned slices run parallel to faces: paths[i] and errs[i] describe
// faces[i]. On success paths[i] is the canonical crop path (ThumbPath) and
// errs[i] is nil; on failure paths[i] is "" and errs[i] holds the error. Each
// crop is written lazily and atomically (tmp + rename) with the exact geometry
// and encoding of the single-face path — an already-cached crop is reused
// without touching the source, so if every face is already cached the source
// thumbnail is never opened or decoded.
func EnsureThumbs(cacheDir, srcThumb string, faces []FaceCrop) ([]string, []error) {
	paths := make([]string, len(faces))
	errs := make([]error, len(faces))

	// First pass: resolve already-cached crops and collect the ones still to
	// build. Decoding is deferred until we know at least one crop is missing,
	// preserving EnsureThumb's laziness (a fully-cached source is never opened).
	pending := make([]int, 0, len(faces))
	for i := range faces {
		dst := ThumbPath(cacheDir, faces[i].ID)
		paths[i] = dst
		if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
			continue
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 {
		return paths, errs
	}

	src, err := decodeSource(srcThumb)
	if err != nil {
		for _, i := range pending {
			paths[i] = ""
			errs[i] = err
		}
		return paths, errs
	}

	for _, i := range pending {
		if err := writeCrop(paths[i], src, faces[i].BBox); err != nil {
			paths[i] = ""
			errs[i] = err
		}
	}
	return paths, errs
}

// decodeSource opens and decodes the source thumbnail. It is the single point
// through which EnsureThumbs decodes, so decodeSourceHook counts real decodes.
func decodeSource(srcThumb string) (image.Image, error) {
	if decodeSourceHook != nil {
		decodeSourceHook(srcThumb)
	}
	in, err := os.Open(srcThumb)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	src, _, err := image.Decode(in)
	if err != nil {
		return nil, err
	}
	return src, nil
}

// writeCrop scales the bbox sub-rect of src into a face-thumb-sized RGBA and
// writes it atomically to dst. This is the exact per-face body the single-face
// path used before batching, so crop geometry and encoding are unchanged.
func writeCrop(dst string, src image.Image, bbox [4]int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	x, y, w, h := bbox[0], bbox[1], bbox[2], bbox[3]
	if w <= 0 || h <= 0 {
		return errors.New("face: bbox has zero dimension")
	}
	rect := image.Rect(x, y, x+w, y+h).Intersect(src.Bounds())
	if rect.Empty() {
		return errors.New("face: bbox does not intersect source image")
	}

	// Scale straight from the sub-rect of src into the final RGBA — the
	// scaler accepts a source rectangle, so the intermediate "cropped"
	// RGBA the older code allocated and copied into was unnecessary.
	tw, th := imgfit.Within(rect.Dx(), rect.Dy(), FaceThumbSize)
	scaled := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), src, rect, draw.Src, nil)

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, scaled, &jpeg.Options{Quality: 82}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
