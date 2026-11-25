package scan

import (
	"path/filepath"
	"regexp"
	"strings"
)

// DateFolderRe matches the YYYY-MM-DD folder names import and Organize file
// into; group 1 is the year. Shared by the GUI and web sidebars.
var DateFolderRe = regexp.MustCompile(`^(\d{4})-\d{2}-\d{2}$`)

type MediaType int

const (
	TypeUnknown MediaType = iota
	TypePhoto
	TypeRAW
	TypeHEIC
	TypeVideo
)

func (t MediaType) String() string {
	switch t {
	case TypePhoto:
		return "photo"
	case TypeRAW:
		return "raw"
	case TypeHEIC:
		return "heic"
	case TypeVideo:
		return "video"
	}
	return "unknown"
}

// Title returns String with its first letter upper-cased, for display in the
// GUI and web UI info panels ("Photo", "Raw", "Heic", "Video", "Unknown").
func (t MediaType) Title() string {
	s := t.String()
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

var extensions = map[string]MediaType{
	".jpg":  TypePhoto,
	".jpeg": TypePhoto,
	".png":  TypePhoto,
	".webp": TypePhoto,
	".gif":  TypePhoto,
	".bmp":  TypePhoto,
	".tif":  TypePhoto,
	".tiff": TypePhoto,

	".cr2": TypeRAW,
	".cr3": TypeRAW,
	".nef": TypeRAW,
	".arw": TypeRAW,
	".dng": TypeRAW,
	".raf": TypeRAW,
	".orf": TypeRAW,
	".rw2": TypeRAW,
	// Additional RAW variants (S-11). All ride thumb.RAW's exiftool
	// embedded-preview path (PreviewImage/JpgFromRaw/…), with ffmpeg full-decode
	// as the fallback — exiftool extracts previews from each of these formats.
	".pef": TypeRAW, // Pentax
	".srw": TypeRAW, // Samsung
	".nrw": TypeRAW, // Nikon
	".x3f": TypeRAW, // Sigma (Foveon)

	".heic": TypeHEIC,
	".heif": TypeHEIC,
	// AVIF (AV1-in-HEIF) routes to the HEIC backend, not TypePhoto (S-11): Go's
	// image.Decode has no AVIF decoder, and thumb.Image only reaches ffmpeg for
	// sources >= imageFfmpegThreshold (a small .avif would hit the Go decoder and
	// fail). thumb.HEIC always tries ffmpeg first regardless of size and falls
	// back to heif-convert (libheif decodes AVIF too), so both size classes get a
	// working backend.
	".avif": TypeHEIC,
	// .jxl (JPEG XL) is deliberately omitted (S-11): no reliable backend exists in
	// this codebase. Go's image.Decode has no JXL decoder, ffmpeg's libjxl support
	// is build-dependent and commonly absent, and the exiftool/heif-convert paths
	// don't cover it. Detecting it would only pollute the grid with broken
	// placeholders, so it stays invisible until a working decoder is wired in.

	".mp4":  TypeVideo,
	".mov":  TypeVideo,
	".mkv":  TypeVideo,
	".webm": TypeVideo,
	".avi":  TypeVideo,
	".m4v":  TypeVideo,
	".mts":  TypeVideo,
	".m2ts": TypeVideo,
	// Additional container formats (S-11), all decoded by ffmpeg (thumb.Video).
	".3gp":  TypeVideo,
	".wmv":  TypeVideo,
	".flv":  TypeVideo,
	".mpg":  TypeVideo,
	".mpeg": TypeVideo,
}

// DetectType classifies path by its extension alone (case-insensitive).
// strings.ToLower returns an already-lowercase ASCII extension without
// allocating, and a missing key yields TypeUnknown, the zero value.
func DetectType(path string) MediaType {
	return extensions[strings.ToLower(filepath.Ext(path))]
}
