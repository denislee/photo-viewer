package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
)

// ThumbSize is the target longest-edge of cached thumbnails.
const ThumbSize = 256

// LargeThumbSize is the longest edge of the larger thumbnail tier the grid
// uses for big cells (G5). It is generated on demand, only for thumbnails
// actually viewed large, under LargeThumbDir.
const LargeThumbSize = 512

// LargeThumbDir is the cache subdirectory of the LargeThumbSize tier.
const LargeThumbDir = "thumbs-512"

// DisplaySize is the target longest-edge of the larger "display" renditions
// the webserver serves to the browser viewer (W-03). A file a browser can't
// render (RAW/HEIC/TIFF) or a needlessly-large JPEG is downscaled to this size
// instead of shipping the full-resolution original: ~2048 px is visually
// indistinguishable on a phone/tablet screen while being an order of magnitude
// smaller over the wire.
const DisplaySize = 2048

// DisplayDir is the cache subdirectory of the DisplaySize tier.
const DisplayDir = "display"

// thumbGenTimeout bounds a single thumbnail generation. Each of the four
// thumbnailers shells out under exec.CommandContext, but nothing ever fed
// them a deadline — so a hung ffmpeg/exiftool/heif-convert would block its
// goroutine forever, permanently burn its cpuSem/extSem slot and (via the
// singleflight below) wedge every future Path() for that thumb id on a
// channel that never closes. That is the "viewer gradually stops producing
// thumbnails until restart" failure mode. Arming the ctx here is what
// actually fires the thumbnailers' kill paths; a timeout then unwinds
// exactly like any other generation failure.
const thumbGenTimeout = 30 * time.Second

// staleTmpMaxAge bounds how long a half-written "*.tmp" rendition may linger in
// a shard directory before a startup sweep reclaims it. Path() writes each
// thumbnail to an os.CreateTemp file and atomically renames it into place; a
// crash or SIGKILL in that window strands the temp, and without a sweep they
// accumulate across runs forever (C-07). One hour is comfortably beyond
// thumbGenTimeout (30 s), so an in-flight generation is never mistaken for an
// orphan. Mirrors the webserver's hlsTmpMaxAge .tmp reaper.
const staleTmpMaxAge = time.Hour

// ThumbStore turns Entry → on-disk thumbnail JPEG path. It generates the
// thumbnail on demand if the file does not yet exist.
//
// Two semaphores split CPU-bound decoders (native Go image package) from
// external-tool decoders (heif-convert, exiftool, ffmpeg). The external
// decoders spend most of their time waiting on a child process, so capping
// them at NumCPU under-utilises hardware; CPU-bound decoders, on the other
// hand, must stay capped or the system bogs down under rapid scrolling.
type ThumbStore struct {
	dir    string // <cache>/thumbs (or <cache>/display for a display store)
	size   int    // target longest-edge for generated renditions
	cpuSem chan struct{}
	extSem chan struct{}

	// Singleflight: concurrent Path() calls for the same thumb id share one
	// generation. The first caller stores a channel here; followers wait on
	// it and re-stat the result. Keyed by thumb id.
	flightMu sync.Mutex
	inflight map[string]chan struct{}

	// tiers are sibling stores of other sizes created through Tier. Forget
	// and Rename on this store apply to them too, so trashing or deleting a
	// file doesn't orphan its larger renditions.
	tierMu sync.Mutex
	tiers  map[string]*ThumbStore
}

func NewThumbStore(cacheDir string) (*ThumbStore, error) {
	return newStore(cacheDir, "thumbs", ThumbSize, nil, nil)
}

// NewDisplayStore constructs a second store rooted at <cacheDir>/display that
// generates larger (DisplaySize) JPEG renditions. It shares every bit of
// ThumbStore's machinery — sharding, singleflight, mtime staleness, atomic
// rename — differing only in the on-disk subdirectory and the target size, so
// the webserver gets browser-renderable images for RAW/HEIC/TIFF and oversized
// originals for free (W-03). Wipe <cacheDir>/display alongside thumbs/ and hls/
// on a rebuild.
func NewDisplayStore(cacheDir string) (*ThumbStore, error) {
	return newStore(cacheDir, DisplayDir, DisplaySize, nil, nil)
}

// newStore builds a store rooted at <cacheDir>/<subdir> whose generated
// renditions fit within size×size. NewThumbStore and NewDisplayStore differ
// only in the subdirectory and size parameters and pass nil semaphores, so each
// independent store root owns its own generation pools; Tier passes the parent
// store's semaphores so all its sizes together stay within one CPU /
// external-tool bound. A nil cpuSem is allocated at max(runtime.NumCPU(), 2)
// and a nil extSem at three times that width (at least 6).
func newStore(cacheDir, subdir string, size int, cpuSem, extSem chan struct{}) (*ThumbStore, error) {
	d := filepath.Join(cacheDir, subdir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	if cpuSem == nil {
		cpuSem = make(chan struct{}, max(runtime.NumCPU(), 2))
	}
	if extSem == nil {
		extSem = make(chan struct{}, max(cap(cpuSem)*3, 6))
	}
	// Reclaim any "*.tmp" renditions a previous run's crash stranded between
	// os.CreateTemp and rename. Run in the background so the walk never blocks
	// store construction (mirrors the webserver kicking its HLS sweep off a
	// goroutine at init). Targets this store's own root, so a display store
	// gets swept alongside the thumb store (W-03) for free.
	go sweepStaleTmp(d, staleTmpMaxAge, time.Now())
	return &ThumbStore{
		dir:      d,
		size:     size,
		cpuSem:   cpuSem,
		extSem:   extSem,
		inflight: make(map[string]chan struct{}),
	}, nil
}

// Tier returns the sibling store at <cacheDir>/<subdir> generating size px
// renditions, creating it on first use. Asking for an existing subdir with a
// different size is an error, since that subdir's renditions are already
// generated at the size it was created with. A tier shares this store's
// generation semaphores, so all sizes together stay within one CPU /
// external-tool bound, and follows this store's Forget and Rename.
func (s *ThumbStore) Tier(subdir string, size int) (*ThumbStore, error) {
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	if t, ok := s.tiers[subdir]; ok {
		if t.size != size {
			return nil, fmt.Errorf("cache: tier %q already exists with size %d, not %d", subdir, t.size, size)
		}
		return t, nil
	}
	t, err := newStore(s.CacheDir(), subdir, size, s.cpuSem, s.extSem)
	if err != nil {
		return nil, err
	}
	if s.tiers == nil {
		s.tiers = map[string]*ThumbStore{}
	}
	s.tiers[subdir] = t
	return t, nil
}

func (s *ThumbStore) tierList() []*ThumbStore {
	s.tierMu.Lock()
	defer s.tierMu.Unlock()
	out := make([]*ThumbStore, 0, len(s.tiers))
	for _, t := range s.tiers {
		out = append(out, t)
	}
	return out
}

// sweepStaleTmp walks root once, removing crash-orphaned "*.tmp" files older
// than tmpMaxAge. Only files matching the ".tmp" suffix are ever removed — the
// final ".jpg" renditions are never touched — and only when their mtime is
// beyond the threshold, so an in-flight generation's fresh temp survives.
// Best-effort throughout: an unreadable or vanished entry (including a missing
// root) is skipped rather than aborting the walk. Split from newStore with
// explicit age/now parameters so it's testable without racing wall-clock time.
// Mirrors the webserver's sweepHLSDir .tmp reaper.
func sweepStaleTmp(root string, tmpMaxAge time.Duration, now time.Time) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable / vanished dir (or missing root): skip
		}
		if d.IsDir() || !strings.HasSuffix(path, ".tmp") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if now.Sub(info.ModTime()) > tmpMaxAge {
			os.Remove(path)
		}
		return nil
	})
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
	return s.fresh(s.thumbPath(e.ThumbID), e.ModTime)
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
	if s.fresh(dst, e.ModTime) {
		return dst, nil
	}

	// Singleflight: dedupe concurrent calls for the same thumb id.
	s.flightMu.Lock()
	if ch, ok := s.inflight[e.ThumbID]; ok {
		s.flightMu.Unlock()
		<-ch
		if s.fresh(dst, e.ModTime) {
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

	// Re-check under the flight: a prior leader may have generated the thumb and
	// closed its channel in the window between our fast-path stat above and our
	// acquiring the flight.
	if s.fresh(dst, e.ModTime) {
		return dst, nil
	}

	// Unique temp name. The GUI and pv-scan share this cache directory, so a
	// fixed "<id>.jpg.tmp" would collide across processes and let one truncate
	// the other's half-written thumb; os.CreateTemp gives each writer its own.
	// We only need the name — the thumbnailers (re)write it via their own fd —
	// so close our handle immediately.
	// The shard dir is only created when CreateTemp reports it missing, so the
	// common case (shard already present) pays no MkdirAll.
	tmpf, err := os.CreateTemp(filepath.Dir(dst), e.ThumbID+"-*.tmp")
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		tmpf, err = os.CreateTemp(filepath.Dir(dst), e.ThumbID+"-*.tmp")
	}
	if err != nil {
		return "", err
	}
	tmp := tmpf.Name()
	tmpf.Close()

	// Bound generation so a wedged decoder can't hold the semaphore slot (or
	// the singleflight channel) forever. On timeout generate returns the
	// context error just like any other failure: the deferred close(ch) above
	// still fires, releasing followers, and the caller falls back to a
	// placeholder — never a hang. cancel is deferred to after the rename so
	// the ctx outlives the whole generation, then is released promptly.
	ctx, cancel := context.WithTimeout(context.Background(), thumbGenTimeout)
	defer cancel()

	sem := s.semFor(e.Type)
	sem <- struct{}{}
	err = s.generate(ctx, e, tmp)
	<-sem

	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	// No fsync before the rename (G4): it cost one disk flush per thumbnail,
	// which dominated first-time generation and pv-scan on HDDs and SD cards.
	// A crash before writeback can still leave a zero-byte or truncated file
	// behind the rename; fresh() rejects zero-byte files, and readers that
	// fail to decode a thumb call Forget and regenerate it (self-healing).
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// fresh reports whether a non-empty thumbnail at dst exists and is at least as
// new as the source's mod time (i.e. not stale). A zero-byte file, which a
// crash between rename and writeback can leave, counts as missing.
func (s *ThumbStore) fresh(dst string, srcMod time.Time) bool {
	info, err := os.Stat(dst)
	return err == nil && info.Size() > 0 && !info.ModTime().Before(srcMod)
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
		return thumb.Image(ctx, e.Path, dst, s.size)
	case scan.TypeRAW:
		return thumb.RAW(ctx, e.Path, dst, s.size)
	case scan.TypeHEIC:
		return thumb.HEIC(ctx, e.Path, dst, s.size)
	case scan.TypeVideo:
		return thumb.Video(ctx, e.Path, dst, s.size)
	}
	return fmt.Errorf("unsupported media type: %s", e.Type)
}

// Forget deletes the thumbnail file for the given thumb id, if any, in this
// store and its tiers.
func (s *ThumbStore) Forget(id string) {
	os.Remove(s.thumbPath(id))
	for _, t := range s.tierList() {
		t.Forget(id)
	}
}

// Discard deletes this store's thumbnail for id only, leaving its tiers alone.
// Readers call it on a thumbnail that exists but won't decode, so the next
// Path regenerates it.
func (s *ThumbStore) Discard(id string) {
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
	for _, t := range s.tierList() {
		_ = t.Rename(oldID, newID)
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
