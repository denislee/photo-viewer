package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dns/photo-viewer/internal/imgorient"
	"github.com/dns/photo-viewer/internal/jpegturbo"
	"github.com/dns/photo-viewer/internal/scan"
)

// RAW extracts an embedded JPEG preview from a camera RAW file and resamples
// it into a thumbnail. Falls back to decoding the full RAW via ffmpeg.
func RAW(ctx context.Context, src, dst string, size int) error {
	data, orientation, previewErr := loadRAWPreview(ctx, src)
	if previewErr == nil && len(data) > 0 {
		// libjpeg-turbo in process: DCT-scaled decode of the preview with no
		// fork, and the RAW's own orientation tag applied (the ffmpeg path
		// below can only honour a tag inside the preview itself).
		if jpegturbo.Available() && jpegturbo.IsJPEG(data) {
			if img, err := DecodeFit(ctx, data, size); err == nil {
				return writeThumb(img, dst, size, orientation)
			}
		}
		// A full-resolution embedded preview (often 10–45 MP) is far cheaper to
		// DCT-scale through ffmpeg than to expand into a huge RGBA in Go — the
		// same >=imageFfmpegThreshold watershed the plain-image path uses. On
		// ffmpeg failure we fall through to the Go decoder below.
		if len(data) >= imageFfmpegThreshold {
			if haveFfmpeg() {
				if err := imageBytesViaFfmpeg(ctx, data, dst, size); err == nil {
					return nil
				}
			}
		}
		if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			// The Go decoder ignores EXIF orientation; the preview is stored in
			// sensor orientation, so pass the RAW's Orientation tag to writeThumb,
			// which bakes it in after the downscale (S-04). (The ffmpeg fast path
			// above relies on ffmpeg's autorotate instead — a large preview that
			// lacks its own tag is no worse than the pre-I-12 behaviour, which
			// never rotated at all.)
			return writeThumb(img, dst, size, orientation)
		}
	}

	// Fallback: ffmpeg decodes the full RAW, scales it and writes the thumbnail
	// in one pass (T4-02), autorotating as it goes — the same imageViaFfmpeg path
	// large plain images take, instead of a full-resolution MJPEG round trip
	// through Go's decoder.
	err := errFfmpegNotInstalled
	if haveFfmpeg() {
		err = imageViaFfmpeg(ctx, src, dst, size)
	}
	if err != nil {
		if previewErr != nil {
			return previewErr
		}
		return err
	}
	return nil
}

// LoadRAWImage returns a decoded image.Image from a RAW file. It tries
// embedded previews first (via exiftool) and falls back to decoding the
// full RAW via ffmpeg. Used by the full-resolution viewer path; the thumbnail
// path (RAW) has its own ffmpeg fast/orientation handling.
//
// The returned orientation is the EXIF Orientation still to be applied with
// imgorient.Apply: the embedded preview is stored in sensor orientation, so it
// carries the RAW's tag, while the ffmpeg fallback autorotates and reports 1.
// Orientation is returned rather than applied so the caller can rotate after
// downscaling — previews are often full-resolution (S-21).
//
// fit, when non-nil, receives the decoded JPEG's stored size and the
// orientation still to be applied, and returns the minimum stored-orientation
// size to DCT-scale to (T4-05): a viewer window smaller than a 24–45 MP preview
// then decodes only what it shows. nil decodes at full size.
func LoadRAWImage(ctx context.Context, src string, fit func(w, h, orientation int) (minW, minH int)) (img image.Image, orientation int, err error) {
	data, orientation, err := loadRAWPreview(ctx, src)
	if err == nil {
		img, _, derr := DecodeFitFunc(ctx, data, bindFit(fit, orientation))
		if derr == nil {
			return img, orientation, nil
		}
	}

	img, ferr := decodeRAWViaFfmpeg(ctx, src, bindFit(fit, 1))
	if ferr == nil {
		return img, 1, nil
	}
	if err != nil {
		return nil, 1, err
	}
	return nil, 1, errors.New("could not decode RAW file")
}

// bindFit fixes the orientation argument of a LoadRAWImage fit, giving the
// two-argument form DecodeFitFunc takes. A nil fit stays nil (full size).
func bindFit(fit func(w, h, orientation int) (int, int), orientation int) func(w, h int) (int, int) {
	if fit == nil {
		return nil
	}
	return func(w, h int) (int, int) { return fit(w, h, orientation) }
}

// decodeRAWViaFfmpeg decodes the full RAW via ffmpeg (which handles DNG, CR2,
// etc. and auto-applies orientation) and DCT-scales the single MJPEG frame it
// emits with libjpeg-turbo. It is the viewer's fallback (LoadRAWImage) only —
// the thumbnail path (RAW) has imageViaFfmpeg to write straight to disk.
// -threads 1 keeps the fork inside the C-06 cap and -loglevel error leaves
// stderr carrying only the failure reason.
func decodeRAWViaFfmpeg(ctx context.Context, src string, fit func(w, h int) (int, int)) (image.Image, error) {
	if !haveFfmpeg() {
		return nil, errFfmpegNotInstalled
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-loglevel", "error", "-threads", "1", "-i", src, "-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "-")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, ffmpegError(err, &stderr)
	}
	img, _, err := DecodeFitFunc(ctx, out.Bytes(), fit)
	return img, err
}

// LoadRAWPreview returns the bytes of an embedded JPEG/TIFF preview.
func LoadRAWPreview(ctx context.Context, src string) ([]byte, error) {
	data, _, err := loadRAWPreview(ctx, src)
	return data, err
}

// loadRAWPreview returns the bytes of an embedded JPEG/TIFF preview together
// with the EXIF Orientation still to be applied to it. The orientation rides
// the same daemon probe that classifies the preview tags, so the common path
// costs no extra file open; when the probe can't report it (daemon missing, no
// such tag) it falls back to imgorient.ReadOrientation, and it is only computed
// on a path that actually returns preview bytes.
func loadRAWPreview(ctx context.Context, src string) (data []byte, orientation int, err error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, 0, err
	}
	var head [4]byte
	n, _ := f.Read(head[:])
	f.Close()

	// If it's already a JPEG, return it whole.
	if n >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff {
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, 0, err
		}
		return data, imgorient.ReadOrientation(src), nil
	}

	if !haveExiftool() {
		return nil, 0, errors.New("exiftool not installed")
	}

	// Common preview tags in descending order of typical quality/size.
	tags := []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage", "PreviewTIFF", "OtherImage"}

	// One shared-daemon metadata read reports which of those tags this file
	// actually carries, plus the Orientation tag, so we run "exiftool -b" only
	// for previews that exist instead of forking it once per candidate tag (up
	// to five sequential processes per RAW). Probe and extraction both ride the
	// -stay_open daemon (T5-05). When the probe can't classify the file (daemon
	// missing, unusual layout) it returns nil and we fall back to trying every
	// tag, so no format regresses.
	candidates, orientation := probeRAWPreview(src, tags)
	if len(candidates) == 0 {
		candidates = tags
	}
	for _, tag := range candidates {
		b, err := extractEmbedded(ctx, src, "-"+tag)
		if err == nil && len(b) > 0 {
			// Quick check: is this actually an image?
			if _, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
				if orientation == 0 {
					orientation = imgorient.ReadOrientation(src)
				}
				return b, orientation, nil
			}
		}
	}

	return nil, 0, errors.New("no embedded preview found")
}

// probeRAWPreview returns the subset of candidate preview tags that src
// actually contains, preserving the caller's priority order, plus the file's
// EXIF Orientation (0 when it has none, or an out-of-range value). It issues a
// single textual exiftool read through the shared -stay_open daemon (no per-tag
// fork): with "-S", a present binary tag prints "TagName: (Binary data N bytes
// ...)" while absent tags print nothing, and "-Orientation#" prints
// "Orientation: <n>" when the tag exists. Returns (nil, 0) on any error so the
// caller can fall back to trying every tag and reading the orientation itself.
func probeRAWPreview(src string, candidates []string) (present []string, orientation int) {
	args := make([]string, 0, len(candidates)+3)
	args = append(args, "-S")
	for _, t := range candidates {
		args = append(args, "-"+t)
	}
	args = append(args, "-Orientation#", src)
	out, err := scan.RunExiftool(args...)
	if err != nil {
		return nil, 0
	}
	found := make(map[string]bool, len(candidates))
	for line := range strings.SplitSeq(string(out), "\n") {
		tag, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		tag = strings.TrimSpace(tag)
		// The orientation is not a preview candidate: it reports a value, not
		// the presence of a binary preview, so keep it out of the tag set.
		if tag == "Orientation" {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 1 && n <= 8 {
				orientation = n
			}
			continue
		}
		found[tag] = true
	}
	for _, t := range candidates {
		if found[t] {
			present = append(present, t)
		}
	}
	return present, orientation
}

// extractEmbedded returns the bytes of one embedded tag (tag carries its own
// leading '-'). (T5-05) It hands the request to the shared -stay_open daemon
// and has exiftool write the binary value to a temp file with "-W!" instead of
// to stdout, so a RAW preview costs a daemon round trip (~4-8 ms) rather than a
// one-shot fork (~70-81 ms). RunExiftool itself falls back to a one-shot exec
// with the same args, which writes the same file, so the daemon is not a
// hard requirement.
func extractEmbedded(ctx context.Context, src, tag string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pv-rawprev-")
	if err != nil {
		return extractEmbeddedOneShot(ctx, src, tag)
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "preview.bin")
	// exiftool reads '%' in the -W argument as a format code, so escape it: a
	// temp dir path is normally '%'-free, but $TMPDIR is caller-controlled.
	if _, err := scan.RunExiftool("-b", tag, "-W!", strings.ReplaceAll(out, "%", "%%"), src); err != nil {
		return extractEmbeddedOneShot(ctx, src, tag)
	}
	if data, err := os.ReadFile(out); err == nil {
		return data, nil
	}
	// The daemon discards exiftool's stderr, so a missing output file means
	// either "this file has no such tag" or "the source couldn't be read at
	// all". When the source itself is the problem, rerun it one-shot so the
	// caller gets exiftool's reason instead of a misleading "not present".
	if _, statErr := os.Stat(src); statErr != nil {
		return extractEmbeddedOneShot(ctx, src, tag)
	}
	// No file means the tag is absent from this file rather than a failure.
	return nil, fmt.Errorf("exiftool: %s not present", tag)
}

// extractEmbeddedOneShot is the original fork-per-tag extraction: exiftool
// writes the tag's bytes to stdout. It is the fallback both when the temp dir
// can't be created and when the daemon request fails.
func extractEmbeddedOneShot(ctx context.Context, src, tag string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "exiftool", "-b", tag, src)
	var buf, stderr bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, scan.ExiftoolError(err, &stderr)
	}
	return buf.Bytes(), nil
}
