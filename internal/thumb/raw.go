package thumb

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"strings"

	"github.com/dns/photo-viewer/internal/scan"
)

// RAW extracts an embedded JPEG preview from a camera RAW file and resamples
// it into a thumbnail. Falls back to ffmpeg if no embedded preview is found.
func RAW(ctx context.Context, src, dst string, size int) error {
	img, err := LoadRAWImage(ctx, src)
	if err != nil {
		return err
	}
	return writeThumb(img, dst, size)
}

// LoadRAWImage returns a decoded image.Image from a RAW file. It tries
// embedded previews first (via exiftool) and falls back to decoding the
// full RAW via ffmpeg.
func LoadRAWImage(ctx context.Context, src string) (image.Image, error) {
	data, err := LoadRAWPreview(ctx, src)
	if err == nil {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err == nil {
			return img, nil
		}
	}

	// Fallback: ffmpeg can decode many RAW formats (DNG, CR2, etc) directly.
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		cmd := exec.CommandContext(ctx, "ffmpeg", "-i", src, "-frames:v", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "-")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			img, _, err := image.Decode(&out)
			if err == nil {
				return img, nil
			}
		}
	}

	if err != nil {
		return nil, err
	}
	return nil, errors.New("could not decode RAW file")
}

// LoadRAWPreview returns the bytes of an embedded JPEG/TIFF preview.
func LoadRAWPreview(ctx context.Context, src string) ([]byte, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	var head [4]byte
	n, _ := f.Read(head[:])
	f.Close()

	// If it's already a JPEG, return it whole.
	if n >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff {
		return os.ReadFile(src)
	}

	if _, err := exec.LookPath("exiftool"); err != nil {
		return nil, errors.New("exiftool not installed")
	}

	// Common preview tags in descending order of typical quality/size.
	tags := []string{"PreviewImage", "JpgFromRaw", "ThumbnailImage", "PreviewTIFF", "OtherImage"}

	// One shared-daemon metadata read reports which of those tags this file
	// actually carries, so we run "exiftool -b" only for previews that exist
	// instead of forking it once per candidate tag (up to five sequential
	// processes per RAW). The probe rides the -stay_open daemon (no fork); only
	// the binary extraction below spawns a process. When the probe can't
	// classify the file (daemon missing, unusual layout) it returns nil and we
	// fall back to trying every tag, so no format regresses.
	candidates := presentPreviewTags(src, tags)
	if len(candidates) == 0 {
		candidates = tags
	}
	for _, tag := range candidates {
		b, err := extractEmbedded(ctx, src, "-"+tag)
		if err == nil && len(b) > 0 {
			// Quick check: is this actually an image?
			if _, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
				return b, nil
			}
		}
	}

	return nil, errors.New("no embedded preview found")
}

// presentPreviewTags returns the subset of candidate preview tags that src
// actually contains, preserving the caller's priority order. It issues a single
// textual exiftool read through the shared -stay_open daemon (no per-tag fork):
// with "-S", a present binary tag prints "TagName: (Binary data N bytes ...)"
// while absent tags print nothing. Returns nil on any error so the caller can
// fall back to trying every tag.
func presentPreviewTags(src string, candidates []string) []string {
	args := make([]string, 0, len(candidates)+2)
	args = append(args, "-S")
	for _, t := range candidates {
		args = append(args, "-"+t)
	}
	args = append(args, src)
	out, err := scan.RunExiftool(args...)
	if err != nil {
		return nil
	}
	found := make(map[string]bool, len(candidates))
	for _, line := range strings.Split(string(out), "\n") {
		if tag, _, ok := strings.Cut(line, ":"); ok {
			found[strings.TrimSpace(tag)] = true
		}
	}
	var present []string
	for _, t := range candidates {
		if found[t] {
			present = append(present, t)
		}
	}
	return present
}

func extractEmbedded(ctx context.Context, src, tag string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "exiftool", "-b", tag, src)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
