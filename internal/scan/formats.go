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
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := extensions[ext]; ok {
		return t
	}
	return TypeUnknown
}
