// Package ui is the Gio frontend for photo-viewer. The non-UI surface
// (cache, scan, thumb, face) is reused unchanged.
package ui

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/webserver"
)

// FavoritesView is the sentinel "path" used by the sidebar's synthetic
// Favorites row. The grid renders the favorite entries (regardless of
// directory) when the controller's currentDir equals this value.
const FavoritesView = "\x00favorites"

// TrashView is the sentinel currentDir used to render the soft-deleted items
// living in the trash directory. The sidebar shows a synthetic "Trash" row
// that selects it.
const TrashView = "\x00trash"

// Sort modes accepted in Config.SortMode and Controller.sortMode.
const (
	SortByName     = "name"
	SortByDuration = "duration"
)

// normalizeSort returns a known sort mode, defaulting to SortByName for any
// unrecognized or empty value.
func normalizeSort(s string) string {
	if s == SortByDuration {
		return SortByDuration
	}
	return SortByName
}

// YearViewPrefix marks synthetic currentDir values produced by PreviewYear.
// The suffix is the YYYY year string. The grid shows the union of entries
// under each date subdir bucketed beneath that year header.
const YearViewPrefix = "\x00year:"

type Controller struct {
	libraryRoot string
	cacheDir    string
	trashDir    string
	// indexPath is the real on-disk SQLite path, resolved once at construction
	// via cache.IndexPath (which may fall back to cacheDir on a read-only
	// library root). Stored so IndexStatus reports the true path without
	// re-deriving or re-statting the filesystem on every modal poll.
	indexPath string
	index     *cache.Index
	store     *cache.ThumbStore

	mu          sync.Mutex
	treeDir     string // anchor for the sidebar tree (parent + subdirs)
	currentDir  string // what the grid displays
	entries     []cache.Entry
	subdirs     []string
	mediaFilter string
	showRAW     bool
	sortMode    string // "name" or "duration"
	scanCancel  context.CancelFunc
	scanning    int // active scanInto goroutines

	// warmUpCancel is owned by the thumbnail warm-up pass and lives outside
	// scanCancel so that picking a new directory (which cancels the current
	// scan) does not also abort a warm-up running in the background.
	warmUpCancel  context.CancelFunc
	warmUpRunning bool
	// warmUpGen increments each time WarmUp() starts a fresh pass so the
	// background goroutine can tell whether warmUpCancel still references
	// its own context when it exits.
	warmUpGen int
	// Warm-up progress, exposed via IndexStatus.WarmUp. Kept apart from the
	// scan* fields below: a warm-up usually runs alongside directory scans,
	// and sharing the fields made each overwrite the other's status.
	warmUpStartedAt time.Time
	warmUpEndedAt   time.Time
	warmUpDone      int
	warmUpTotal     int

	// Indexing status, exposed via IndexStatus for the info modal.
	scanTarget    string
	scanStartedAt time.Time
	scanEndedAt   time.Time
	scanBatched   int // entries reconciled during the current/last scan
	scanLastErr   string

	// IndexStatus is polled every frame the index-info modal is open, and its
	// TotalRows is a LIKE-prefix COUNT over the whole index — precisely while a
	// scan is hammering the same DB with ReconcileBatch writes. Cache the count
	// behind a short TTL so the modal costs at most one COUNT per idxStatusTTL
	// instead of one per frame. Slight staleness is fine for a status readout.
	// Guarded by its own mutex (not c.mu) so a poll never blocks the scan.
	idxStatusMu     sync.Mutex
	idxStatusRows   int
	idxStatusRoot   string // library root the cached count was computed for
	idxStatusExpiry time.Time

	// yearPreviewDirs holds the date subdirs whose union is shown when
	// currentDir is a YearViewPrefix sentinel. refreshFromIndex reads this
	// to recompute entries when scan flushes trigger a refresh.
	yearPreviewDirs []string

	thumbs *thumbCache

	// dirCounts maps absolute path → file count (recursive, current filter
	// applied) for the rows the sidebar renders: library root, parent of
	// treeDir, and each immediate subdir. Recomputed by refreshFromIndex.
	// dirCountsVer is incremented on every clone-swap so callers can
	// cheaply detect changes without comparing the map contents.
	dirCounts    map[string]int
	dirCountsVer uint64

	// Coalesces refreshFromIndex calls so back-to-back scan flushes don't
	// pile up dozens of concurrent refresh goroutines (each one re-runs the
	// per-subdir count queries and hits the index mutex). At most one
	// refresh runs at a time; if more are requested while it's running, a
	// single follow-up runs once with the latest target dir.
	refreshMu      sync.Mutex
	refreshRunning bool
	refreshPending bool
	refreshDir     string

	// trashCount mirrors what cache.TrashStats would report. Seeded lazily
	// (on first read) so startup doesn't pay for a directory walk; bumped
	// in lockstep with MoveToTrash / RestoreFromTrash / EmptyTrash so
	// refreshFromIndex doesn't have to re-stat the trash directory on
	// every delete. Guarded by mu.
	trashCount      int
	trashCountValid bool

	// Favorite writes (G-14). ToggleFavorite flips the UI at once and queues
	// the DB write; a single drain goroutine applies the queue in order, so
	// rapid presses on one file can't land out of order. A failed write is
	// undone and recorded in favReverts for the UI goroutine, since the viewer
	// keeps its own copy of the entries. Guarded by favMu.
	favMu      sync.Mutex
	favQueue   []favWrite
	favRunning bool
	favReverts []FavoriteRevert

	// Selection state. Guarded by mu; read through SelectionMode /
	// IsSelected / SnapshotSelected.
	selectionMode bool
	selectedPaths map[string]bool

	invalidate func()

	// processes tracks long-running background work surfaced in the
	// main-screen process bar. Optional — when nil, scan/warm-up just
	// skip the registration calls.
	processes *ProcessRegistry

	// webserver is the optional HTTP server that serves the index to a
	// browser. Lazily constructed when the user first opens the webserver
	// modal so headless / unused sessions don't allocate it.
	webserver *webserver.Server
}

func NewController(root string, idx *cache.Index, store *cache.ThumbStore, cacheDir string) *Controller {
	trashDir, _ := cache.TrashDir(root, cacheDir)
	c := &Controller{
		libraryRoot:   root,
		cacheDir:      cacheDir,
		trashDir:      trashDir,
		indexPath:     cache.IndexPath(root, cacheDir),
		index:         idx,
		store:         store,
		treeDir:       root,
		currentDir:    root,
		mediaFilter:   "All",
		showRAW:       true,
		sortMode:      normalizeSort(GetConfig().SortMode),
		selectedPaths: make(map[string]bool),
	}
	c.thumbs = newThumbCache(store, func() {
		if c.invalidate != nil {
			c.invalidate()
		}
	})
	return c
}

// SetInvalidate registers a callback used to wake the Gio frame loop after
// background work mutates state.
func (c *Controller) SetInvalidate(f func()) { c.invalidate = f }

// SetProcessRegistry wires the long-running-task registry so that
// background scans and the thumbnail warm-up appear in the main-screen
// process bar with pause / resume / cancel controls.
func (c *Controller) SetProcessRegistry(r *ProcessRegistry) { c.processes = r }

// WebServer lazily constructs and returns the controller's HTTP server.
// The instance is reused across Start/Stop cycles so its state survives
// the modal closing.
func (c *Controller) WebServer() *webserver.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.webserver == nil {
		// Second thumb store rooted at <cacheDir>/display for the larger
		// browser-viewer renditions (W-03). Best-effort: cacheDir is the same
		// always-writable root the primary thumb store already resolved, so this
		// only fails in pathological cases; a nil store degrades /display to
		// serving originals (fine for JPEG/PNG, no RAW/HEIC rendition).
		display, _ := cache.NewDisplayStore(c.cacheDir)
		c.webserver = webserver.New(c.index, c.store, display, c.libraryRoot)
	}
	return c.webserver
}

// Snapshot returns a stable view of the current directory's entries and the
// sidebar's tree anchor + its child directories. The slices must not be
// mutated by the caller.
//
// treeDir is the path the sidebar tree is rendered around (used to compute
// the parent and the listed subdirs). currentDir is the directory whose
// entries are shown in the grid. The two diverge while the sidebar is
// keyboard-previewing a directory without committing to descending into it.
func (c *Controller) Snapshot() (treeDir, currentDir string, entries []cache.Entry, subdirs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.treeDir, c.currentDir, c.entries, c.subdirs
}

func (c *Controller) LibraryRoot() string      { return c.libraryRoot }
func (c *Controller) Thumbs() *thumbCache      { return c.thumbs }
func (c *Controller) Index() *cache.Index      { return c.index }
func (c *Controller) Store() *cache.ThumbStore { return c.store }

// DirCounts returns the cached recursive file counts for the rows the
// sidebar is currently rendering. Keys are absolute paths; values are
// counts under the active filter/showRAW. Returned map must not be
// mutated by the caller.
func (c *Controller) DirCounts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirCounts
}

// DirCountsWithVersion returns DirCounts alongside a monotonically increasing
// version that is bumped on every clone-swap of the underlying map. Callers
// that memoize the sidebar row list can compare the version instead of the map
// contents to cheaply detect staleness.
func (c *Controller) DirCountsWithVersion() (map[string]int, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirCounts, c.dirCountsVer
}

// SelectDir is the equivalent of the Fyne Controller.SelectDir — it cancels
// any in-flight scan, refreshes from the index, then kicks off an incremental
// scan. Both the grid and the sidebar tree re-anchor on path. Safe to call
// from any goroutine.
func (c *Controller) SelectDir(path string) {
	if path == FavoritesView || path == TrashView {
		c.mu.Lock()
		if c.scanCancel != nil {
			c.scanCancel()
			c.scanCancel = nil
		}
		c.currentDir = path
		c.mu.Unlock()
		c.scheduleRefresh(path)
		return
	}
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.treeDir = path
	c.currentDir = path
	c.yearPreviewDirs = nil
	c.mu.Unlock()

	c.scheduleRefresh(path)
	go c.scanInto(ctx, path)
}

// SetFilter switches the active media filter ("All", "Photos", "Videos") and
// re-runs the index query for the current grid directory.
func (c *Controller) SetFilter(filter string) {
	c.mu.Lock()
	c.mediaFilter = filter
	dir := c.currentDir
	c.mu.Unlock()
	c.scheduleRefresh(dir)
}

// SetShowRAW toggles whether RAW entries are included in the grid.
func (c *Controller) SetShowRAW(v bool) {
	c.mu.Lock()
	c.showRAW = v
	dir := c.currentDir
	c.mu.Unlock()
	c.scheduleRefresh(dir)
}

// Filter / ShowRAW expose the current settings so the toolbar can render
// the active state without a separate state mirror.
func (c *Controller) Filter() string { c.mu.Lock(); defer c.mu.Unlock(); return c.mediaFilter }
func (c *Controller) ShowRAW() bool  { c.mu.Lock(); defer c.mu.Unlock(); return c.showRAW }
func (c *Controller) Sort() string   { c.mu.Lock(); defer c.mu.Unlock(); return c.sortMode }

// SetSort switches the grid sort mode and refreshes the active view so the
// new order is visible immediately.
func (c *Controller) SetSort(mode string) {
	mode = normalizeSort(mode)
	c.mu.Lock()
	if c.sortMode == mode {
		c.mu.Unlock()
		return
	}
	c.sortMode = mode
	dir := c.currentDir
	c.mu.Unlock()
	c.scheduleRefresh(dir)
}

// surfaceError records err as the controller's last error — the value the
// index-info modal shows via IndexStatus.LastError — after logging it under
// label, then requests a redraw so the message appears without waiting for the
// next scan flush. Used by fire-and-forget goroutines (e.g. the Rebuild button
// handler) whose failures would otherwise vanish. Safe to call from any
// goroutine; scanLastErr is guarded by c.mu like the other status fields.
func (c *Controller) surfaceError(label string, err error) {
	if err == nil {
		return
	}
	log.Printf("%s: %v", label, err)
	c.mu.Lock()
	c.scanLastErr = err.Error()
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
}

// PreviewDir refreshes the grid with path's contents without changing the
// sidebar's tree anchor. Used while the user is exploring directories with
// the keyboard — j/k previews each dir; Enter/click commits via SelectDir.
func (c *Controller) PreviewDir(path string) {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
		c.scanCancel = nil
	}
	c.currentDir = path
	c.yearPreviewDirs = nil
	c.mu.Unlock()

	c.scheduleRefresh(path)
}

// PreviewYear loads the union of entries under each given date subdir into the
// grid without changing the sidebar tree anchor. Used while keyboard-previewing
// a YYYY year header in the sidebar — the grid then shows every photo in that
// year regardless of which date folder it lives in.
func (c *Controller) PreviewYear(year string, dirs []string) {
	if len(dirs) == 0 {
		return
	}
	key := YearViewPrefix + year
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
		c.scanCancel = nil
	}
	c.currentDir = key
	c.yearPreviewDirs = append(c.yearPreviewDirs[:0], dirs...)
	c.mu.Unlock()

	c.scheduleRefresh(key)
}

// ApplyMove keeps the index and thumbnail store consistent after the organize
// pass has renamed a media file from oldPath to newPath on disk. Without it the
// grid would show a broken row for the vanished old path and — because the
// thumb id is derived from the absolute path — regenerate the thumbnail from
// scratch at the new path (an expensive video decode) on the next scan.
//
// Mirrors performDeletion's index+thumb bookkeeping, but for a move rather than
// a delete: the row is relocated (favorite flag + already-probed video duration
// preserved) instead of dropped. Deliberately does NOT refresh the grid — the
// caller fires a single RefreshActive once the whole batch of moves is done so
// a long organize pass doesn't trigger one grid refresh per file.
func (c *Controller) ApplyMove(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" || oldPath == newPath {
		return nil
	}
	// Carry the existing thumbnail over so it isn't regenerated. Missing source
	// thumbs are treated as success by Rename, so this is safe even if the
	// thumb was never generated.
	if c.store != nil {
		_ = c.store.Rename(cache.ThumbIDFor(oldPath), cache.ThumbIDFor(newPath))
	}
	// Read the old row before we drop it so we can carry over the favorite flag
	// and the already-probed duration: newPath is a brand-new row, so a fresh
	// reconcile would default favorite to false and (with duration 0) force a
	// re-probe of the video on the next scan.
	old, hadOld := c.index.GetEntry(oldPath)
	if info, err := os.Stat(newPath); err == nil {
		var durationMs int64
		if hadOld {
			durationMs = old.DurationMs
		}
		c.index.ReconcileBatch([]scan.Result{{
			Path:       newPath,
			Type:       scan.DetectType(newPath),
			Size:       info.Size(),
			ModTime:    info.ModTime(),
			DurationMs: durationMs,
		}})
		if hadOld && old.Favorite {
			_ = c.index.SetFavorite(newPath, true)
		}
		// Relocate the file's face rows to the new path rather than let them
		// orphan. The thumbnail was renamed (not regenerated), so the embeddings
		// and thumb_mtime stay valid — this also spares the pipeline from
		// re-detecting the moved file from scratch. After this, no faces remain
		// under oldPath, so RemoveEntry's own face cleanup below is a no-op.
		_ = c.index.MoveFaces(oldPath, newPath)
	}
	// Drop the stale row last so an in-flight scan can't re-observe the old path
	// after we've already relocated its row (the file itself is already gone
	// from the old path on disk, so the scan won't re-add it). RemoveEntry also
	// clears any face rows still under oldPath — the safety net when the newPath
	// stat above failed and MoveFaces was skipped.
	return c.index.RemoveEntry(oldPath)
}

// RefreshActive re-queries the index for the directory currently shown in the
// grid and repaints. Exposed so flows that mutate the index outside the normal
// scan path (the organize move pass via ApplyMove) can surface their changes
// with a single coalesced refresh once their batch completes.
func (c *Controller) RefreshActive() {
	c.scheduleRefresh(c.activeDir())
}

// scheduleRefresh asks for a refreshFromIndex against dir, coalescing with any
// in-flight refresh so callers can fire it freely without spawning a goroutine
// per flush.
func (c *Controller) scheduleRefresh(dir string) {
	c.refreshMu.Lock()
	c.refreshDir = dir
	if c.refreshRunning {
		c.refreshPending = true
		c.refreshMu.Unlock()
		return
	}
	c.refreshRunning = true
	c.refreshMu.Unlock()
	go func() {
		for {
			c.refreshMu.Lock()
			d := c.refreshDir
			c.refreshPending = false
			c.refreshMu.Unlock()
			c.refreshFromIndex(d)
			c.refreshMu.Lock()
			if !c.refreshPending {
				c.refreshRunning = false
				c.refreshMu.Unlock()
				return
			}
			c.refreshMu.Unlock()
		}
	}()
}

func (c *Controller) refreshFromIndex(dir string) {
	// Snapshot the controller state this refresh reads once, up front, so the
	// year-preview branch below can scope its query to a treeDir consistent with
	// the yearPreviewDirs read in the same lock.
	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	sortMode := c.sortMode
	treeDir := c.treeDir
	root := c.libraryRoot
	prevSubdirs := c.subdirs
	yearDirs := append([]string(nil), c.yearPreviewDirs...)
	c.mu.Unlock()

	var entries []cache.Entry
	switch {
	case dir == FavoritesView:
		entries = c.index.ListFavorites()
	case dir == TrashView:
		entries = cache.ListTrash(c.trashDir)
	case strings.HasPrefix(dir, YearViewPrefix):
		// U-08: resolve the year-preview union in a single covering-index query
		// scoped to treeDir instead of one prefix-LIKE ListDir per date dir. The
		// yearPreviewDirs are the YYYY-MM-DD children of treeDir bucketed under
		// this year header, and each entry's year column is derived from that
		// parent-dir name, so ListByYear(year, …, treeDir) returns exactly their
		// union with the media filter pushed into SQL. A parse failure (the
		// sentinel is always built from a YYYY string, so this shouldn't happen)
		// falls back to the per-dir loop.
		if y, err := strconv.Atoi(strings.TrimPrefix(dir, YearViewPrefix)); err == nil {
			entries = c.index.ListByYear(y, filter, showRAW, treeDir)
		} else {
			for _, d := range yearDirs {
				entries = append(entries, c.index.ListDir(d)...)
			}
		}
	default:
		entries = c.index.ListDir(dir)
	}

	var filtered []cache.Entry
	for _, e := range entries {
		if !showRAW && e.Type == scan.TypeRAW {
			continue
		}
		switch filter {
		case "Photos":
			if e.Type == scan.TypeVideo {
				continue
			}
		case "Videos":
			if e.Type != scan.TypeVideo {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	sortEntries(filtered, sortMode)

	// Avoid running listSubdirs (filesystem I/O) under the mutex.
	wantSubdirs := treeDir == dir && dir != FavoritesView && dir != TrashView
	var subs []string
	if wantSubdirs {
		subs = listSubdirs(dir)
	} else {
		subs = prevSubdirs
	}

	// Counts are computed for the rows the sidebar will render: root,
	// treeDir's parent (if the tree isn't anchored at root), and each subdir.
	// Recomputed on every refresh so they stay in sync with filter/showRAW
	// changes and with newly-batched scan results.
	//
	// The per-subdir counts come from one grouped scan over treeDir's path
	// range (CountChildDirsFiltered) instead of N round-trips, so a deep
	// directory with dozens of children is one query instead of N+3.
	counts := make(map[string]int, len(subs)+3)
	counts[root] = c.index.CountDirFiltered(root, filter, showRAW)
	if treeDir != root {
		parent := filepath.Dir(treeDir)
		counts[parent] = c.index.CountDirFiltered(parent, filter, showRAW)
	}
	if len(subs) > 0 {
		childCounts := c.index.CountChildDirsFiltered(treeDir, filter, showRAW)
		for _, s := range subs {
			counts[s] = childCounts[s]
		}
	}
	counts[FavoritesView] = c.index.CountFavorites(filter, showRAW)
	if c.trashDir != "" {
		// Cached mirror — no per-refresh os.ReadDir + stat over the trash
		// directory. The delete / restore / empty paths keep this in sync.
		counts[TrashView] = c.cachedTrashCount()
	}

	c.mu.Lock()
	if c.currentDir == dir {
		c.entries = filtered
	}
	if wantSubdirs && c.treeDir == dir {
		c.subdirs = subs
	}
	if c.treeDir == treeDir {
		c.dirCounts = counts
		c.dirCountsVer++
	}
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
}

func (c *Controller) activeDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentDir
}

// sortEntries orders the slice in place according to mode. SortByDuration
// puts the longest videos first; entries without a duration (photos, RAW,
// HEIC, or videos whose duration couldn't be probed) fall to the end ordered
// by path so the listing stays stable.
//
// SortByName is a no-op: ListDir returns rows ordered by path via the PK
// index, and the filter pass that precedes this call is a linear scan that
// preserves that order.
func sortEntries(entries []cache.Entry, mode string) {
	if normalizeSort(mode) != SortByDuration {
		return
	}
	// Path is a unique tie-breaker (it's the index primary key), so the
	// comparator is a total order and stability is irrelevant — sort.Slice
	// is faster and allocation-free.
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].DurationMs, entries[j].DurationMs
		if di != dj {
			// Non-video / unknown duration → end of list.
			if di == 0 {
				return false
			}
			if dj == 0 {
				return true
			}
			return di > dj
		}
		return entries[i].Path < entries[j].Path
	})
}

// listSubdirs returns the immediate child directories of dir, sorted by name,
// hidden directories filtered out (matching the scan package's behavior).
func listSubdirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	// os.ReadDir already returns entries sorted by filename; joining each with
	// the common `dir` prefix preserves that order, so no explicit sort needed.
	return out
}
