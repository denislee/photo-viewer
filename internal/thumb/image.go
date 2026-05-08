package thumb
import (
	"context"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Image decodes src, resamples it to fit within size×size, and writes a JPEG
// to dst. Aspect ratio is preserved.
func Image(ctx context.Context, src, dst string, size int) error {
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


func writeThumb(src image.Image, dst string, size int) error {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	tw, th := fitWithin(w, h, size)
	thumb := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(thumb, thumb.Bounds(), src, b, draw.Over, nil)
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
