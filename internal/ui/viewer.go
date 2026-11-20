package ui

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/imgorient"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
	"github.com/dns/photo-viewer/internal/video"
)

// Viewer renders the currently selected entry over the whole window. Open
// is true when the viewer should claim the screen. ShowInfo toggles the
// metadata side panel (bound to 'i').
type Viewer struct {
	Open     bool
	Index    int
	ShowInfo bool

	// Confirming is true while the delete-confirmation modal is up. While
	// true, Enter triggers the deletion and Esc/Ctrl+[ dismiss.
	Confirming bool

	// InTrash is set by the window each frame to mirror whether the active
	// view is the Trash sentinel. When true, the confirm modal copy flips
	// to "Restore this file from trash?" since DeletePath repurposes the
	// keybind as Restore for trash items.
	InTrash bool

	// entries is the list being paged through. Show stores the caller's
	// slice as is (G4-03); entriesOwned turns true once mutableEntries has
	// replaced it with a private copy. UI goroutine only.
	entries      []cache.Entry
	entriesOwned bool

	// loadingPath/loadingTarget identify the main (visible-image) decode in
	// flight, so Layout doesn't restart it every frame. UI goroutine only.
	loadingPath      string
	loadingTarget    image.Point
	loadingCtxCancel context.CancelFunc

	// recent caches up to viewerRecentCap decoded images, the visible one
	// included, so paging back and forth and reopening the viewer don't
	// re-decode. Ordered MRU→LRU. It survives Close: entries are keyed by path
	// and source mtime, so a changed file is never served stale.
	//
	// Touched both from the layout goroutine and from background decode
	// goroutines (main load + neighbour prefetch), so all access goes
	// through recentMu, as does inflight.
	recentMu sync.Mutex
	recent   []recentImage
	// inflight holds every running prefetch / warm decode by path. A main
	// load for the same path adopts the job instead of decoding it twice (V4).
	inflight map[string]*decodeJob

	initOnce    sync.Once
	prefetchSem chan struct{} // limits concurrent prefetch decodes; waiters queue
	mainSem     chan struct{} // limits concurrent main decodes (cancelled ones drain)

	// target is the image area of the last Layout (packed by setTarget), the
	// box decodes fit to (V2). Read by background prefetches.
	target atomic.Uint64

	// lastNav / rapidNav detect key repeat: when two navigations land within
	// viewerRepeatWindow, decodes wait viewerSettleDelay for the index to
	// settle and the thumbnail stays up meanwhile (V5). navAt is when the
	// current image was requested, for the debug timing log. UI goroutine only.
	lastNav  time.Time
	rapidNav bool
	navAt    time.Time

	// warmPath / warmJob track the grid-selection warm-up decode (V7).
	warmPath string
	warmJob  *decodeJob

	// display is the on-disk ~2048 px rendition store used for HEIC and RAW
	// (V8). Set once at startup before any decode; nil disables it.
	display *cache.ThumbStore

	// curIdx mirrors Index for cross-goroutine reads. Index itself is only
	// written on the UI goroutine (lock-free), so background prefetch
	// goroutines can't read it directly without racing the -race detector.
	// Every writer of Index also stores it here (via syncIndex); prefetch
	// stale-checks load it instead of touching Index.
	curIdx atomic.Int64

	// Decoded dimensions are cached per path. Decoding happens off the UI
	// goroutine so opening the viewer doesn't stutter on big originals.
	dimMu    sync.Mutex
	dimCache map[string]string
	dimAsked map[string]bool

	// Media info (created, camera, lens) is fetched in the background.
	infoMu    sync.Mutex
	infoCache map[string]cachedInfo
	infoAsked map[string]bool

	// infoRows is the info panel's formatted text, rebuilt only when infoKey
	// changes (G3-03); infoBuilds counts rebuilds for tests. UI goroutine only.
	infoKey    infoPanelKey
	infoRows   []infoRow
	infoBuilds int

	// player is lazily created the first time the viewer lands on a video.
	// It's kept alive between videos so navigation between clips doesn't
	// pay the libmpv-init cost each time; the player itself is also a
	// no-op when libmpv failed to load on this machine.
	// player is stored atomically: only the preload goroutine constructs it
	// (ensurePlayer, under playerOnce), because libmpv/EGL init takes 59–86 ms
	// and must never run on the frame goroutine (G4-01). Layout, the key
	// handlers, Close and stopVideoIfRunning just Load it, and treat nil as
	// "not ready yet" (or libmpv unavailable).
	playerOnce sync.Once
	player     atomic.Pointer[video.Player]

	// lastVideoOp is the most recent frame playVideo handed to Gio, keyed by
	// the file and target size it was rendered for. Gio redraws for many
	// reasons besides a new video frame (cursor motion, overlays, a paused
	// clip); reusing the op keeps its texture handle, so those frames skip
	// both the mpv render and the RAM→GPU upload. It aliases the player's
	// frame buffer, so it is dropped when playback stops. UI goroutine only.
	lastVideoOp   paint.ImageOp
	lastVideoPath string
	lastVideoSize image.Point // render size the op was made for (cache key)
	// lastVideoDraw is the size lastVideoOp should be painted at. It equals
	// lastVideoSize except when a native-size render (V5-02) is painted scaled
	// into the image area; keeping it separately means a cached frame is also
	// drawn correctly.
	lastVideoDraw image.Point

	invalidate func()
}

// viewerRecentCap is the size of the recently-decoded image LRU. Entries are
// decoded to fit the viewer's image area (at most 2× it per axis, usually
// within 15 %), so on a 1440p window each is ~10–15 MB and six of them stay
// well under 100 MB. Six holds the current image, the +1/+2/−1 prefetches and
// two more of history.
const viewerRecentCap = 6

// viewerPrefetchSlots bounds concurrent prefetch decodes. Excess requests wait
// for a slot rather than being dropped; stale ones are cancelled.
const viewerPrefetchSlots = 2

// viewerMainSlots bounds concurrent main decodes. A cancelled libjpeg-turbo
// decode can't be interrupted and may hold a slot for ~100 ms; two slots let
// the newest request start right away in that case.
const viewerMainSlots = 2

// viewerRepeatWindow and viewerSettleDelay implement the key-repeat debounce:
// a navigation within viewerRepeatWindow of the previous one waits
// viewerSettleDelay before decoding, and is cancelled if another arrives.
const (
	viewerRepeatWindow = 150 * time.Millisecond
	viewerSettleDelay  = 50 * time.Millisecond
	// viewerResizeDelay debounces re-decodes when the window grows.
	viewerResizeDelay = 120 * time.Millisecond
	// viewerWarmDelay is how long the grid selection must rest on an entry
	// before it is decoded ahead of Enter.
	viewerWarmDelay = 250 * time.Millisecond
)

// viewerStaleDistance is how far (in entries) a prefetch target may fall
// behind the current index before the prefetch is cancelled.
const viewerStaleDistance = 2

// viewerMetaCap is the soft cap on entries in dimCache/infoCache (and their
// "already asked" sidecars). The maps are bulk-cleared when they exceed the
// cap so the viewer's metadata cache can't grow unboundedly as the user
// pages through a large library. Recomputing is cheap (one ExifTool / image
// header read) so a periodic clear is preferable to running a full LRU.
const viewerMetaCap = 1024

// viewerMaxPreviewSide caps the long side of any decoded image, and is the
// square fit target used before the first layout reports the real image area.
// Decodes normally fit the window (see decodeOriginal); full resolution is
// reserved for a future zoom mode.
const viewerMaxPreviewSide = 4096

// recentImage is one decoded image in the recent cache.
type recentImage struct {
	path string
	mod  time.Time // source mtime when decoded; a mismatch is a miss
	op   paint.ImageOp
	sz   image.Point // decoded (upright) size
	// native is the upright size of the best source available, or zero when
	// unknown (a display-store rendition). full means sz already is the
	// source's full resolution, so a larger window can't gain anything.
	native image.Point
	full   bool
}

// decodedImage is what decodeOriginal returns.
type decodedImage struct {
	op     paint.ImageOp
	sz     image.Point
	native image.Point
	full   bool
}

// decodeJob is one background prefetch / warm decode.
type decodeJob struct {
	done   chan struct{} // closed when the job finishes, successful or not
	cancel context.CancelFunc
	idx    int // entry index it was started for; -1 when not index-bound
	// adopted is set when a main load waits on this job; such a job is
	// never cancelled as stale.
	adopted atomic.Bool
	// started is set once the job holds a slot and is decoding.
	started atomic.Bool
}

type cachedInfo struct {
	Created      string
	Camera       string
	Lens         string
	Aperture     string
	ShutterSpeed string
	ISO          string
	FocalLength  string
}

// SetInvalidate wires a callback used to wake the Gio frame loop after the
// background dimension decoder finishes.
func (v *Viewer) SetInvalidate(f func()) { v.invalidate = f }

// syncIndex mirrors the UI-goroutine-owned Index into the atomic curIdx so
// background goroutines (prefetch stale-checks) can read the current position
// without racing. Call it from every site that writes Index.
func (v *Viewer) syncIndex() { v.curIdx.Store(int64(v.Index)) }

// init lazily allocates the semaphores and maps, so a zero Viewer works.
func (v *Viewer) init() {
	v.initOnce.Do(func() {
		v.prefetchSem = make(chan struct{}, viewerPrefetchSlots)
		v.mainSem = make(chan struct{}, viewerMainSlots)
		v.recentMu.Lock()
		v.inflight = map[string]*decodeJob{}
		v.recentMu.Unlock()
	})
}

// SetDisplayStore wires the on-disk display-rendition store used to cache
// HEIC and RAW decodes. Call before the viewer is first shown.
// It also reaps HEIC staging dirs a crashed run left in the cache dir.
func (v *Viewer) SetDisplayStore(s *cache.ThumbStore) {
	v.display = s
	if s != nil {
		go sweepHEICStages(s.CacheDir(), time.Hour, time.Now())
	}
}

// sweepHEICStages removes heicStageDir directories under dir older than
// maxAge: ones a crash stranded between MkdirTemp and the deferred RemoveAll.
func sweepHEICStages(dir string, maxAge time.Duration, now time.Time) {
	matches, _ := filepath.Glob(filepath.Join(dir, heicStagePrefix+"*"))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() && now.Sub(fi.ModTime()) > maxAge {
			_ = os.RemoveAll(m)
		}
	}
}

// setTarget records the image area decodes should fit to.
func (v *Viewer) setTarget(sz image.Point) {
	if sz.X <= 0 || sz.Y <= 0 {
		return
	}
	v.target.Store(uint64(uint32(sz.X))<<32 | uint64(uint32(sz.Y)))
}

// targetSize returns the last image area, or a viewerMaxPreviewSide square
// before the first layout.
func (v *Viewer) targetSize() image.Point {
	t := v.target.Load()
	if t == 0 {
		return image.Pt(viewerMaxPreviewSide, viewerMaxPreviewSide)
	}
	return image.Pt(int(uint32(t>>32)), int(uint32(t)))
}

// NoteWindowSize lets the window report the viewer's would-be area while the
// viewer is closed, so warm-up decodes before the first open fit the window.
func (v *Viewer) NoteWindowSize(gtx layout.Context, size image.Point) {
	if v.ShowInfo {
		size.X -= min(gtx.Dp(unit.Dp(300)), size.X/2)
	}
	v.setTarget(size)
}

// navigated records a navigation for the key-repeat debounce.
func (v *Viewer) navigated() {
	now := time.Now()
	v.rapidNav = !v.lastNav.IsZero() && now.Sub(v.lastNav) < viewerRepeatWindow
	v.lastNav = now
	v.navAt = now
}

func (v *Viewer) Show(entries []cache.Entry, idx int) {
	// Share the slice (G4-03): the caller hands us the controller's published
	// entries (via Snapshot), which the controller never mutates and the grid
	// keeps reading. Copying it cost 1.9 ms and 8.8 MB per Enter at 100k
	// entries. The viewer's own writers (ConfirmDelete, the 'F' toggle,
	// SetFavorite) go through mutableEntries, which clones on first write.
	v.init()
	v.entries = entries
	v.entriesOwned = false
	v.Index = idx
	v.syncIndex()
	v.Open = true
	v.rapidNav = false
	v.navAt = time.Now()
	v.cancelLoading()
	v.loadingPath = ""
	v.warmPath = ""
	v.preloadVideoIfNeeded()
	v.prefetchNeighbours(1)
}

// SetFavorite updates the viewer's own copy of path's favorite flag — used
// when the controller undoes a toggle whose write failed.
func (v *Viewer) SetFavorite(path string, fav bool) {
	for i := range v.entries {
		if v.entries[i].Path == path && v.entries[i].Favorite != fav {
			v.mutableEntries()[i].Favorite = fav
		}
	}
}

// mutableEntries returns v.entries, first replacing a slice still shared with
// the controller by a private copy. Every write to the viewer's entries goes
// through it.
func (v *Viewer) mutableEntries() []cache.Entry {
	if !v.entriesOwned {
		v.entries = slices.Clone(v.entries)
		v.entriesOwned = true
	}
	return v.entries
}

// removeEntry drops entries[i], copying a shared slice rather than shifting
// the controller's array in place.
func (v *Viewer) removeEntry(i int) {
	if v.entriesOwned {
		v.entries = slices.Delete(v.entries, i, i+1)
		return
	}
	v.entries = slices.Concat(v.entries[:i], v.entries[i+1:])
	v.entriesOwned = true
}

func (v *Viewer) Close() {
	v.Open = false
	v.Confirming = false
	v.loadingPath = ""
	v.cancelLoading()
	// Keep v.recent (V7): reopening a photo just viewed is instant. Cancel
	// the prefetches, which would otherwise decode images nobody will see.
	v.recentMu.Lock()
	for _, j := range v.inflight {
		j.cancel()
	}
	v.recentMu.Unlock()
	if p := v.player.Load(); p != nil {
		p.Stop()
	}
	// Stop released the player's frame buffer; drop our op too, or it keeps
	// the window-sized RGBA (8–33 MB) alive until the next video (G4-06).
	v.dropVideoFrame()
}

// dropVideoFrame forgets the last video frame handed to Gio. The op aliases
// the player's frame buffer, so it must go whenever playback stops.
func (v *Viewer) dropVideoFrame() {
	v.lastVideoOp, v.lastVideoPath, v.lastVideoSize, v.lastVideoDraw = paint.ImageOp{}, "", image.Point{}, image.Point{}
}

// SetVideoVisible tells the player whether its output is on screen. The window
// calls it with false while a modal covers the viewer, so a playing clip stops
// redrawing at its frame rate behind the dialog (G5-04). Layout switches it
// back on when it draws the video. Safe with no player yet.
func (v *Viewer) SetVideoVisible(visible bool) {
	if p := v.player.Load(); p != nil {
		p.SetVisible(visible)
	}
}

// stopVideoIfRunning halts mpv playback whenever the current entry isn't a
// video. Called from Next/Prev so paging away from a clip releases its
// decoder; the player itself stays warm for the next video.
func (v *Viewer) stopVideoIfRunning() {
	p := v.player.Load()
	if p == nil {
		return
	}
	if v.Index < 0 || v.Index >= len(v.entries) || v.entries[v.Index].Type != scan.TypeVideo {
		p.Stop()
		v.dropVideoFrame()
	}
}

// preloadVideoIfNeeded kicks an mpv loadfile for the current entry if it's a
// video, in a goroutine so the UI thread doesn't pay the initial libmpv
// initialisation cost on first call. Subsequent calls are cheap. By the time
// Layout runs and calls playVideo, mpv is already demuxing — first-frame
// latency drops by however long Layout would otherwise have spent waiting.
func (v *Viewer) preloadVideoIfNeeded() {
	if v.Index < 0 || v.Index >= len(v.entries) {
		return
	}
	e := v.entries[v.Index]
	if e.Type != scan.TypeVideo {
		return
	}
	go func(path string) {
		p, created := v.ensurePlayer()
		if p == nil {
			return
		}
		if p.Current() != path {
			_ = p.Load(path)
		}
		if created && v.invalidate != nil {
			// Layout showed the thumbnail while the player was being built;
			// wake it now so it starts drawing frames.
			v.invalidate()
		}
	}(e.Path)
}

// cachePut inserts img for e at the MRU end of v.recent, replacing any older
// decode of the same path and evicting the oldest entry past viewerRecentCap.
// A smaller decode never replaces a larger one of the same source version (a
// late prefetch landing after an upgrade).
func (v *Viewer) cachePut(e cache.Entry, d decodedImage) {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	// A smaller decode never replaces a larger one of the same source version
	// (a late prefetch landing after an upgrade).
	for _, r := range v.recent {
		if r.path == e.Path {
			if r.mod.Equal(e.ModTime) && r.sz.X > d.sz.X {
				return
			}
			break
		}
	}
	v.cacheDropLocked(e.Path)
	r := recentImage{path: e.Path, mod: e.ModTime, op: d.op, sz: d.sz, native: d.native, full: d.full}
	v.recent = append([]recentImage{r}, v.recent...)
	if len(v.recent) > viewerRecentCap {
		v.recent = v.recent[:viewerRecentCap]
	}
}

// cacheDropLocked removes the first recent entry for path, reporting whether
// there was one. The caller holds recentMu.
func (v *Viewer) cacheDropLocked(path string) bool {
	for i, r := range v.recent {
		if r.path == path {
			v.recent = append(v.recent[:i], v.recent[i+1:]...)
			return true
		}
	}
	return false
}

// cacheGet returns the cached image for e and moves it to MRU. An entry for
// the same path but a different source mtime is dropped and reported missing.
func (v *Viewer) cacheGet(e cache.Entry) (recentImage, bool) {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for i, r := range v.recent {
		if r.path != e.Path {
			continue
		}
		if !r.mod.Equal(e.ModTime) {
			v.recent = append(v.recent[:i], v.recent[i+1:]...)
			return recentImage{}, false
		}
		if i != 0 {
			copy(v.recent[1:i+1], v.recent[:i])
			v.recent[0] = r
		}
		return r, true
	}
	return recentImage{}, false
}

// cacheHasEntry is cacheHas that also requires the source mtime to match.
func (v *Viewer) cacheHasEntry(e cache.Entry) bool {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for _, r := range v.recent {
		if r.path == e.Path && r.mod.Equal(e.ModTime) {
			return true
		}
	}
	return false
}

// RequestDelete opens the delete-confirmation modal for the current entry.
// Has no effect if the viewer isn't open or there is no current entry.
func (v *Viewer) RequestDelete() {
	if !v.Open || v.Index < 0 || v.Index >= len(v.entries) {
		return
	}
	v.Confirming = true
}

// CancelDelete dismisses the confirmation modal without deleting anything.
func (v *Viewer) CancelDelete() { v.Confirming = false }

// ConfirmDelete invokes deleter for the current entry's path, removes it
// from the local entry list, and advances to the next entry. If the list
// becomes empty the viewer closes.
func (v *Viewer) ConfirmDelete(deleter func(path string) error) {
	if !v.Confirming || v.Index < 0 || v.Index >= len(v.entries) {
		v.Confirming = false
		return
	}
	e := v.entries[v.Index]
	v.Confirming = false
	if err := deleter(e.Path); err != nil {
		return
	}
	v.removeEntry(v.Index)
	if len(v.entries) == 0 {
		v.Close()
		return
	}
	if v.Index >= len(v.entries) {
		v.Index = len(v.entries) - 1
	}
	v.syncIndex()
	// Drop the deleted entry from the recent cache too — the path no
	// longer exists, and the cache entry would never get evicted otherwise.
	v.recentMu.Lock()
	v.cacheDropLocked(e.Path)
	v.recentMu.Unlock()
	v.cancelLoading()
	v.loadingPath = ""
	v.navAt = time.Now()
	// Same hand-off as Next/Prev: stop a video that is no longer current
	// (Layout only drives the player for a video entry, so a deleted clip
	// followed by a photo would keep playing), preload the new current
	// video, and warm the neighbours so the next step isn't a blurry
	// thumbnail fallback.
	v.stopVideoIfRunning()
	v.preloadVideoIfNeeded()
	v.cancelStalePrefetches()
	v.prefetchNeighbours(1)
}

func (v *Viewer) cancelLoading() {
	if v.loadingCtxCancel != nil {
		v.loadingCtxCancel()
		v.loadingCtxCancel = nil
	}
}

func (v *Viewer) Next() {
	if v.Index < len(v.entries)-1 {
		v.step(1)
	}
}

func (v *Viewer) Prev() {
	if v.Index > 0 {
		v.step(-1)
	}
}

// step moves the index by dir (±1) and hands off loading: the old main decode
// is cancelled, prefetches that fell out of range are cancelled, and the next
// two entries in the direction of travel plus the one behind are prefetched.
func (v *Viewer) step(dir int) {
	v.init()
	v.Index += dir
	v.syncIndex()
	v.navigated()
	v.cancelLoading()
	v.loadingPath = ""
	v.stopVideoIfRunning()
	v.preloadVideoIfNeeded()
	v.cancelStalePrefetches()
	v.prefetchNeighbours(dir)
}

// prefetchNeighbours prefetches +1 and +2 in direction dir and −1 behind,
// nearest first so the queue serves them in that order.
func (v *Viewer) prefetchNeighbours(dir int) {
	v.prefetchAt(v.Index + dir)
	v.prefetchAt(v.Index - dir)
	v.prefetchAt(v.Index + 2*dir)
}

// cancelStalePrefetches cancels index-bound prefetches that are now more than
// viewerStaleDistance away, unless a main load adopted them.
func (v *Viewer) cancelStalePrefetches() {
	cur := v.Index
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for _, j := range v.inflight {
		if j.idx < 0 || j.adopted.Load() {
			continue
		}
		if d := j.idx - cur; d < -viewerStaleDistance || d > viewerStaleDistance {
			j.cancel()
		}
	}
}

// newPlayer constructs the libmpv player. A variable so tests can stand in a
// slow or failing constructor.
var newPlayer = video.New

// ensurePlayer returns the lazily-initialised libmpv player, or nil if libmpv
// failed to load, constructing it on the first call; created reports whether
// this call did. It blocks for the whole libmpv/EGL init, so only the preload
// goroutine may call it; everything on the frame goroutine uses player.Load.
func (v *Viewer) ensurePlayer() (p *video.Player, created bool) {
	v.playerOnce.Do(func() {
		p, err := newPlayer(v.invalidate)
		if err != nil {
			log.Printf("video: player unavailable: %v", err)
			p = nil
		}
		v.player.Store(p)
		created = true
	})
	return v.player.Load(), created
}

// playVideo loads the current entry into mpv if it isn't already, renders the
// current frame, and returns it wrapped as a paint.ImageOp together with the
// pixel size it was rendered at, which is also the size to paint it at. A clip
// smaller than (w, h) is rendered at its native size and the returned size is
// that smaller rect, so the caller must scale it up to the image area (V5-02);
// a clip at least as large as the area keeps rendering at (w, h) where mpv
// letterboxes inside the rect itself. Returns ok false when libmpv is
// unavailable, the player is still being built by the preload goroutine, or no
// frame is ready yet; Layout then shows the thumbnail. It never constructs the
// player itself (G4-01).
func (v *Viewer) playVideo(e cache.Entry, w, h int) (op paint.ImageOp, size image.Point, ok bool) {
	p := v.player.Load()
	if p == nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	if p.Current() != e.Path {
		if err := p.Load(e.Path); err != nil {
			return paint.ImageOp{}, image.Point{}, false
		}
	}
	// Render small clips at their native size and let the GPU magnify: mpv
	// then does no scaling at all per frame, and the result is as sharp as the
	// source. A clip at least as large as the area keeps the window-size render
	// (mpv's GPU downscale is the good path there).
	rw, rh := w, h
	upscale := false
	if nw, nh := p.NativeSize(); nw > 0 && nh > 0 && nw <= w && nh <= h && (nw < w || nh < h) {
		rw, rh = nw, nh
		upscale = true
	}
	sz := image.Pt(rw, rh)
	if !p.NeedsRedraw() && v.lastVideoPath == e.Path && v.lastVideoSize == sz {
		return v.lastVideoOp, v.lastVideoDraw, true
	}
	frame, ok := p.Render(rw, rh)
	if !ok {
		return paint.ImageOp{}, image.Point{}, false
	}
	op = paint.NewImageOp(frame)
	if upscale {
		// The GPU magnifies the native-size frame, so it needs linear
		// filtering. Gio still builds a mip chain for a FilterLinear upload,
		// but at the clip's native size that costs less than mpv scaling,
		// reading back and uploading a window-size frame.
		op.Filter = paint.FilterLinear
	} else {
		// mpv already rendered at imgArea pixels, so the frame is drawn 1:1:
		// FilterLinear would make Gio build (and never sample) a full mip
		// chain on every frame upload (V5-01).
		op.Filter = paint.FilterNearest
	}
	v.lastVideoOp, v.lastVideoPath, v.lastVideoSize, v.lastVideoDraw = op, e.Path, sz, sz
	return op, sz, true
}

// Layout fills gtx.Constraints with a black background and the active
// image scaled to fit. Videos and unsupported formats fall back to a
// placeholder color. When ShowInfo is true a sidebar panel is laid out on
// the right with the entry's metadata.
func (v *Viewer) Layout(gtx layout.Context, th *Theme, tc *thumbCache) layout.Dimensions {
	size := gtx.Constraints.Max
	rect := image.Rectangle{Max: size}
	clipArea := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{A: 0xff}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	if v.Index < 0 || v.Index >= len(v.entries) {
		clipArea.Pop()
		return layout.Dimensions{Size: size}
	}
	e := v.entries[v.Index]

	imgArea := rect
	infoW := 0
	if v.ShowInfo {
		infoW = gtx.Dp(unit.Dp(300))
		infoW = min(infoW, size.X/2)
		imgArea = image.Rectangle{Max: image.Pt(size.X-infoW, size.Y)}
	}

	if e.Type == scan.TypeVideo {
		// Drawing the video makes it visible again after a modal hid it (G5-04).
		v.SetVideoVisible(true)
		if op, size, ok := v.playVideo(e, imgArea.Dx(), imgArea.Dy()); ok {
			if size == imgArea.Size() {
				// mpv already letterboxed to (Dx, Dy), so we paint at
				// imgArea.Min with the rect's native size.
				off := opOffset(gtx, imgArea.Min)
				op.Add(gtx.Ops)
				paint.PaintOp{}.Add(gtx.Ops)
				off.Pop()
			} else {
				// A native-size render (V5-02): mpv letterboxed inside
				// size, whose aspect is the clip's, so fit that to the area.
				drawFitted(gtx, op, size, imgArea)
			}
		} else if imgOp, imgSz, fb := tc.Get(e); fb {
			// While mpv is still warming up or libmpv is unavailable,
			// show the cached thumbnail so the screen isn't blank.
			drawFitted(gtx, imgOp, imgSz, imgArea)
		}
	} else {
		v.setTarget(imgArea.Size())
		imgOp, imgSz, ok := v.imageFor(e, tc, v.targetSize())
		if ok {
			drawFitted(gtx, imgOp, imgSz, imgArea)
		}
	}

	if v.ShowInfo {
		v.layoutInfoPanel(gtx, th, e, image.Rectangle{
			Min: image.Pt(size.X-infoW, 0),
			Max: image.Pt(size.X, size.Y),
		})
	}

	if v.Confirming {
		drawDeleteConfirm(gtx, th, filepath.Base(e.Path), rect, v.InTrash)
	}

	clipArea.Pop()
	return layout.Dimensions{Size: size}
}

// drawDeleteConfirm dims rect and centers a confirmation box showing
// filename and the available shortcuts. Shared between the viewer and grid.
// When restore is true the prompt asks about restoring an item out of the
// trash instead of deleting it, since the same key binding handles both.
func drawDeleteConfirm(gtx layout.Context, th *Theme, filename string, rect image.Rectangle, restore bool) {
	dimStack := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{A: 0xb0}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	dimStack.Pop()

	boxW := gtx.Dp(unit.Dp(420))
	if boxW > rect.Dx()-gtx.Dp(unit.Dp(40)) {
		boxW = rect.Dx() - gtx.Dp(unit.Dp(40))
	}
	pad := gtx.Dp(unit.Dp(20))

	macro := op.Record(gtx.Ops)
	innerGtx := gtx
	innerGtx.Constraints.Max = image.Pt(boxW-pad*2, rect.Dy())
	innerGtx.Constraints.Min = image.Pt(boxW-pad*2, 0)
	contentDims := layout.Flex{Axis: layout.Vertical, Spacing: layout.SpaceEnd}.Layout(innerGtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			title := "Delete this file?"
			if restore {
				title = "Restore this file from trash?"
			}
			lbl := material.Label(th.Theme, unit.Sp(15), title)
			lbl.Color = th.Foreground
			lbl.Font.Weight = 700
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13), filename)
			lbl.Color = th.Foreground
			lbl.MaxLines = 2
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			hint := "Enter to delete    Esc / Ctrl+[ to cancel"
			if restore {
				hint = "Enter to restore    Esc / Ctrl+[ to cancel"
			}
			lbl := material.Label(th.Theme, unit.Sp(12), hint)
			lbl.Color = th.Muted
			return lbl.Layout(gtx)
		}),
	)
	contentCall := macro.Stop()

	boxH := contentDims.Size.Y + pad*2
	boxRect := image.Rectangle{
		Min: image.Pt(rect.Min.X+(rect.Dx()-boxW)/2, rect.Min.Y+(rect.Dy()-boxH)/2),
		Max: image.Pt(rect.Min.X+(rect.Dx()-boxW)/2+boxW, rect.Min.Y+(rect.Dy()-boxH)/2+boxH),
	}
	bg := clip.UniformRRect(boxRect, gtx.Dp(unit.Dp(8))).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{R: 0x1c, G: 0x1f, B: 0x26, A: 0xff}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()
	drawBorder(gtx, boxRect, 1, color.NRGBA{R: 0xc0, G: 0x40, B: 0x40, A: 0xff})

	off := op.Offset(image.Pt(boxRect.Min.X+pad, boxRect.Min.Y+pad)).Push(gtx.Ops)
	contentCall.Add(gtx.Ops)
	off.Pop()
}

// layoutInfoPanel draws the metadata sidebar at rect. The panel has its own
// dark background to keep it readable over photo content.
func (v *Viewer) layoutInfoPanel(gtx layout.Context, th *Theme, e cache.Entry, rect image.Rectangle) {
	// Background fill.
	bg := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xee}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()

	dims := v.dimensionsFor(e)
	info := v.mediaInfoFor(e)

	pad := gtx.Dp(unit.Dp(12))
	innerW := max(rect.Dx()-pad*2, 80)
	innerH := max(rect.Dy()-pad*2, 60)
	innerGtx := gtx
	innerGtx.Constraints.Max = image.Pt(innerW, innerH)
	innerGtx.Constraints.Min = image.Pt(0, 0)
	clipArea := clip.Rect(rect).Push(gtx.Ops)
	defer clipArea.Pop()
	off := opOffset(gtx, image.Pt(rect.Min.X+pad, rect.Min.Y+pad))
	defer off.Pop()

	// The strings only change with the entry or its async metadata, so they
	// are built once per change, not every frame (G3-03), and the rows are
	// stacked directly instead of through ~30 per-frame Flex closures.
	key := infoPanelKey{path: e.Path, mod: e.ModTime.UnixNano(), size: e.Size, typ: e.Type, fav: e.Favorite, dims: dims, info: info}
	if key != v.infoKey || v.infoRows == nil {
		v.infoKey = key
		v.infoRows = buildInfoRows(e, info, dims)
		v.infoBuilds++
	}

	y := 0
	put := func(lbl material.LabelStyle) {
		g := innerGtx
		g.Constraints.Max.Y = innerH - y
		if g.Constraints.Max.Y <= 0 {
			return
		}
		st := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
		y += lbl.Layout(g).Size.Y
		st.Pop()
	}
	title := material.Label(th.Theme, unit.Sp(14), v.infoRows[0].value)
	title.Color = th.Foreground
	title.Font.Weight = 700
	title.MaxLines = 3
	put(title)
	y += gtx.Dp(unit.Dp(10))
	rowGap := gtx.Dp(unit.Dp(8))
	for _, r := range v.infoRows[1:] {
		lbl := material.Label(th.Theme, unit.Sp(11), r.label)
		lbl.Color = infoLabelColor
		lbl.Font.Weight = 700
		put(lbl)
		val := material.Label(th.Theme, unit.Sp(12), r.value)
		val.Color = th.Foreground
		val.MaxLines = 4
		put(val)
		y += rowGap
	}
}

// infoLabelColor is the info panel's row-label colour.
var infoLabelColor = color.NRGBA{R: 0x90, G: 0x95, B: 0xa0, A: 0xff}

// infoRow is one label/value line of the info panel.
type infoRow struct{ label, value string }

// infoPanelKey is everything the info panel's text depends on.
type infoPanelKey struct {
	path string
	mod  int64
	size int64
	typ  scan.MediaType
	fav  bool
	dims string
	info cachedInfo
}

// buildInfoRows formats the info panel's text for e. Row 0 is the title (the
// file name, value only); the rest are label/value pairs. mediaInfoFor already
// substitutes "—" for missing fields.
func buildInfoRows(e cache.Entry, info cachedInfo, dims string) []infoRow {
	rows := []infoRow{
		{value: filepath.Base(e.Path)},
		{"Path", e.Path},
		{"Type", titleCaseGio(e.Type.String())},
		{"Size", formatBytesGio(e.Size)},
		{"Created", info.Created},
		{"Modified", e.ModTime.Local().Format("2006-01-02 15:04:05")},
		{"Dimensions", dims},
		{"Camera", info.Camera},
		{"Lens", info.Lens},
	}
	if info.Aperture != "—" || info.ShutterSpeed != "—" || info.ISO != "—" || info.FocalLength != "—" {
		rows = append(rows, infoRow{"Settings", fmt.Sprintf("%s  %s  ISO %s  %s",
			info.Aperture, info.ShutterSpeed, info.ISO, info.FocalLength)})
	}
	fav := "No"
	if e.Favorite {
		fav = "Yes"
	}
	return append(rows, infoRow{"Favorite", fav})
}

// dimensionsFor returns a "WxH" string for image entries, "—" for video/RAW/
// HEIC, or "…" while a background decode is still running. The decode is
// kicked off lazily once per path.
func (v *Viewer) dimensionsFor(e cache.Entry) string {
	if e.Type == scan.TypeRAW || e.Type == scan.TypeHEIC || e.Type == scan.TypeVideo {
		return "—"
	}
	v.dimMu.Lock()
	if v.dimCache == nil {
		v.dimCache = map[string]string{}
		v.dimAsked = map[string]bool{}
	}
	if cached, ok := v.dimCache[e.Path]; ok {
		v.dimMu.Unlock()
		return cached
	}
	if v.dimAsked[e.Path] {
		v.dimMu.Unlock()
		return "…"
	}
	v.dimAsked[e.Path] = true
	v.dimMu.Unlock()

	go func(path string) {
		dim := decodeDimensions(path)
		v.dimMu.Lock()
		if len(v.dimCache) >= viewerMetaCap {
			v.dimCache = map[string]string{}
			v.dimAsked = map[string]bool{}
		}
		v.dimCache[path] = dim
		v.dimMu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}(e.Path)
	return "…"
}

func decodeDimensions(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "—"
	}
	defer f.Close()
	// One open for both the EXIF orientation and the image header (G3-02):
	// read the orientation from the handle, rewind, then DecodeConfig.
	orient := imgorient.ReadOrientationFrom(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "—"
	}
	var cfg image.Config
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".tif", ".tiff":
		// x/image/tiff needs an io.ReaderAt to jump to the IFD; handed a plain
		// io.Reader it buffers everything up to it (72 ms / 118 MB on a 48 MB
		// TIFF). Passing the *os.File keeps it on the ReaderAt path (W5-04).
		cfg, err = tiff.DecodeConfig(f)
	default:
		cfg, _, err = image.DecodeConfig(f)
	}
	if err != nil {
		return "—"
	}
	w, h := cfg.Width, cfg.Height
	// DecodeConfig reports the stored (unrotated) dimensions; EXIF orientations
	// 5–8 transpose the image, so the displayed size — and what decodeOriginal
	// now paints — has width and height swapped.
	if orient >= 5 {
		w, h = h, w
	}
	return fmt.Sprintf("%d × %d", w, h)
}

// pendingInfo is shown while the background metadata fetch runs.
var pendingInfo = cachedInfo{Created: "…", Camera: "…", Lens: "…", Aperture: "…", ShutterSpeed: "…", ISO: "…", FocalLength: "…"}

// mediaInfoFor returns the extended metadata for the entry, or empty/placeholder
// values while a background fetch is still running.
func (v *Viewer) mediaInfoFor(e cache.Entry) cachedInfo {
	v.infoMu.Lock()
	if v.infoCache == nil {
		v.infoCache = map[string]cachedInfo{}
		v.infoAsked = map[string]bool{}
	}
	if cached, ok := v.infoCache[e.Path]; ok {
		v.infoMu.Unlock()
		return cached
	}
	if v.infoAsked[e.Path] {
		v.infoMu.Unlock()
		return pendingInfo
	}
	v.infoAsked[e.Path] = true
	v.infoMu.Unlock()

	go func(path string) {
		info := scan.GetMediaInfo(path)
		c := cachedInfo{
			Created:      info.Created.Local().Format("2006-01-02 15:04:05"),
			Camera:       cmp.Or(info.Camera, "—"),
			Lens:         cmp.Or(info.Lens, "—"),
			Aperture:     cmp.Or(info.Aperture, "—"),
			ShutterSpeed: cmp.Or(info.ShutterSpeed, "—"),
			ISO:          cmp.Or(info.ISO, "—"),
			FocalLength:  cmp.Or(info.FocalLength, "—"),
		}
		v.infoMu.Lock()
		if len(v.infoCache) >= viewerMetaCap {
			v.infoCache = map[string]cachedInfo{}
			v.infoAsked = map[string]bool{}
		}
		v.infoCache[path] = c
		v.infoMu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}(e.Path)
	return pendingInfo
}

// opOffset is a thin wrapper around op.Offset so defer Pop() reads cleanly
// next to its push site.
func opOffset(gtx layout.Context, pt image.Point) op.TransformStack {
	return op.Offset(pt).Push(gtx.Ops)
}

func titleCaseGio(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// imageFor returns the image to paint for photo/HEIC/RAW entries: the decoded
// original from the recent cache, or the grid thumbnail while the decode runs.
// It starts the main decode when needed and a re-decode when the window grew
// past what the cached decode can show sharply (V2). Videos and decode
// failures fall back to the thumbnail. UI goroutine only.
func (v *Viewer) imageFor(e cache.Entry, tc *thumbCache, target image.Point) (paint.ImageOp, image.Point, bool) {
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
		v.init()
		if r, ok := v.cacheGet(e); ok {
			if r.needsUpgrade(target) && (v.loadingPath != e.Path || v.loadingTarget != target) {
				v.kickDecode(e, target, viewerResizeDelay)
			}
			return r.op, r.sz, true
		}
		if v.loadingPath != e.Path {
			var delay time.Duration
			if v.rapidNav {
				delay = viewerSettleDelay
			}
			v.kickDecode(e, target, delay)
		}
	}
	// Fall back to the cached thumbnail (always JPEG).
	return tc.Get(e)
}

// needsUpgrade reports whether a window of size target would show r
// noticeably softer than a fresh decode could.
func (r recentImage) needsUpgrade(target image.Point) bool {
	if r.full {
		return false
	}
	if r.native == (image.Point{}) {
		// A display rendition: only a window too big for one gains anything.
		return !displayEnough(target)
	}
	want := fitInside(r.native, target)
	return want.X*100 > r.sz.X*102 || want.Y*100 > r.sz.Y*102
}

// sleepCtx waits d, or until ctx is done, and reports whether the full wait
// elapsed. A non-positive d returns at once without sleeping.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// kickDecode starts the main decode of e for the given image area, after an
// optional settle delay, and cancels any previous main decode. If a prefetch
// of the same path is already running, it waits for that instead of decoding
// twice (V4). The result goes into the recent cache, where the next frame's
// imageFor finds it.
func (v *Viewer) kickDecode(e cache.Entry, target image.Point, delay time.Duration) {
	v.cancelLoading()
	ctx, cancel := context.WithCancel(context.Background())
	v.loadingCtxCancel = cancel
	v.loadingPath = e.Path
	v.loadingTarget = target
	navAt := v.navAt

	// Adopt a prefetch of this path only if it is already decoding. One still
	// queued for a prefetch slot would make the visible image wait behind
	// other prefetches, so cancel it and decode here instead.
	v.recentMu.Lock()
	job := v.inflight[e.Path]
	if job != nil {
		if job.started.Load() {
			job.adopted.Store(true)
		} else {
			job.cancel()
			job = nil
		}
	}
	v.recentMu.Unlock()

	go func() {
		defer cancel()
		if job != nil {
			select {
			case <-job.done:
			case <-ctx.Done():
				return
			}
			if v.cacheHasEntry(e) {
				v.logTiming(e, navAt, "prefetch")
				if v.invalidate != nil {
					v.invalidate()
				}
				return
			}
			// The prefetch failed or was cancelled: decode it ourselves.
		}
		if delay > 0 && !sleepCtx(ctx, delay) {
			return
		}
		select {
		case v.mainSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-v.mainSem }()
		if ctx.Err() != nil {
			return
		}
		d, ok := decodeOriginal(ctx, e, target, v.display)
		if !ok || ctx.Err() != nil {
			return
		}
		v.cachePut(e, d)
		v.logTiming(e, navAt, "decode")
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
}

// logTiming logs time-to-full-resolution when PV_DEBUG_TIMING is set.
func (v *Viewer) logTiming(e cache.Entry, since time.Time, how string) {
	if debugTiming && !since.IsZero() {
		log.Printf("timing: viewer %s full-res in %v (%s)", filepath.Base(e.Path), time.Since(since).Round(time.Millisecond), how)
	}
}

// decodeWork returns the background decode prefetchAt and Warm run: it decodes
// e to fit the current target and parks the result in the recent cache.
func (v *Viewer) decodeWork(e cache.Entry) func(context.Context) {
	return func(ctx context.Context) {
		d, ok := decodeOriginal(ctx, e, v.targetSize(), v.display)
		if ok && ctx.Err() == nil {
			v.cachePut(e, d)
		}
	}
}

// prefetchAt decodes the entry at idx in the background and parks the result
// in the recent cache so a subsequent Next/Prev to it is instant. No-op if idx
// is out of range, the entry is already cached, or it is already in flight.
// Requests beyond viewerPrefetchSlots queue for a slot rather than being
// dropped; ones that go stale while queued are cancelled.
//
// For video entries we don't decode anything — mpv owns playback — but we
// do warm the OS page cache by streaming the first few MB so that when the
// user navigates to the clip, mpv's demuxer doesn't have to wait on a cold
// disk read for the MOOV atom + first GOP.
func (v *Viewer) prefetchAt(idx int) {
	if idx < 0 || idx >= len(v.entries) {
		return
	}
	e := v.entries[idx]
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
	case scan.TypeVideo:
		v.prefetchVideo(e, idx)
		return
	default:
		return
	}
	if v.loadingPath == e.Path || v.cacheHasEntry(e) {
		return
	}
	var delay time.Duration
	if v.rapidNav {
		delay = viewerSettleDelay
	}
	v.startPrefetch(e, idx, delay, v.decodeWork(e))
}

// Warm decodes e ahead of time while the grid has it selected, so Enter opens
// it at full resolution immediately (V7). The decode starts only after the
// selection has rested on e for viewerWarmDelay; moving on cancels it. Call it
// from the UI goroutine whenever the grid selection may have changed.
func (v *Viewer) Warm(e cache.Entry) {
	if e.Path == v.warmPath {
		return
	}
	v.init()
	v.warmPath = e.Path
	if v.warmJob != nil && !v.warmJob.adopted.Load() {
		v.warmJob.cancel()
	}
	v.warmJob = nil
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
	default:
		return
	}
	if v.cacheHasEntry(e) {
		return
	}
	v.warmJob = v.startPrefetch(e, -1, viewerWarmDelay, v.decodeWork(e))
}

// startPrefetch runs work for e as a background job under prefetchSem and
// registers it in inflight. idx ≥ 0 makes the job index-bound: it is skipped
// if the current index has moved more than viewerStaleDistance away by the
// time it gets a slot. Returns nil if a job for e is already in flight.
func (v *Viewer) startPrefetch(e cache.Entry, idx int, delay time.Duration, work func(ctx context.Context)) *decodeJob {
	ctx, cancel := context.WithCancel(context.Background())
	job := &decodeJob{done: make(chan struct{}), cancel: cancel, idx: idx}
	v.recentMu.Lock()
	if v.inflight[e.Path] != nil {
		v.recentMu.Unlock()
		cancel()
		return nil
	}
	v.inflight[e.Path] = job
	v.recentMu.Unlock()

	go func() {
		defer func() {
			v.recentMu.Lock()
			if v.inflight[e.Path] == job {
				delete(v.inflight, e.Path)
			}
			v.recentMu.Unlock()
			cancel()
			close(job.done)
		}()
		if delay > 0 && !sleepCtx(ctx, delay) {
			return
		}
		select {
		case v.prefetchSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-v.prefetchSem }()

		// Stale check: abort if we've navigated far away while queued.
		// curIdx is the atomic mirror of Index; reading it here avoids
		// racing the UI-goroutine writers of Index.
		if idx >= 0 && !job.adopted.Load() {
			if d := int(v.curIdx.Load()) - idx; d < -viewerStaleDistance || d > viewerStaleDistance {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		job.started.Store(true)
		work(ctx)
	}()
	return job
}

// videoPrefetchBytes is the head-of-file window we read into the OS page
// cache for neighbour videos. Picked so that the MOOV atom of a typical
// MP4/MOV (a few MB) and the first GOP land in cache, without churning RAM
// for files we may never open.
const videoPrefetchBytes = 16 << 20

// prefetchVideo warms the OS page cache for an adjacent video by reading
// its first videoPrefetchBytes. Doesn't touch the player — mpv only loads
// one file at a time — but on a cold cache this is the difference between
// mpv blocking on disk seeks and the first frame appearing instantly.
//
// Like the photo prefetch path it shares the prefetchSem bound (so rapid
// paging can't stack N concurrent 16 MB reads into a seek storm) and is
// skipped via the stale check when the user has already paged away from idx.
func (v *Viewer) prefetchVideo(e cache.Entry, idx int) {
	v.init()
	v.startPrefetch(e, idx, 0, func(ctx context.Context) {
		f, err := os.Open(e.Path)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = io.CopyN(io.Discard, fsutil.NewContextReader(ctx, f), videoPrefetchBytes)
	})
}

// fitInside returns sz scaled down (never up) to fit inside box, rounding up so
// the result never falls short of the box on the limiting axis.
func fitInside(sz, box image.Point) image.Point {
	if sz.X <= 0 || sz.Y <= 0 || box.X <= 0 || box.Y <= 0 {
		return sz
	}
	if sz.X <= box.X && sz.Y <= box.Y {
		return sz
	}
	// Compare sz.X/box.X against sz.Y/box.Y without floating point.
	if sz.X*box.Y >= sz.Y*box.X { // width-limited
		return image.Pt(box.X, max((sz.Y*box.X+sz.X-1)/sz.X, 1))
	}
	return image.Pt(max((sz.X*box.Y+sz.Y-1)/sz.Y, 1), box.Y)
}

// displayEnough reports whether a cache.DisplaySize rendition is sharp enough
// for an image area of size target. The photo's aspect isn't known up front,
// so assume a 3:2 landscape: it fits target at min(long side, 1.5 × short side).
// A 10 % margin lets a 2048 px rendition serve 1080p and 1440p windows.
func displayEnough(target image.Point) bool {
	long, short := max(target.X, target.Y), min(target.X, target.Y)
	return min(long, short*3/2) <= cache.DisplaySize*11/10
}

// heicStagePrefix names the staging directory a HEIC conversion writes into.
const heicStagePrefix = "heicview-"

// heicStageDir makes the temporary directory decodeOriginal stages
// heif-convert output in. It is created next to the on-disk caches rather than
// in the global temp dir, which is RAM-backed on systemd distros and never
// swept, so a multi-megabyte conversion doesn't sit in memory. With no display
// store it falls back to the OS temp dir.
func heicStageDir(display *cache.ThumbStore) (string, error) {
	parent := ""
	if display != nil {
		parent = display.CacheDir()
	}
	return os.MkdirTemp(parent, heicStagePrefix+"*")
}

// decodeOriginal decodes an entry's original image to fit an image area of
// size target (V2). JPEGs go through libjpeg-turbo with DCT scaling (V3);
// HEIC and RAW come from the on-disk display rendition when display is set
// and the window is small enough for it (V8). The result is an upright,
// origin-anchored RGBA, so Gio uploads it without another copy. Returns false
// on any error or cancellation.
func decodeOriginal(ctx context.Context, e cache.Entry, target image.Point, display *cache.ThumbStore) (decodedImage, bool) {
	if target.X <= 0 || target.Y <= 0 {
		target = image.Pt(viewerMaxPreviewSide, viewerMaxPreviewSide)
	}
	switch e.Type {
	case scan.TypeHEIC, scan.TypeRAW:
		if display != nil && displayEnough(target) {
			if d, ok := decodeDisplayRendition(ctx, e, target, display); ok {
				return d, true
			}
			if ctx.Err() != nil {
				return decodedImage{}, false
			}
		}
	}

	var img image.Image
	var err error
	orientation := 1
	native := image.Point{}
	switch e.Type {
	case scan.TypeHEIC:
		tmpDir, err2 := heicStageDir(display)
		if err2 != nil {
			return decodedImage{}, false
		}
		defer os.RemoveAll(tmpDir)
		jpgPath, err2 := thumb.HEICToJPEG(ctx, e.Path, tmpDir, false)
		if err2 != nil {
			return decodedImage{}, false
		}
		data, err2 := thumb.ReadFile(ctx, jpgPath)
		if err2 != nil {
			return decodedImage{}, false
		}
		// heif-convert / ffmpeg bake in the HEIF rotation: orientation 1.
		img, native, err = decodeFitted(ctx, data, 1, target)
	case scan.TypeRAW:
		img, orientation, err = thumb.LoadRAWImage(ctx, e.Path, func(w, h, o int) (int, int) {
			return fitMinimum(w, h, o, target)
		})
	case scan.TypePhoto:
		data, err2 := thumb.ReadFile(ctx, e.Path)
		if err2 != nil {
			return decodedImage{}, false
		}
		orientation = imgorient.ReadOrientationFrom(bytes.NewReader(data))
		img, native, err = decodeFitted(ctx, data, orientation, target)
	default:
		return decodedImage{}, false
	}
	if err != nil || img == nil || ctx.Err() != nil {
		return decodedImage{}, false
	}
	return finishDecode(img, orientation, target, native, false), true
}

// decodeFitted decodes data, letting libjpeg-turbo DCT-scale a JPEG down to
// no less than what fits target once oriented. native is the image's full
// upright size when the header could be read, zero otherwise.
func decodeFitted(ctx context.Context, data []byte, orientation int, target image.Point) (image.Image, image.Point, error) {
	img, cfg, err := thumb.DecodeFitFunc(ctx, data, func(w, h int) (int, int) {
		return fitMinimum(w, h, orientation, target)
	})
	native := image.Point{}
	if cfg.Width > 0 {
		native = image.Pt(cfg.Width, cfg.Height)
		if orientation >= 5 {
			native = image.Pt(cfg.Height, cfg.Width)
		}
	}
	return img, native, err
}

// fitMinimum returns the smallest stored-orientation size a w×h JPEG can be
// DCT-scaled to and still cover what fits target once the orientation is
// applied.
func fitMinimum(w, h, orientation int, target image.Point) (int, int) {
	native := image.Pt(w, h)
	if orientation >= 5 {
		native = image.Pt(h, w)
	}
	need := fitInside(native, target)
	if orientation >= 5 {
		return need.Y, need.X
	}
	return need.X, need.Y
}

// decodeDisplayRendition serves HEIC/RAW from the display store, generating
// the rendition on first view (one heif-convert/ffmpeg/exiftool run) so later
// views decode a ~300 KB JPEG instead. Renditions are already upright.
func decodeDisplayRendition(ctx context.Context, e cache.Entry, target image.Point, display *cache.ThumbStore) (decodedImage, bool) {
	path, err := display.Path(e)
	if err != nil || ctx.Err() != nil {
		return decodedImage{}, false
	}
	data, err := thumb.ReadFile(ctx, path)
	if err != nil {
		return decodedImage{}, false
	}
	img, _, err := decodeFitted(ctx, data, 1, target)
	if err != nil || ctx.Err() != nil {
		// An unreadable rendition is regenerated on the next view.
		if ctx.Err() == nil {
			display.Forget(e.ThumbID)
		}
		return decodedImage{}, false
	}
	// native stays unknown: the original may be larger than the rendition.
	return finishDecode(img, 1, target, image.Point{}, true), true
}

// finishDecode turns a decoded image into an upright RGBA op. An image more
// than twice the fitted size per axis (only the image/jpeg and non-JPEG paths
// produce one) is downscaled to the fit; anything up to 2× is left for the
// GPU, whose mipmapped linear filter minifies it well and far faster than a
// CPU resample (300 ms for 12 MP). native is the upright full size if known;
// rendition marks a display-store decode, whose source size stays unknown.
func finishDecode(img image.Image, orientation int, target, native image.Point, rendition bool) decodedImage {
	b := img.Bounds()
	stored := b.Size()
	upright := stored
	if orientation >= 5 {
		upright = image.Pt(stored.Y, stored.X)
	}
	if native == (image.Point{}) && !rendition {
		// No header size (not a JPEG): nothing DCT-scaled it, so the decode
		// is the full size.
		native = upright
	}
	fit := fitInside(upright, target)
	limit := image.Pt(min(2*fit.X, viewerMaxPreviewSide), min(2*fit.Y, viewerMaxPreviewSide))
	var rgba *image.RGBA
	if upright.X > limit.X || upright.Y > limit.Y {
		fit = fitInside(fit, image.Pt(viewerMaxPreviewSide, viewerMaxPreviewSide))
		dw, dh := fit.X, fit.Y
		if orientation >= 5 {
			dw, dh = fit.Y, fit.X
		}
		rgba = image.NewRGBA(image.Rect(0, 0, dw, dh))
		draw.ApproxBiLinear.Scale(rgba, rgba.Rect, img, b, draw.Src, nil)
	} else {
		rgba = imgorient.ToRGBA(img)
	}
	// Rotation is a lossless remap that commutes with scaling, so it runs
	// after the downscale, on the RGBA fast path (V1).
	out := imgorient.Apply(rgba, orientation)
	op := paint.NewImageOp(out)
	op.Filter = paint.FilterLinear
	sz := out.Bounds().Size()
	return decodedImage{op: op, sz: sz, native: native, full: native != (image.Point{}) && sz == native}
}
