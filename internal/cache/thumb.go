package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
)

// ThumbSize is the target longest-edge of cached thumbnails.
const ThumbSize = 256

// ThumbStore turns Entry → on-disk thumbnail JPEG path. It generates the
// thumbnail on demand if the file does not yet exist.
//
// Two semaphores split CPU-bound decoders (native Go image package) from
// external-tool decoders (heif-convert, exiftool, ffmpeg). The external
// decoders spend most of their time waiting on a child process, so capping
// them at NumCPU under-utilises hardware; CPU-bound decoders, on the other
// hand, must stay capped or the system bogs down under rapid scrolling.
type ThumbStore struct {
	dir    string // <cache>/thumbs
	cpuSem chan struct{}
	extSem chan struct{}
}

func NewThumbStore(cacheDir string) (*ThumbStore, error) {
	d := filepath.Join(cacheDir, "thumbs")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	cpu := runtime.NumCPU()
	if cpu < 2 {
		cpu = 2
	}
	ext := cpu * 3
	if ext < 6 {
		ext = 6
	}
	return &ThumbStore{
		dir:    d,
		cpuSem: make(chan struct{}, cpu),
		extSem: make(chan struct{}, ext),
	}, nil
}

func (s *ThumbStore) thumbPath(id string) string {
	if len(id) < 2 {
		return filepath.Join(s.dir, id+".jpg")
	}
	return filepath.Join(s.dir, id[:2], id+".jpg")
}

// Path returns the on-disk thumbnail path for e, generating it if missing
// or stale (older than the source file). Returns ("", err) if generation
// failed (e.g. tool not installed).
func (s *ThumbStore) Path(e Entry) (string, error) {
	dst := s.thumbPath(e.ThumbID)
	if info, err := os.Stat(dst); err == nil {
		if info.ModTime().After(e.ModTime) || info.ModTime().Equal(e.ModTime) {
			return dst, nil
		}
		// Stale: source changed since the thumb was written. Regenerate.
		os.Remove(dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"

	sem := s.semFor(e.Type)
	sem <- struct{}{}
	err := s.generate(e, tmp)
	<-sem

	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (s *ThumbStore) semFor(t scan.MediaType) chan struct{} {
	switch t {
	case scan.TypeRAW, scan.TypeHEIC, scan.TypeVideo:
		return s.extSem
	default:
		return s.cpuSem
	}
}

func (s *ThumbStore) generate(e Entry, dst string) error {
	switch e.Type {
	case scan.TypePhoto:
		return thumb.Image(e.Path, dst, ThumbSize)
	case scan.TypeRAW:
		return thumb.RAW(e.Path, dst, ThumbSize)
	case scan.TypeHEIC:
		return thumb.HEIC(e.Path, dst, ThumbSize)
	case scan.TypeVideo:
		return thumb.Video(e.Path, dst, ThumbSize)
	}
	return fmt.Errorf("unsupported media type: %s", e.Type)
}

// Forget deletes the thumbnail file for the given thumb id, if any.
func (s *ThumbStore) Forget(id string) {
	os.Remove(s.thumbPath(id))
}
