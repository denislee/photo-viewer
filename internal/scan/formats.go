package scan

import (
	"path/filepath"
	"strings"
)

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

func DetectType(path string) MediaType {
	ext := filepath.Ext(path)
	if ext == "" {
		return TypeUnknown
	}

	var lowerExt string
	hasUpper := false
	for i := 0; i < len(ext); i++ {
		if ext[i] >= 'A' && ext[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if hasUpper {
		lowerExt = strings.ToLower(ext)
	} else {
		lowerExt = ext
	}

	if t, ok := extensions[lowerExt]; ok {
		return t
	}
	return TypeUnknown
}
