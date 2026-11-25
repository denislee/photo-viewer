// Package ui is the Gio frontend for photo-viewer. The non-UI surface
// (cache, scan, thumb, face) is reused unchanged.
package ui

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// countsKey identifies the inputs the sidebar counts depend on. When a
// refresh's key matches the one the published dirCounts was computed from, the
// counts are still valid and refreshFromIndex can skip recomputing them —
// previewing another directory or switching the sort order must not re-run the
// per-subdir COUNT queries (U5-01).
type countsKey struct {
	treeDir string
	root    string
	filter  string
	showRAW bool
}

type Controller struct {
	libraryRoot string
	cacheDir    string
	trashDir    string
	index       *cache.Index
	store       *cache.ThumbStore

	mu          sync.Mutex
	treeDir     string // anchor for the sidebar tree (parent + subdirs)
	currentDir  string // what the grid displays
	entries     []cache.Entry
	subdirs     []string
	mediaFilter string
	showRAW     bool
	sortMode    string // "name" or "duration"
	scanCancel  context.CancelFunc

	// warmUpCancel is owned by the thumbnail warm-up pass and lives outside
	// scanCancel so that picking a new directory (which cancels the current
	// scan) does not also abort a warm-up running in the background.
	warmUpCancel context.CancelFunc
	// warmUpGen increments each time WarmUp() starts a fresh pass so the
	// background goroutine can tell whether warmUpCancel still references
	// its own context when it exits.
	warmUpGen int

	thumbs *thumbCache

	// dirCounts maps absolute path → file count (recursive, current filter
	// applied) for the rows the sidebar renders: library root, parent of
	// treeDir, and each immediate subdir. Recomputed by refreshFromIndex.
	// dirCountsVer is incremented on every clone-swap so callers can
	// cheaply detect changes without comparing the map contents.
	dirCounts    map[string]int
	dirCountsVer uint64
	// countsKey/countsValid record the inputs the published dirCounts was
	// computed from. A refresh whose key matches (and that carries no recount
	// request) can keep the map as-is (U5-01). Guarded by mu.
	countsKey   countsKey
	countsValid bool

	// Coalesces refreshFromIndex calls so back-to-back scan flushes don't
	// pile up dozens of concurrent refresh goroutines (each one re-runs the
	// per-subdir count queries and hits the index mutex). At most one
	// refresh runs at a time; if more are requested while it's running, a
	// single follow-up runs once with the latest target dir.
	refreshMu      sync.Mutex
	refreshRunning bool
	refreshPending bool
	refreshDir     string
	// refreshRecount forces the follow-up refresh to recompute the sidebar
	// counts. OR-merged across requests so a pending recount is never lost
	// when a plain scheduleRefresh coalesces with it (U5-01).
	refreshRecount bool
	// refreshCalls counts refreshFromIndex runs. A test seam: it lets tests
	// assert that a scan flush which changed no rows schedules no refresh.
	refreshCalls atomic.Uint64

	// lastRefreshNanos is the duration of the most recent refreshFromIndex, in
	// nanoseconds. scanInto spaces its refresh requests by at least half of it
	// (floored at 500ms) so a first scan's refresh work never saturates the
	// frame loop (U5-04).
	lastRefreshNanos atomic.Int64

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
	// SnapshotSelected.
	selectionMode bool
	selectedPaths map[string]bool

	invalidate func()

	// processes tracks long-running background work surfaced in the
	// main-screen process bar. Optional — when nil, scan/warm-up just
	// skip the registration calls.
	processes *ProcessRegistry

	displayOnce sync.Once
	display     *cache.ThumbStore

	// deletions tracks the background delete/restore goroutines started by
	// DeletePaths, so the exit path can wait for their index writes to land
	// (I-19 item 6). Without it, os.Exit could run after the trash renames
	// but before RemoveEntries, leaving ghost rows until a Rebuild.
	deletions sync.WaitGroup
}

func NewController(root string, idx *cache.Index, store *cache.ThumbStore, cacheDir string) *Controller {
	trashDir, _ := cache.TrashDir(root, cacheDir)
	c := &Controller{
		libraryRoot:   root,
		cacheDir:      cacheDir,
		trashDir:      trashDir,
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

// NewWebServer constructs the HTTP server that serves the index to a browser.
// Run calls it exactly once at startup and hands it to the web-server modal,
// which reuses it across Start/Stop cycles (U4-14: the old lazily-built,
// mutex-guarded getter had a single caller, which ran at startup anyway).
func (c *Controller) NewWebServer() *webserver.Server {
	// Second thumb store rooted at <cacheDir>/display for the larger
	// browser-viewer renditions (W-03). Best-effort: cacheDir is the same
	// always-writable root the primary thumb store already resolved, so this
	// only fails in pathological cases; a nil store degrades /display to
	// serving originals (fine for JPEG/PNG, no RAW/HEIC rendition).
	return webserver.New(c.index, c.store, c.DisplayStore(), c.libraryRoot, c.trashDir)
}

// DisplayStore returns the shared <cacheDir>/display rendition store, creating
// it on first use. The webserver serves its renditions to browsers (W-03) and
// the viewer decodes HEIC/RAW from it (V8). Returns nil if it can't be created.
func (c *Controller) DisplayStore() *cache.ThumbStore {
	c.displayOnce.Do(func() {
		// A tier of the thumb store, so trash/delete/rename keep it in step
		// and its generation shares the store's CPU / external-tool bound.
		if c.store == nil {
			c.display, _ = cache.NewDisplayStore(c.cacheDir)
		} else if d, err := c.store.Tier(cache.DisplayDir, cache.DisplaySize); err == nil {
			c.display = d
		}
	})
	return c.display
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

// DirCountsWithVersion returns the cached recursive file counts for the rows
// the sidebar is currently rendering (keys are absolute paths; values are
// counts under the active filter/showRAW; the map must not be mutated by the
// caller), alongside a monotonically increasing version that is bumped on
// every clone-swap of the underlying map. Callers that memoize the sidebar row
// list can compare the version instead of the map contents to cheaply detect
// staleness.
func (c *Controller) DirCountsWithVersion() (map[string]int, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirCounts, c.dirCountsVer
}

// cancelScanLocked cancels the in-flight scan, if any. The caller holds c.mu.
func (c *Controller) cancelScanLocked() {
	if c.scanCancel != nil {
		c.scanCancel()
		c.scanCancel = nil
	}
}

// SelectDir switches the active directory: it cancels any in-flight scan,
// refreshes from the index, then starts an incremental scan of path. The grid
// and the sidebar tree both re-anchor on path; the Favorites and Trash views
// skip the scan. Safe to call from any goroutine.
func (c *Controller) SelectDir(path string) {
	if path == FavoritesView || path == TrashView {
		c.mu.Lock()
		c.cancelScanLocked()
		c.currentDir = path
		c.mu.Unlock()
		c.scheduleRefresh(path)
		return
	}
	c.mu.Lock()
	c.cancelScanLocked()
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.treeDir = path
	c.currentDir = path
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

// surfaceError logs err under label and shows it in a warning dialog. Used by
// fire-and-forget goroutines (e.g. the Rebuild button handler) whose failures
// would otherwise vanish. Safe to call from any goroutine; the dialog runs on
// its own goroutine since zenity blocks until dismissed.
func (c *Controller) surfaceError(label string, err error) {
	if err == nil {
		return
	}
	log.Printf("%s: %v", label, err)
	go warnUser("Photo Viewer: "+label+" failed", err.Error())
}

// warnUser shows a warning dialog. A package var so tests can capture the
// message instead of spawning zenity. It blocks until the dialog closes, so
// call it from a goroutine.
var warnUser = func(title, msg string) {
	_, _ = runZenity("--warning", "--no-markup", "--title="+title, "--text="+msg)
}

// PreviewDir refreshes the grid with path's contents without changing the
// sidebar's tree anchor. Used while the user is exploring directories with
// the keyboard — j/k previews each dir; Enter/click commits via SelectDir.
func (c *Controller) PreviewDir(path string) {
	c.mu.Lock()
	c.cancelScanLocked()
	c.currentDir = path
	c.mu.Unlock()

	c.scheduleRefresh(path)
}

// PreviewYear loads the union of entries under the given YYYY year into the
// grid without changing the sidebar tree anchor. Used while keyboard-previewing
// a YYYY year header in the sidebar — the grid then shows every photo in that
// year regardless of which date folder it lives in.
func (c *Controller) PreviewYear(year string) {
	key := YearViewPrefix + year
	c.mu.Lock()
	c.cancelScanLocked()
	c.currentDir = key
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
// a delete: the row is relocated (favorite flag, probed duration, cached hashes
// and media_date preserved) instead of dropped. Deliberately does NOT refresh the grid — the
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
	// Relocate the row (and its face rows) in one transaction. A rename keeps
	// size and mtime, so the favorite flag, probed duration, cached hashes and
	// media_date all stay valid and are carried over as-is (U4-03).
	err := c.index.MovePath(oldPath, newPath)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// No row for oldPath (never indexed, or already dropped): index newPath
	// from a fresh stat instead.
	if info, err := os.Stat(newPath); err == nil {
		c.index.ReconcileBatchChanged([]scan.Result{{
			Path:    newPath,
			Type:    scan.DetectType(newPath),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}})
		// Relocate any face rows still keyed to oldPath. The thumbnail was
		// renamed (not regenerated), so the embeddings and thumb_mtime stay
		// valid.
		_ = c.index.MoveFaces(oldPath, newPath)
	}
	// RemoveEntry clears any face rows still under oldPath — the safety net
	// when the newPath stat above failed and MoveFaces was skipped.
	return c.index.RemoveEntry(oldPath)
}

// IndexImported incrementally scans dir, an import's outbox, so the files an
// import just filed show up in the grid and sidebar without a manual rescan
// (I-03 step 4). The scan registers in the process bar like any other and
// refreshes the active directory as it commits. A dir outside the library
// root is ignored: the index only covers the library.
func (c *Controller) IndexImported(dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(c.libraryRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	go c.scanInto(context.Background(), abs)
}

// RefreshActive re-queries the index for the directory currently shown in the
// grid and repaints. Exposed so flows that mutate the index outside the normal
// scan path (the organize move pass via ApplyMove) can surface their changes
// with a single coalesced refresh once their batch completes. A recount is
// requested because those flows changed the data the sidebar counts describe.
func (c *Controller) RefreshActive() {
	c.scheduleRefreshRecount(c.activeDir())
}

// scheduleRefresh asks for a refreshFromIndex against dir, coalescing with any
// in-flight refresh so callers can fire it freely without spawning a goroutine
// per flush. The sidebar counts are left alone when their inputs are unchanged.
func (c *Controller) scheduleRefresh(dir string) {
	c.scheduleRefreshOpts(dir, false)
}

// scheduleRefreshRecount is scheduleRefresh for callers that changed index
// data: it forces the next refreshFromIndex to recompute the sidebar counts.
// The request is OR-merged, so coalescing with a plain scheduleRefresh (in
// either order) never loses it.
func (c *Controller) scheduleRefreshRecount(dir string) {
	c.scheduleRefreshOpts(dir, true)
}

// scheduleRefreshOpts implements both scheduleRefresh flavors.
func (c *Controller) scheduleRefreshOpts(dir string, recount bool) {
	c.refreshMu.Lock()
	c.refreshDir = dir
	c.refreshRecount = c.refreshRecount || recount
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
			recount := c.refreshRecount
			c.refreshPending = false
			c.refreshRecount = false
			c.refreshMu.Unlock()
			c.refreshFromIndex(d, recount)
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

func (c *Controller) refreshFromIndex(dir string, recount bool) {
	start := time.Now()
	c.refreshCalls.Add(1)
	// Snapshot the controller state this refresh reads once, up front, so the
	// year-preview branch below can scope its query to a treeDir consistent with
	// the filter snapshot.
	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	sortMode := c.sortMode
	treeDir := c.treeDir
	root := c.libraryRoot
	prevSubdirs := c.subdirs
	countsValid := c.countsValid
	prevCountsKey := c.countsKey
	c.mu.Unlock()

	var entries []cache.Entry
	// needGoFilter marks the branch that still hands back unfiltered rows: the
	// trash listing (read off the filesystem). Every other branch pushes the
	// media filter into SQL.
	needGoFilter := false
	// fromDirView marks the plain dir listing, whose rows are exactly what the
	// sidebar counts describe — so the counts can be derived from the loaded
	// slice instead of re-queried (U5-02).
	fromDirView := false
	switch {
	case dir == FavoritesView:
		entries = c.index.ListPage(cache.View{Kind: "favorites", Filter: filter, ShowRAW: showRAW}, 0, 0)
	case dir == TrashView:
		entries = cache.ListTrash(c.trashDir)
		needGoFilter = true
	case strings.HasPrefix(dir, YearViewPrefix):
		// U-08: resolve the year-preview union in a single covering-index query
		// scoped to treeDir instead of one prefix-LIKE ListDir per date dir. The
		// date dirs are the YYYY-MM-DD children of treeDir bucketed under this
		// year header, and each entry's year column is derived from that
		// parent-dir name, so ListByYear(year, …, treeDir) returns exactly their
		// union with the media filter pushed into SQL.
		y, _ := strconv.Atoi(strings.TrimPrefix(dir, YearViewPrefix))
		entries = c.index.ListByYear(y, filter, showRAW, treeDir)
	default:
		entries = c.index.ListPage(cache.View{Kind: "dir", Dir: dir, Filter: filter, ShowRAW: showRAW}, 0, 0)
		fromDirView = true
	}

	// Filter in place: every branch above returns a freshly allocated slice,
	// so reusing its backing array avoids a second full-size copy per refresh
	// (~9 MB at 100k entries). The SQL-backed branches already applied the
	// filter, so only the trash rows need the Go pass.
	filtered := entries
	if needGoFilter && (filter != "All" || !showRAW) {
		filtered = slices.DeleteFunc(entries, func(e cache.Entry) bool {
			if !showRAW && e.Type == scan.TypeRAW {
				return true
			}
			switch filter {
			case "Photos":
				return e.Type == scan.TypeVideo
			case "Videos":
				return e.Type != scan.TypeVideo
			}
			return false
		})
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

	// Publish the grid first: the entries are what the user is looking at, and
	// the sidebar counts below can cost several queries on a deep tree. Waking
	// the frame loop here lets the grid repaint while the counts are computed
	// (U5-01).
	c.mu.Lock()
	if c.currentDir == dir {
		c.entries = filtered
	}
	if wantSubdirs && c.treeDir == dir {
		c.subdirs = subs
	}
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}

	// The counts only depend on the tree anchor, the filter and showRAW — plus,
	// for the tree dir itself, the set of subdirs it lists. A refresh that
	// changed none of those (a preview of another dir, a sort-order switch, a
	// rescan that landed no rows) can keep the published map as-is: the
	// in-memory patches in patchLocalDeletion / patchFavorite / bumpTrashCount
	// have kept it accurate (U5-01).
	key := countsKey{treeDir: treeDir, root: root, filter: filter, showRAW: showRAW}
	needCounts := recount || !countsValid || prevCountsKey != key ||
		(wantSubdirs && !slices.Equal(subs, prevSubdirs))
	if !needCounts {
		c.lastRefreshNanos.Store(int64(time.Since(start)))
		return
	}

	// Counts are computed for the rows the sidebar will render: root,
	// treeDir's parent (if the tree isn't anchored at root), and each subdir.
	//
	// The dir-view entries already are exactly the filtered rows under treeDir,
	// in path order, so the tree's total and its per-child counts come straight
	// from the loaded slice instead of two more queries on the biggest range in
	// the index (U5-02). The per-subdir counts otherwise come from one grouped
	// scan over treeDir's path range (CountChildDirsFiltered) instead of N
	// round-trips, so a deep directory with dozens of children is one query
	// instead of N+3.
	useLoadedCounts := fromDirView && dir == treeDir
	counts := make(map[string]int, len(subs)+3)
	if useLoadedCounts {
		counts[dir] = len(entries)
	}
	// The library root's total is the tree's total when the tree is anchored
	// there; otherwise (including a previewed subdir whose parent chain reaches
	// the root) it still comes from the index.
	if !useLoadedCounts || root != dir {
		counts[root] = c.index.CountDirFiltered(root, filter, showRAW)
	} else {
		counts[root] = counts[dir]
	}
	if treeDir != root {
		parent := filepath.Dir(treeDir)
		counts[parent] = c.index.CountDirFiltered(parent, filter, showRAW)
	}
	if len(subs) > 0 {
		var childCounts map[string]int
		if useLoadedCounts {
			childCounts = childCountsFromEntries(dir, entries)
		} else {
			childCounts = c.index.CountChildDirsFiltered(treeDir, filter, showRAW)
		}
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
	if c.treeDir == treeDir {
		c.dirCounts = counts
		c.dirCountsVer++
		c.countsKey = key
		c.countsValid = true
	}
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
	c.lastRefreshNanos.Store(int64(time.Since(start)))
}

// childCountsFromEntries reproduces cache.Index.CountChildDirsFiltered on an
// already media-filtered, path-sorted entry slice: it counts, per immediate
// child directory of dir, the rows living anywhere beneath it. Rows directly
// in dir (no further separator) and the dir's own exact-path row — which
// ListPage folds in when a *file* path is passed as dir — contribute nothing,
// matching the SQL's instr(…, '/') > 0 guard.
func childCountsFromEntries(dir string, entries []cache.Entry) map[string]int {
	prefix := dir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	out := make(map[string]int, 16)
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rest := e.Path[len(prefix):]
		j := strings.IndexByte(rest, filepath.Separator)
		if j <= 0 {
			continue
		}
		out[filepath.Join(dir, rest[:j])]++
	}
	return out
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
			// Non-video / unknown duration (0 or the -1 probe-failure
			// sentinel) → end of list.
			if di <= 0 {
				return false
			}
			if dj <= 0 {
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
