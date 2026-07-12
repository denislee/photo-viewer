package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"strings"
	"sync"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/imgorient"
)

// imageFfmpegThreshold is the source-size watershed above which we shell out
// to ffmpeg (which uses libjpeg-turbo's DCT-scaled decode for JPEGs and is
// dramatically faster on multi-MP files). Smaller images are handled by Go's
// in-process decoder to avoid the fork+exec overhead.
const imageFfmpegThreshold = 2 * 1024 * 1024

// External-tool availability is probed once via exec.LookPath and the result
// cached: LookPath stats every $PATH entry, and the thumbnail pipeline runs
// these guards on every file, so a 50k-file warm-up would otherwise waste
// hundreds of thousands of stats. Tool presence can't change meaningfully
// mid-run. Mirrors haveFFprobe in internal/scan/duration.go; a process restart
// re-probes.
var (
	haveFfmpeg      = sync.OnceValue(func() bool { _, err := exec.LookPath("ffmpeg"); return err == nil })
	haveExiftool    = sync.OnceValue(func() bool { _, err := exec.LookPath("exiftool"); return err == nil })
	haveHeifConvert = sync.OnceValue(func() bool { _, err := exec.LookPath("heif-convert"); return err == nil })
)

// errFfmpegNotInstalled is returned by the ffmpeg-dependent paths when the
// cached haveFfmpeg probe reports ffmpeg missing.
var errFfmpegNotInstalled = errors.New("ffmpeg not installed")

// Image decodes src, resamples it to fit within size×size, and writes a JPEG
// to dst. Aspect ratio is preserved.
func Image(ctx context.Context, src, dst string, size int) error {
	if info, err := os.Stat(src); err == nil && info.Size() >= imageFfmpegThreshold {
		if haveFfmpeg() {
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
	// Go's image decoders ignore the EXIF Orientation tag (and jpeg.Encode
	// writes none), so a rotated small JPEG would thumbnail sideways here — the
	// ffmpeg path above already auto-applies orientation, so only this
	// in-process fallback passes a non-identity value. writeThumb bakes it in
	// after the downscale (S-04).
	return writeThumb(img, dst, size, imgorient.ReadOrientation(src))
}

// ffmpegScaleFilter builds the `-vf` scale expression that fits the longest
// edge to size while preserving aspect ratio. Shared by the image and RAW
// ffmpeg fast paths (video.go keeps its own copy for its two-stage seek run).
func ffmpegScaleFilter(size int) string {
	return fmt.Sprintf("scale='if(gt(iw,ih),%d,-2)':'if(gt(iw,ih),-2,%d)'", size, size)
}

// ffmpegError folds ffmpeg's stderr (captured via -loglevel error) into the
// returned error so a failure reports *why* instead of a bare "exit status 1".
func ffmpegError(err error, stderr *bytes.Buffer) error {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("ffmpeg: %s", msg)
	}
	return err
}

// imageViaFfmpeg uses ffmpeg's hardware-accelerated decoder + scale filter to
// produce a JPEG thumbnail directly, sidestepping a full pixel-buffer decode
// in Go. The scale filter matches the one used for video thumbs. ffmpeg
// auto-applies EXIF orientation, so the output is already upright.
func imageViaFfmpeg(ctx context.Context, src, dst string, size int) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-frames:v", "1",
		"-vf", ffmpegScaleFilter(size),
		"-f", "image2",
		dst,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dst)
		return ffmpegError(err, &stderr)
	}
	if _, err := os.Stat(dst); err != nil {
		return err
	}
	return nil
}

// imageBytesViaFfmpeg DCT-scales an in-memory JPEG straight to dst, streaming
// it to ffmpeg over stdin so no temp file is needed. It exists for RAW embedded
// previews, which are full-resolution JPEGs (10–45 MP) that are far cheaper to
// scale through libjpeg-turbo's DCT decode than to expand into a huge RGBA in
// Go. ffmpeg auto-applies whatever orientation the preview JPEG carries; on any
// ffmpeg error the caller falls back to the Go decoder, so a build that can't
// read from a pipe degrades to (slower) correctness rather than failure.
func imageBytesViaFfmpeg(ctx context.Context, data []byte, dst string, size int) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-y",
		"-f", "image2pipe",
		"-i", "pipe:0",
		"-frames:v", "1",
		"-vf", ffmpegScaleFilter(size),
		"-f", "image2",
		dst,
	)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(dst)
		return ffmpegError(err, &stderr)
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

// writeThumb resamples src to fit within size×size, bakes in the given EXIF
// orientation, and writes a JPEG to dst. orientation must be one of [1, 8]
// (1 = already upright / identity); callers that decoded through a tool which
// already auto-rotates (ffmpeg, heif-convert) pass 1.
func writeThumb(src image.Image, dst string, size, orientation int) error {
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
	// without zeroing is safe. CatmullRom (a 4×4 cubic kernel) is used over
	// ApproxBiLinear because bilinear aliases badly at the large downscale
	// factors thumbnails hit (a multi-MP source → 256 px); it costs more CPU,
	// but this runs once per thumbnail under the store's cpuSem/extSem bound.
	draw.CatmullRom.Scale(thumb, thumb.Rect, src, b, draw.Src, nil)

	// Bake EXIF orientation in *after* the downscale (S-04): rotation commutes
	// with scaling, so remapping the small thumbnail costs (src/dst)² less than
	// orienting the full-resolution source. fitWithin was fed the stored dims,
	// so for the transpose orientations (5–8) the output's axes swap here —
	// both edges stay ≤ size, so it still fits. orientation==1 returns thumb
	// untouched (the common case), so the pooled buffer is reused as-is.
	oriented := imgorient.Apply(thumb, orientation)

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	return jpeg.Encode(out, oriented, &jpeg.Options{Quality: 82})
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
