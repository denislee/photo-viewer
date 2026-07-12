package export

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/imgorient"
	"github.com/dns/photo-viewer/internal/scan"
)

// recompressOutputPath rewrites dst's extension when the encoder we're
// about to use produces a different container/format than the source. For
// images we always emit JPEG; for videos we always emit MP4 (H.264).
func recompressOutputPath(dst string, t scan.MediaType) string {
	switch t {
	case scan.TypePhoto, scan.TypeHEIC:
		ext := strings.ToLower(filepath.Ext(dst))
		if ext == ".jpg" || ext == ".jpeg" {
			return dst
		}
		return strings.TrimSuffix(dst, filepath.Ext(dst)) + ".jpg"
	case scan.TypeVideo:
		if strings.EqualFold(filepath.Ext(dst), ".mp4") {
			return dst
		}
		return strings.TrimSuffix(dst, filepath.Ext(dst)) + ".mp4"
	}
	return dst
}

// recompressFile re-encodes src into dst at a reduced size. Returns true if
// recompression was attempted (success or hard failure); false means the
// caller should fall back to a plain copy. RAW files always fall back to
// copy because lossy re-encoding RAW defeats the point of keeping it.
func recompressFile(ctx context.Context, t scan.MediaType, src, dst string, maxEdge, jpegQuality, videoCRF int) (bool, error) {
	switch t {
	case scan.TypePhoto:
		return true, recompressImage(src, dst, maxEdge, jpegQuality)
	case scan.TypeHEIC:
		if _, err := exec.LookPath("heif-convert"); err != nil {
			return false, nil
		}
		return true, recompressHEIC(ctx, src, dst, maxEdge, jpegQuality)
	case scan.TypeVideo:
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return false, nil
		}
		return true, recompressVideo(ctx, src, dst, maxEdge, videoCRF)
	}
	return false, nil
}

// recompressImage decodes any image format supported by image.Decode (plus
// the x/image additions: webp, bmp, tiff), resizes it so the longest edge
// is at most maxEdge (no upscaling), and writes a JPEG at the given
// quality.
func recompressImage(src, dst string, maxEdge, quality int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	img, _, err := image.Decode(in)
	if err != nil {
		return err
	}
	// Go's image decoders ignore the EXIF Orientation tag and jpeg.Encode
	// writes no EXIF, so a portrait camera JPEG (Orientation != 1) would
	// otherwise export with sideways pixels AND no tag left to correct it —
	// permanently rotated. Bake the orientation into the pixels so the output
	// JPEG is upright with no orientation tag needed. (The plain non-recompress
	// copy path keeps the original bytes incl. the EXIF tag, so it doesn't need
	// this.) Orient *after* the downscale (S-04): rotation commutes with
	// scaling, so the remap runs on the small output, not the full-res source.
	// fitWithin is fed the stored dims; for the transpose orientations (5–8) the
	// output's axes swap, both staying ≤ maxEdge.
	orient := imgorient.ReadOrientation(src)
	b := img.Bounds()
	tw, th := fitWithin(b.Dx(), b.Dy(), maxEdge)
	scaled := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(scaled, scaled.Rect, img, b, draw.Src, nil)
	dstImg := imgorient.Apply(scaled, orient)

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, dstImg, &jpeg.Options{Quality: quality}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	// fsync before the rename so the encoded JPEG is on stable storage, not just
	// in the page cache, before it becomes visible at dst. A failed Sync is the
	// "not yet durable" case, so treat it as an encode failure and drop the temp.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// recompressHEIC converts the HEIF source to a temp JPEG via heif-convert
// (the same tool used for thumbnails) and then resamples that with
// recompressImage. The intermediate JPEG is heif-convert's max-quality
// output, so quality loss is dominated by the final encode.
func recompressHEIC(ctx context.Context, src, dst string, maxEdge, quality int) error {
	tmpFull, err := os.CreateTemp("", "pv-export-*.jpg")
	if err != nil {
		return err
	}
	tmpFull.Close()
	defer os.Remove(tmpFull.Name())

	cmd := exec.CommandContext(ctx, "heif-convert", "-q", "95", src, tmpFull.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("heif-convert: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return recompressImage(tmpFull.Name(), dst, maxEdge, quality)
}

// recompressVideo invokes ffmpeg to transcode src into dst, fitting the
// frame inside maxEdge × maxEdge (aspect preserved, never upscaled) and
// using libx264 at the given CRF for the video stream. Audio is re-encoded
// to AAC at 128 kbps so MOV/MTS sources still produce a portable MP4.
func recompressVideo(ctx context.Context, src, dst string, maxEdge, crf int) error {
	// force_original_aspect_ratio=decrease keeps the source dims when they
	// already fit; otherwise scales down to fit inside maxEdge×maxEdge.
	// The trailing -2 rounding ensures both axes stay even (required by
	// libx264's chroma subsampling).
	vf := fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2", maxEdge, maxEdge)
	tmp := dst + ".tmp.mp4"
	args := []string{
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", fmt.Sprintf("%d", crf),
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		tmp,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dst)
}

func fitWithin(w, h, maxEdge int) (int, int) {
	if maxEdge <= 0 || (w <= maxEdge && h <= maxEdge) {
		return w, h
	}
	if w >= h {
		return maxEdge, maxEdge * h / w
	}
	return maxEdge * w / h, maxEdge
}
