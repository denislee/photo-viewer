package thumb

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"sync"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// imageFfmpegThreshold is the source-size watershed above which we shell out
// to ffmpeg (which uses libjpeg-turbo's DCT-scaled decode for JPEGs and is
// dramatically faster on multi-MP files). Smaller images are handled by Go's
// in-process decoder to avoid the fork+exec overhead.
const imageFfmpegThreshold = 2 * 1024 * 1024

// Image decodes src, resamples it to fit within size×size, and writes a JPEG
// to dst. Aspect ratio is preserved.
func Image(ctx context.Context, src, dst string, size int) error {
	if info, err := os.Stat(src); err == nil && info.Size() >= imageFfmpegThreshold {
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			if err := imageViaFfmpeg(ctx, src, dst, size); err == nil {
				return nil
			}
			// fall through to the Go decoder on ffmpeg failure (e.g. format
			// it can't read); the in-process path is more permissive.
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	img, _, err := image.Decode(in)
	if err != nil {
		return err
	}
	return writeThumb(img, dst, size)
}

// imageViaFfmpeg uses ffmpeg's hardware-accelerated decoder + scale filter to
// produce a JPEG thumbnail directly, sidestepping a full pixel-buffer decode
// in Go. The scale filter matches the one used for video thumbs.
func imageViaFfmpeg(ctx context.Context, src, dst string, size int) error {
	vf := fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", size, size)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-frames:v", "1",
		"-vf", vf,
		"-f", "image2",
		dst,
	)
	if err := cmd.Run(); err != nil {
		os.Remove(dst)
		return err
	}
	if _, err := os.Stat(dst); err != nil {
		return err
	}
	return nil
}


// rgbaPool recycles the destination RGBA used by writeThumb. The thumbnail
// size is fixed at the package level (cache.ThumbSize) so essentially every
// buffer is reusable as-is; the slice length is reset before scaling and
// the original capacity preserved when returning to the pool.
var rgbaPool = sync.Pool{
	New: func() any { return &image.RGBA{} },
}

func writeThumb(src image.Image, dst string, size int) error {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	tw, th := fitWithin(w, h, size)
	thumb := rgbaPool.Get().(*image.RGBA)
	defer rgbaPool.Put(thumb)

	needed := 4 * tw * th
	if cap(thumb.Pix) < needed {
		thumb.Pix = make([]uint8, needed)
	} else {
		thumb.Pix = thumb.Pix[:needed]
	}
	thumb.Stride = 4 * tw
	thumb.Rect = image.Rect(0, 0, tw, th)
	// draw.Src overwrites every destination pixel, so reusing the buffer
	// without zeroing is safe.
	draw.ApproxBiLinear.Scale(thumb, thumb.Rect, src, b, draw.Src, nil)

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	return jpeg.Encode(out, thumb, &jpeg.Options{Quality: 82})
}

func fitWithin(w, h, max int) (int, int) {
	if w <= max && h <= max {
		return w, h
	}
	if w >= h {
		return max, max * h / w
	}
	return max * w / h, max
}
