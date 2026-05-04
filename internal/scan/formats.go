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

	".heic": TypeHEIC,
	".heif": TypeHEIC,

	".mp4":  TypeVideo,
	".mov":  TypeVideo,
	".mkv":  TypeVideo,
	".webm": TypeVideo,
	".avi":  TypeVideo,
	".m4v":  TypeVideo,
	".mts":  TypeVideo,
	".m2ts": TypeVideo,
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
