package thumb

import (
	"bytes"
	"context"
	"image"
	"io"
	"os"

	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/imgfit"
	"github.com/dns/photo-viewer/internal/jpegturbo"
)

// DecodeScaled decodes an in-memory image. A JPEG goes through libjpeg-turbo
// with the strongest DCT downscale that keeps the result at least minW×minH
// (stored orientation; zero means full size), which is several times faster
// than image/jpeg and returns an *image.RGBA ready for Gio. Anything else, or a
// JPEG libjpeg-turbo can't handle (CMYK, built with nojpegturbo), falls back to
// image.Decode at full size, reading through a context-aware reader so a
// cancelled ctx aborts the decode early instead of running it to completion.
func DecodeScaled(ctx context.Context, data []byte, minW, minH int) (image.Image, error) {
	img, _, err := DecodeFitFunc(ctx, data, func(int, int) (int, int) { return minW, minH })
	return img, err
}

// DecodeFitFunc is DecodeScaled with the DCT minimum chosen by fit from the
// JPEG's stored size, in one header parse (jpegturbo.DecodeFit). cfg is the
// stored size whenever the JPEG header could be read (also when libjpeg-turbo
// then failed and image.Decode took over), zero otherwise.
func DecodeFitFunc(ctx context.Context, data []byte, fit func(w, h int) (minW, minH int)) (image.Image, jpegturbo.Config, error) {
	var cfg jpegturbo.Config
	if jpegturbo.Available() && jpegturbo.IsJPEG(data) {
		img, c, err := jpegturbo.DecodeFit(data, fit)
		if err == nil {
			return img, c, nil
		}
		cfg = c
	}
	img, _, err := image.Decode(fsutil.NewContextReader(ctx, bytes.NewReader(data)))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cfg, cerr
		}
		return nil, cfg, err
	}
	return img, cfg, nil
}

// DecodeFit decodes data just large enough to be resampled to fit a size×size
// box: the DCT scale only ever reduces to the fitted size, never below it.
func DecodeFit(ctx context.Context, data []byte, size int) (image.Image, error) {
	img, _, err := DecodeFitFunc(ctx, data, func(w, h int) (int, int) { return imgfit.Within(w, h, size) })
	return img, err
}

// readJPEG returns src's contents when it starts with a JPEG SOI marker, and
// (nil, nil) for any other format so the caller can take its usual path.
func readJPEG(ctx context.Context, src string) ([]byte, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var head [3]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || !jpegturbo.IsJPEG(head[:]) {
		return nil, nil
	}
	var buf bytes.Buffer
	if info, err := f.Stat(); err == nil {
		// +MinRead: ReadFrom grows the buffer by MinRead once it is exactly
		// full, before it sees EOF; without the slack that reallocation
		// doubles the buffer and copies the whole file (T5-02).
		buf.Grow(int(info.Size()) + bytes.MinRead)
	}
	buf.Write(head[:])
	if _, err := buf.ReadFrom(fsutil.NewContextReader(ctx, f)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadFile reads path fully, aborting with ctx.Err() once ctx is cancelled.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if info, err := f.Stat(); err == nil {
		// +MinRead: ReadFrom grows the buffer by MinRead once it is exactly
		// full, before it sees EOF; without the slack that reallocation
		// doubles the buffer and copies the whole file (T5-02).
		buf.Grow(int(info.Size()) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(fsutil.NewContextReader(ctx, f)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
