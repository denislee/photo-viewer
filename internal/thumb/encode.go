package thumb

import (
	"image"
	"image/jpeg"
	"io"
	"sync"

	"golang.org/x/image/draw"

	"github.com/dns/photo-viewer/internal/jpegturbo"
)

// EncodeJPEG writes img to w as a JPEG at the given quality. When img is an
// *image.RGBA and libjpeg-turbo was linked in, it encodes through
// jpegturbo.EncodeRGBA — roughly 10× faster than image/jpeg (T5-03). The
// libjpeg-turbo path is an optimisation only: any compression error falls
// through to image/jpeg, so callers get a working JPEG either way.
func EncodeJPEG(w io.Writer, img image.Image, quality int) error {
	if rgba, ok := img.(*image.RGBA); ok && jpegturbo.Available() {
		if data, err := jpegturbo.EncodeRGBA(rgba, quality); err == nil {
			_, err = w.Write(data)
			return err
		}
	}
	return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
}

// scalerKey identifies a cached CatmullRom scaler by the dimensions it was
// built for; the source and destination rectangles passed to Scale must match
// them for the cached scratch buffers to be the right size.
type scalerKey struct {
	dw, dh, sw, sh int
}

// scalers caches one draw.Scaler per size combination. draw.CatmullRom.Scale
// builds a fresh, un-pooled scaler on every call and allocates dw*sh*32 bytes
// of float64 scratch — 124 MB for a 2048 px display rendition (T5-04). A scaler
// from Kernel.NewScaler pools that scratch internally, so repeated same-size
// resamples (every display rendition of a 12 MP camera) stop reallocating it.
// The map is small and bounded; a Scaler is safe for concurrent use.
var (
	scalerMu sync.Mutex
	scalers  = make(map[scalerKey]draw.Scaler)
)

// ScaleCatmullRom scales the sr region of src into the dr region of dst with
// CatmullRom, reusing a cached scaler for the given size combination. It draws
// exactly what draw.CatmullRom.Scale(dst, dr, src, sr, draw.Src, nil) would.
func ScaleCatmullRom(dst *image.RGBA, dr image.Rectangle, src image.Image, sr image.Rectangle) {
	key := scalerKey{dw: dr.Dx(), dh: dr.Dy(), sw: sr.Dx(), sh: sr.Dy()}
	scalerMu.Lock()
	scaler, ok := scalers[key]
	if !ok {
		// Display renditions come in a handful of sizes; if the map grows past
		// that, drop it rather than hold scratch buffers for one-off sizes
		// (thumbnails are all one size, exports a few). Rebuilding costs one
		// allocation.
		if len(scalers) > 64 {
			scalers = make(map[scalerKey]draw.Scaler)
		}
		scaler = draw.CatmullRom.NewScaler(key.dw, key.dh, key.sw, key.sh)
		scalers[key] = scaler
	}
	scalerMu.Unlock()
	scaler.Scale(dst, dr, src, sr, draw.Src, nil)
}
