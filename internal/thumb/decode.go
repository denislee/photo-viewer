package thumb

import (
	"bytes"
	"context"
	"image"
	"io"
	"os"

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
	if jpegturbo.Available() && jpegturbo.IsJPEG(data) {
		if img, err := jpegturbo.Decode(data, minW, minH); err == nil {
			return img, nil
		}
	}
	img, _, err := image.Decode(NewContextReader(ctx, bytes.NewReader(data)))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	return img, nil
}

// decodeFit decodes data just large enough to be resampled to fit a size×size
// box: the DCT scale only ever reduces to the fitted size, never below it.
func decodeFit(ctx context.Context, data []byte, size int) (image.Image, error) {
	minW, minH := 0, 0
	if jpegturbo.Available() && jpegturbo.IsJPEG(data) {
		if cfg, err := jpegturbo.DecodeConfig(data); err == nil {
			minW, minH = imgfit.Within(cfg.Width, cfg.Height, size)
		}
	}
	return DecodeScaled(ctx, data, minW, minH)
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
		buf.Grow(int(info.Size()))
	}
	buf.Write(head[:])
	if _, err := buf.ReadFrom(NewContextReader(ctx, f)); err != nil {
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
		buf.Grow(int(info.Size()))
	}
	if _, err := buf.ReadFrom(NewContextReader(ctx, f)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// contextReader fails reads with ctx.Err() once ctx is done. Go's image
// decoders pull their input in small chunks, so wrapping their source in one
// turns a cancelled context into an early decode error.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

// NewContextReader wraps r so reads fail once ctx is cancelled.
func NewContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &contextReader{ctx: ctx, r: r}
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
