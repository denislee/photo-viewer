package export

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dns/photo-viewer/internal/imgorient"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
	"github.com/dns/photo-viewer/internal/tools"
)

// External-tool availability probes are cached in internal/tools: a LookPath
// stats every $PATH entry, so probing per favorite in a large export would
// waste thousands of stats. Kept as vars so tests can reassign them.
var (
	haveFfmpeg      = tools.Ffmpeg
	haveHeifConvert = tools.HeifConvert
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
		return true, recompressImage(ctx, src, dst, maxEdge, jpegQuality)
	case scan.TypeHEIC:
		// Either decoder can satisfy HEIC: recompressHEIC prefers a single
		// ffmpeg pass and falls back to heif-convert. Only plain-copy (return
		// false) when NEITHER tool is present — silently copying the raw HEIC
		// would hand the destination a file most browsers can't render, which
		// defeats the recompress-to-JPEG intent.
		if !haveFfmpeg() && !haveHeifConvert() {
			return false, nil
		}
		return true, recompressHEIC(ctx, src, dst, maxEdge, jpegQuality)
	case scan.TypeVideo:
		if !haveFfmpeg() {
			return false, nil
		}
		return true, recompressVideo(ctx, src, dst, maxEdge, videoCRF)
	}
	return false, nil
}

// recompressImage decodes any image format supported by image.Decode (plus
// the x/image additions: webp, bmp, tiff), resizes it so the longest edge
// is at most maxEdge (no upscaling), and writes a JPEG at the given
// quality. JPEGs decode through libjpeg-turbo with DCT scaling
// (thumb.DecodeFit), so a large photo is decoded only as large as the output
// needs; ctx cancels the read and the decode. The output is encoded through
// thumb.FitEncode (the same fit + CatmullRom scale + orientation path the
// thumbnailer uses), which encodes with libjpeg-turbo when it is linked
// (T5-03) and image/jpeg otherwise.
func recompressImage(ctx context.Context, src, dst string, maxEdge, quality int) error {
	// Capture the source mtime before decoding so we can stamp it onto the
	// finalized output (see the Chtimes after rename below).
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	// One read serves the decode and the orientation lookup (E4-01). DecodeFit
	// lets libjpeg-turbo DCT-scale a JPEG to no less than the fitted size, so a
	// 24 MP photo is never expanded to full resolution only to be scaled down;
	// other formats decode through image.Decode as before. Both stop early
	// when ctx is cancelled.
	data, err := thumb.ReadFile(ctx, src)
	if err != nil {
		return err
	}
	img, err := thumb.DecodeFit(ctx, data, maxEdge)
	if err != nil {
		return err
	}
	// Go's image decoders ignore the EXIF Orientation tag and no JPEG encoder
	// we use writes EXIF, so a portrait camera JPEG (Orientation != 1) would
	// otherwise export with sideways pixels AND no tag left to correct it —
	// permanently rotated. Bake the orientation into the pixels so the output
	// JPEG is upright with no orientation tag needed. (The plain non-recompress
	// copy path keeps the original bytes incl. the EXIF tag, so it doesn't need
	// this.) Orient *after* the downscale (S-04): rotation commutes with
	// scaling, so the remap runs on the small output, not the full-res source.
	// imgfit.Within is fed the stored dims; for the transpose orientations (5–8) the
	// output's axes swap, both staying ≤ maxEdge.
	orient := imgorient.ReadOrientationFrom(bytes.NewReader(data))
	if err := ctx.Err(); err != nil {
		return err
	}

	// thumb.FitEncode owns the fit/scale/orient/encode sequence shared with the
	// thumbnail path: CatmullRom (a 4×4 cubic kernel), not ApproxBiLinear — a
	// kept-forever export of a 24 MP source down to a 2048 px edge is exactly the
	// large-downscale regime where bilinear aliases badly, the same reason the
	// 256 px thumb path uses it. Exports aren't latency-critical (video is
	// ffmpeg-bound anyway), so the extra CPU is fine.
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = thumb.FitEncode(out, img, maxEdge, orient, quality)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// publishTemp fsyncs the encoded JPEG so it is on stable storage, not just
	// in the page cache, before the rename makes it visible at dst.
	if err := publishTemp(tmp, dst); err != nil {
		return err
	}
	// Preserve the source's mtime so a recompressed export sorts by capture time
	// (source mtime), not encode time, in any date-ordered browser — mirroring
	// fsutil.CopyTo's mode+mtime preservation. Best-effort, matching
	// fsutil.CopyTo's ignored Chtimes error: a failed stamp doesn't invalidate
	// an otherwise-good export.
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

// recompressHEIC re-encodes a HEIF source to a downscaled JPEG at dst.
//
// It prefers a SINGLE ffmpeg pass (decode HEIC → scale → JPEG-encode in one
// invocation): one decode, one lossy generation. This mirrors the thumbnail
// path (internal/thumb/heic.go), which learned the same trick. The previous
// implementation always ran heif-convert → a q95 intermediate JPEG →
// decode → scale → re-encode: two lossy generations and two codec passes.
//
// When ffmpeg is unavailable or refuses the file (HEIC support depends on how
// ffmpeg was built), it falls back to the heif-convert → intermediate-JPEG →
// recompressImage chain. Either way the source mtime is stamped onto dst so a
// recompressed export sorts by capture time, not encode time.
func recompressHEIC(ctx context.Context, src, dst string, maxEdge, quality int) error {
	// Capture the original HEIC's mtime up front. The ffmpeg pass writes a fresh
	// file (encode-time mtime) and the fallback's recompressImage stamps the
	// intermediate temp's mtime, so both paths re-stamp the source's mtime after.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	var ffErr error
	if haveFfmpeg() {
		ffErr = recompressHEICViaFfmpeg(ctx, src, dst, maxEdge, quality)
		if ffErr == nil {
			// Preserve the source mtime (S-09). Best-effort, matching
			// fsutil.CopyTo.
			_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
			return nil
		}
		// fall through — ffmpeg either lacks a HEIC decoder or refused this
		// file; the heif-convert chain below is the more permissive path.
	}

	if !haveHeifConvert() {
		if ffErr != nil {
			return fmt.Errorf("recompress HEIC: ffmpeg failed and heif-convert not installed: %w", ffErr)
		}
		return errors.New("recompress HEIC: no HEIC decoder available")
	}

	tmpFull, err := os.CreateTemp("", "pv-export-*.jpg")
	if err != nil {
		return err
	}
	tmpFull.Close()
	defer os.Remove(tmpFull.Name())

	if _, err := tools.Run(ctx, nil, "heif-convert", "-q", "95", src, tmpFull.Name()); err != nil {
		return err
	}
	if err := recompressImage(ctx, tmpFull.Name(), dst, maxEdge, quality); err != nil {
		return err
	}
	// Override the intermediate-temp mtime recompressImage stamped with the
	// original HEIC's mtime. Best-effort, matching fsutil.CopyTo's ignored
	// error.
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

// recompressHEICViaFfmpeg does the whole HEIC → downscaled-JPEG job in one
// ffmpeg invocation: ffmpeg decodes the HEIF (libheif/HEVC), its scaler fits
// the long edge to maxEdge, and its mjpeg encoder writes the JPEG at the
// mapped quality — one decode, one encode, no intermediate max-quality JPEG.
// A tiled grid HEIC (the iPhone default) needs the stream-group filter form on
// ffmpeg >= 7.1; every other layout needs -vf, so the second form is retried
// if the first fails. ffmpeg auto-applies the HEIF container rotation (the same
// autorotate the thumb path relies on), so the output is upright with no
// orientation tag, matching recompressImage's baked-in-orientation contract.
// The write is atomic and durable (temp + fsync + rename), like recompressVideo.
func recompressHEICViaFfmpeg(ctx context.Context, src, dst string, maxEdge, quality int) error {
	tmp := dst + ".tmp.jpg"
	err := runHEICRecompress(ctx, src, tmp,
		[]string{"-filter_complex", "[0:g:0]" + heicScaleFilter(maxEdge)}, quality)
	if err != nil {
		// Only a rejected stream-group specifier (no grid, or an older ffmpeg)
		// is worth a -vf retry; a corrupt file or missing decoder fails both.
		if ctx.Err() != nil || !thumb.IsStreamSpecifierError(err) {
			return err
		}
		// No grid stream group (or an older ffmpeg): retry once with -vf.
		if err := runHEICRecompress(ctx, src, tmp,
			[]string{"-vf", heicScaleFilter(maxEdge)}, quality); err != nil {
			return err
		}
	}
	return publishTemp(tmp, dst)
}

// runHEICRecompress runs the single ffmpeg pass behind recompressHEICViaFfmpeg,
// placing filterArgs where the scaling filter belongs. On failure it removes the
// partial temp and folds ffmpeg's diagnostics into the error.
func runHEICRecompress(ctx context.Context, src, tmp string, filterArgs []string, quality int) error {
	args := []string{
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-frames:v", "1",
	}
	args = append(args, filterArgs...)
	args = append(args,
		"-c:v", "mjpeg",
		"-q:v", strconv.Itoa(mjpegQScale(quality)),
		"-f", "image2",
		tmp,
	)
	if _, err := tools.Run(ctx, nil, "ffmpeg", args...); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// publishTemp makes an ffmpeg-written temp durable and moves it into place. The
// child process's write leaves the bytes only in the page cache; fsync before
// the rename so a yanked USB drive or power loss can't leave a truncated file
// at dst after the export reported success — the guarantee recompressImage
// gives its in-process encode (S-22). The temp is removed on any failure.
func publishTemp(tmp, dst string) error {
	err := syncFile(tmp)
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// syncFile fsyncs the file at p.
func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// heicScaleFilter builds the ffmpeg `-vf` scale expression that fits the long
// edge to maxEdge WITHOUT upscaling (min(edge, maxEdge)), matching
// imgfit.Within's contract. Both output dimensions are forced even
// (trunc(.../2)*2 on the constrained edge, -2 on the auto edge) because the
// mjpeg encoder's default 4:2:0 subsampling requires it. ffmpeg autorotate runs
// before this filter, so iw/ih here are the already-upright display dimensions.
func heicScaleFilter(maxEdge int) string {
	return fmt.Sprintf("scale='if(gt(iw,ih),trunc(min(iw,%d)/2)*2,-2)':'if(gt(iw,ih),-2,trunc(min(ih,%d)/2)*2)'", maxEdge, maxEdge)
}

// mjpegQScale maps a libjpeg quality (1-100, higher = better — the scale the
// export Options use) onto ffmpeg's mjpeg -q:v scale (2-31, LOWER = better).
// The two encoders use different quantizer scales, so this is an approximate
// remap, not an exact equivalence: q=100 → 2 (best), q=1 → 31 (worst), linear
// in between. It keeps the export's "higher JpegQuality ⇒ higher-fidelity
// output" intent without a second lossy generation.
func mjpegQScale(quality int) int {
	quality = max(min(quality, 100), 1)
	qv := 31 - (quality*29)/100
	return max(min(qv, 31), 2)
}

// recompressVideo invokes ffmpeg to transcode src into dst, fitting the
// frame inside maxEdge × maxEdge (aspect preserved, never upscaled) and
// using libx264 at the given CRF for the video stream. Audio is re-encoded
// to AAC at 128 kbps so MOV/MTS sources still produce a portable MP4.
func recompressVideo(ctx context.Context, src, dst string, maxEdge, crf int) error {
	// Capture the source mtime up front so we can stamp it onto the ffmpeg output
	// after the rename below.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
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
	if _, err := tools.Run(ctx, nil, "ffmpeg", args...); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := publishTemp(tmp, dst); err != nil {
		return err
	}
	// Preserve the source's mtime so the transcoded clip sorts by capture time,
	// not encode time. Best-effort, matching fsutil.CopyTo's ignored Chtimes
	// error.
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}
