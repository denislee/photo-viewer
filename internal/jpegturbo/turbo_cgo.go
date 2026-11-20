//go:build cgo && !nojpegturbo

package jpegturbo

/*
#cgo pkg-config: libturbojpeg
#include <stdlib.h>
#include <turbojpeg.h>

// tj3Init is a macro over tj3InitVersion, which cgo can't call directly.
static tjhandle pv_tj_init_decompress(void) {
	return tj3Init(TJINIT_DECOMPRESS);
}

static tjhandle pv_tj_init_compress(void) {
	return tj3Init(TJINIT_COMPRESS);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"sync"
	"unsafe"
)

const available = true

// scalingFactors is libjpeg-turbo's list of supported DCT scaling factors,
// filtered to those ≤ 1 (the library also offers upscaling, which we never use).
var scalingFactors = sync.OnceValue(func() [][2]int {
	var n C.int
	p := C.tj3GetScalingFactors(&n)
	if p == nil || n <= 0 {
		return [][2]int{{1, 2}, {1, 4}, {1, 8}}
	}
	sfs := unsafe.Slice(p, int(n))
	out := make([][2]int, 0, len(sfs))
	for _, sf := range sfs {
		num, denom := int(sf.num), int(sf.denom)
		if num > 0 && denom > 0 && num <= denom {
			out = append(out, [2]int{num, denom})
		}
	}
	return out
})

// handles keeps idle TurboJPEG decompressor handles for reuse. Creating one is
// cheap but not free (it allocates libjpeg's state), and every thumbnail worker
// decodes in a loop. A bounded channel rather than a sync.Pool, because a pool
// may drop items at GC without running tj3Destroy, which would leak C memory.
var handles = make(chan C.tjhandle, 64)

func getHandle() (C.tjhandle, error) {
	select {
	case h := <-handles:
		return h, nil
	default:
	}
	h := C.pv_tj_init_decompress()
	if h == nil {
		return nil, errors.New("jpegturbo: tj3Init failed")
	}
	return h, nil
}

func putHandle(h C.tjhandle) {
	select {
	case handles <- h:
	default:
		C.tj3Destroy(h)
	}
}

// chandles keeps idle TurboJPEG compressor handles for reuse, mirroring
// handles for decompressors. Small pool: encoding runs inline on the thumbnail
// and export paths rather than under a large worker fan-out.
var chandles = make(chan C.tjhandle, 16)

func getCHandle() (C.tjhandle, error) {
	select {
	case h := <-chandles:
		return h, nil
	default:
	}
	h := C.pv_tj_init_compress()
	if h == nil {
		return nil, errors.New("jpegturbo: tj3Init failed")
	}
	return h, nil
}

func putCHandle(h C.tjhandle) {
	select {
	case chandles <- h:
	default:
		C.tj3Destroy(h)
	}
}

func tjError(h C.tjhandle, what string) error {
	return fmt.Errorf("jpegturbo: %s: %s", what, C.GoString(C.tj3GetErrorStr(h)))
}

// header parses the JPEG header into h and returns the stored size.
func header(h C.tjhandle, data []byte) (int, int, error) {
	if len(data) == 0 {
		return 0, 0, errors.New("jpegturbo: empty input")
	}
	if C.tj3DecompressHeader(h, (*C.uchar)(unsafe.Pointer(&data[0])), C.size_t(len(data))) != 0 {
		return 0, 0, tjError(h, "header")
	}
	w := int(C.tj3Get(h, C.TJPARAM_JPEGWIDTH))
	ht := int(C.tj3Get(h, C.TJPARAM_JPEGHEIGHT))
	if w <= 0 || ht <= 0 {
		return 0, 0, errors.New("jpegturbo: invalid dimensions")
	}
	return w, ht, nil
}

func decodeFit(data []byte, fit func(w, h int) (minW, minH int)) (*image.RGBA, Config, error) {
	var cfg Config
	if !IsJPEG(data) {
		return nil, cfg, errors.New("jpegturbo: not a JPEG")
	}
	h, err := getHandle()
	if err != nil {
		return nil, cfg, err
	}
	defer putHandle(h)

	w, ht, err := header(h, data)
	if err != nil {
		return nil, cfg, err
	}
	cfg = Config{Width: w, Height: ht}
	// libjpeg-turbo can't convert CMYK/YCCK to RGB; let image/jpeg handle it.
	if cs := C.tj3Get(h, C.TJPARAM_COLORSPACE); cs == C.TJCS_CMYK || cs == C.TJCS_YCCK {
		return nil, cfg, errors.New("jpegturbo: CMYK JPEG")
	}
	minW, minH := 0, 0
	if fit != nil {
		minW, minH = fit(w, ht)
	}
	num, denom := pickScale(scalingFactors(), w, ht, minW, minH)
	sf := C.tjscalingfactor{num: C.int(num), denom: C.int(denom)}
	if C.tj3SetScalingFactor(h, sf) != 0 {
		return nil, cfg, tjError(h, "scaling factor")
	}
	ow, oh := scaledSize(w, num, denom), scaledSize(ht, num, denom)

	img := image.NewRGBA(image.Rect(0, 0, ow, oh))
	// Passing Go memory is fine here: neither buffer contains Go pointers, and
	// libjpeg-turbo doesn't keep them past the call.
	rc := C.tj3Decompress8(h,
		(*C.uchar)(unsafe.Pointer(&data[0])), C.size_t(len(data)),
		(*C.uchar)(unsafe.Pointer(&img.Pix[0])), C.int(img.Stride), C.TJPF_RGBA)
	if rc != 0 && C.tj3GetErrorCode(h) != C.TJERR_WARNING {
		return nil, cfg, tjError(h, "decompress")
	}
	// A TJERR_WARNING (for example a truncated scan) still produces a usable
	// image, as image/jpeg would reject it; keep it, like most viewers do.
	return img, cfg, nil
}

// encodeRGBA compresses img with 4:2:0 chroma subsampling at quality (clamped
// to 1..100). The output buffer is allocated by libjpeg-turbo itself because a
// NULL *jpegBuf asks it to (TJPARAM_NOREALLOC stays at its default of 0), which
// avoids a Go-side guess at the compressed size.
func encodeRGBA(img *image.RGBA, quality int) ([]byte, error) {
	b := img.Bounds()
	w, hgt := b.Dx(), b.Dy()
	if w <= 0 || hgt <= 0 {
		return nil, errors.New("jpegturbo: empty image")
	}
	q := max(min(quality, 100), 1)
	h, err := getCHandle()
	if err != nil {
		return nil, err
	}
	defer putCHandle(h)

	if C.tj3Set(h, C.TJPARAM_QUALITY, C.int(q)) != 0 {
		return nil, tjError(h, "quality")
	}
	if C.tj3Set(h, C.TJPARAM_SUBSAMP, C.TJSAMP_420) != 0 {
		return nil, tjError(h, "subsampling")
	}
	var buf *C.uchar
	var size C.size_t
	// libjpeg-turbo neither retains the source buffer nor sees Go pointers in
	// it, so handing it the sub-image's first pixel is safe.
	rc := C.tj3Compress8(h,
		(*C.uchar)(unsafe.Pointer(&img.Pix[img.PixOffset(b.Min.X, b.Min.Y)])),
		C.int(w), C.int(img.Stride), C.int(hgt), C.TJPF_RGBA, &buf, &size)
	if rc != 0 {
		if buf != nil {
			C.tj3Free(unsafe.Pointer(buf))
		}
		return nil, tjError(h, "compress")
	}
	out := C.GoBytes(unsafe.Pointer(buf), C.int(size))
	C.tj3Free(unsafe.Pointer(buf))
	return out, nil
}
