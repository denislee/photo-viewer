package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ffmpegNoHEIC latches true the first time ffmpeg fails to open a HEIC that the
// heif-convert fallback then decodes fine — strong evidence this ffmpeg build
// simply lacks a HEIC/HEVC decoder (a corrupt file would have failed the
// fallback too). Once set, HEIC() skips the ffmpeg attempt so a HEIC-heavy
// library doesn't pay one doomed fork per file forever. One-way; a process
// restart re-probes.
var ffmpegNoHEIC atomic.Bool

// HEIC converts a HEIC/HEIF file to JPEG and resamples it.
//
// Strategy: prefer ffmpeg (one subprocess does the HEIC decode + downscale +
// JPEG encode in a single pass — same trick the video thumbnailer uses), using
// the stream-group filter form for tiled grid HEICs and falling back to
// `-vf scale` for everything else. Fall back to `heif-convert` + an in-process
// scale when ffmpeg can't open the file (HEIC support depends on how ffmpeg was
// built). The previous implementation always wrote a full-resolution JPEG then
// re-decoded + re-scaled + re-encoded it, paying for two JPEG codec passes.
func HEIC(ctx context.Context, src, dst string, size int) error {
	ffmpegTried := false
	if !ffmpegNoHEIC.Load() {
		if haveFfmpeg() {
			ffmpegTried = true
			if err := heicViaFfmpeg(ctx, src, dst, size); err == nil {
				return nil
			}
			// fall through — ffmpeg either lacks HEIC or refused this file.
		}
	}

	tmpDir, err := os.MkdirTemp("", "photo-viewer-heic-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	jpg, lastErr := HEICToJPEG(ctx, src, tmpDir, ffmpegTried)
	if lastErr != nil {
		return lastErr
	}
	// The fallback decoded a file ffmpeg had just refused: latch that ffmpeg
	// can't do HEIC so future files skip the wasted fork (see ffmpegNoHEIC).
	if ffmpegTried {
		ffmpegNoHEIC.Store(true)
	}
	// jpg is a freshly written JPEG of unknown size. Skip the os.Stat /
	// ffmpeg-threshold branch inside Image() (we know exactly where the
	// file came from) and just decode + writeThumb directly. heif-convert /
	// ffmpeg already baked in the HEIF rotation, so it's upright (orientation 1).
	data, err := ReadFile(ctx, jpg)
	if err != nil {
		return err
	}
	img, err := DecodeFit(ctx, data, size)
	if err != nil {
		return err
	}
	return writeThumb(img, dst, size, 1)
}

// heicViaFfmpeg thumbnails a HEIC through ffmpeg, handling both container
// layouts. Tiled grid HEIC (every iPhone photo) needs the stream-group form on
// ffmpeg >= 7.1: -vf then fails with "Simple and complex filtering cannot be
// used together", while -filter_complex "[0:g:0]<scale>" works. Non-grid HEIC
// has no stream group and needs -vf instead, and older ffmpeg rejects `g:` —
// both fall through to -vf (T5-01).
func heicViaFfmpeg(ctx context.Context, src, dst string, size int) error {
	gridFilter := []string{"-filter_complex", "[0:g:0]" + ffmpegScaleFilter(size)}
	err := runFfmpegThumbFilter(ctx, dst, gridFilter, nil, "-i", src)
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	// Retry with -vf only when ffmpeg rejected the stream-group specifier (a
	// non-grid file, or an ffmpeg without stream groups). Any other failure —
	// a corrupt file, no HEVC decoder — would fail -vf too, so it costs one
	// fork, not two (T4-03).
	if !isStreamSpecifierError(err) {
		return err
	}
	return imageViaFfmpeg(ctx, src, dst, size)
}

// isStreamSpecifierError reports whether an ffmpeg error says the `[0:g:0]`
// stream specifier matched nothing or was not understood.
func isStreamSpecifierError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "pecifier")
}

// HEICToJPEG decodes a HEIC/HEIF file to a full-resolution JPEG written under
// tmpDir. Returns the path of the produced JPEG; the caller is responsible for
// removing tmpDir. skipFfmpeg skips the ffmpeg fallback; HEIC sets it when its
// own ffmpeg attempt just failed on this file, so a machine whose ffmpeg lacks
// HEIC and has no heif-convert pays one doomed fork per file, not two (T4-03).
// The fallback is also skipped once ffmpegNoHEIC has latched.
func HEICToJPEG(ctx context.Context, src, tmpDir string, skipFfmpeg bool) (string, error) {
	var heifErr error
	if haveHeifConvert() {
		intermediate := filepath.Join(tmpDir, "out.jpg")
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "heif-convert", "-q", "90", "--quiet", src, intermediate)
		cmd.Stderr = &stderr
		errRun := cmd.Run()
		// For multi-image HEIC (Apple live photos, HDR pairs, image stacks)
		// heif-convert writes out-1.jpg, out-2.jpg, ... and never creates the
		// requested name. Pick whichever JPEG it actually produced; ignore
		// errRun if any usable output landed since heif-convert sometimes
		// exits non-zero after successfully writing the primary image.
		if jpg, err := pickHeifOutput(tmpDir, intermediate); err == nil {
			return jpg, nil
		}
		if errRun != nil {
			heifErr = fmt.Errorf("heif-convert: %s", stderrOr(&stderr, errRun))
		} else {
			heifErr = errors.New("heif-convert produced no jpeg output")
		}
	} else {
		heifErr = errors.New("heif-convert not installed")
	}

	// Fallback: ffmpeg can decode HEIC via libheif/HEVC and write a JPEG.
	// Both failure reasons are reported, so a missing heif-convert no longer
	// hides why ffmpeg failed (S6-04).
	if !skipFfmpeg && !ffmpegNoHEIC.Load() && haveFfmpeg() {
		out := filepath.Join(tmpDir, "ff-out.jpg")
		var stderr bytes.Buffer
		// -threads 1 (before -i) caps the HEVC-intra decoder — a single-frame
		// HEIC decode needs no parallel threads, and this path runs under the
		// store's oversized extSem (C-06).
		cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-loglevel", "error",
			"-threads", "1", "-i", src, "-frames:v", "1", "-update", "1", out)
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			if _, err = os.Stat(out); err == nil {
				return out, nil
			}
		}
		return "", errors.Join(heifErr, fmt.Errorf("ffmpeg: %s", stderrOr(&stderr, err)))
	}
	return "", heifErr
}

// stderrOr returns a tool's trimmed stderr, or err's text when it wrote none.
func stderrOr(stderr *bytes.Buffer, err error) string {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return msg
	}
	return err.Error()
}

func pickHeifOutput(dir, preferred string) (string, error) {
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}
	// Handle sequences and potential extension differences (case/jpeg vs jpg)
	for _, ext := range []string{"*.jpg", "*.jpeg", "*.JPG", "*.JPEG", "*.png"} {
		matches, _ := filepath.Glob(filepath.Join(dir, ext))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("heif-convert produced no jpeg output")
}
