package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

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

	// Singleflight: concurrent Path() calls for the same thumb id share one
	// generation. The first caller stores a channel here; followers wait on
	// it and re-stat the result. Keyed by thumb id.
	flightMu sync.Mutex
	inflight map[string]chan struct{}
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
		dir:      d,
		cpuSem:   make(chan struct{}, cpu),
		extSem:   make(chan struct{}, ext),
		inflight: make(map[string]chan struct{}),
	}, nil
}

// CacheDir returns the base cache directory the store lives under (the
// parent of <cache>/thumbs). Other on-disk caches that want to sit
// alongside the thumbnails — e.g. the HLS segment cache — derive their
// location from here so they share the same writable root the store
// already resolved.
func (s *ThumbStore) CacheDir() string {
	return filepath.Dir(s.dir)
}

// Has reports whether a fresh thumbnail for e already exists on disk — i.e.
// one that is not older than the source file. It is a single stat with no
// generation, singleflight, or directory creation, so a bulk warm-up can use
// it to cheaply skip the (usually large) set of already-cached thumbnails
// before paying the cost of entering Path's generation machinery.
func (s *ThumbStore) Has(e Entry) bool {
	info, err := os.Stat(s.thumbPath(e.ThumbID))
	if err != nil {
		return false
	}
	return !info.ModTime().Before(e.ModTime)
}

// WarmUpConcurrency is the number of worker goroutines a bulk warm-up should
// run so both decode semaphores stay saturated. Path() is internally bounded
// by cpuSem/extSem, but a single caller only ever keeps one decode in flight;
// fanning out to this many callers lets the CPU-bound and external-tool pools
// run at full width at once.
func (s *ThumbStore) WarmUpConcurrency() int {
	return cap(s.cpuSem) + cap(s.extSem)
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

	// Singleflight: dedupe concurrent calls for the same thumb id.
	s.flightMu.Lock()
	if ch, ok := s.inflight[e.ThumbID]; ok {
		s.flightMu.Unlock()
		<-ch
		if info, err := os.Stat(dst); err == nil &&
			(info.ModTime().After(e.ModTime) || info.ModTime().Equal(e.ModTime)) {
			return dst, nil
		}
		return "", fmt.Errorf("thumb generation failed for %s", e.Path)
	}
	ch := make(chan struct{})
	s.inflight[e.ThumbID] = ch
	s.flightMu.Unlock()
	defer func() {
		s.flightMu.Lock()
		delete(s.inflight, e.ThumbID)
		s.flightMu.Unlock()
		close(ch)
	}()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"

	sem := s.semFor(e.Type)
	sem <- struct{}{}
	err := s.generate(context.Background(), e, tmp)
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

func (s *ThumbStore) generate(ctx context.Context, e Entry, dst string) error {
	switch e.Type {
	case scan.TypePhoto:
		return thumb.Image(ctx, e.Path, dst, ThumbSize)
	case scan.TypeRAW:
		return thumb.RAW(ctx, e.Path, dst, ThumbSize)
	case scan.TypeHEIC:
		return thumb.HEIC(ctx, e.Path, dst, ThumbSize)
	case scan.TypeVideo:
		return thumb.Video(ctx, e.Path, dst, ThumbSize)
	}
	return fmt.Errorf("unsupported media type: %s", e.Type)
}

// Forget deletes the thumbnail file for the given thumb id, if any.
func (s *ThumbStore) Forget(id string) {
	os.Remove(s.thumbPath(id))
}

// Rename moves the thumbnail file from oldID to newID, creating the
// destination shard directory if needed. Used by the soft-delete flow so
// trashed files keep their existing thumbnail without regeneration.
// Missing source files are silently treated as success.
func (s *ThumbStore) Rename(oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	src := s.thumbPath(oldID)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	dst := s.thumbPath(newID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
