// Package ui is the Gio frontend for photo-viewer. The non-UI surface
// (cache, scan, thumb, face) is reused unchanged.
package ui

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
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

	// Indexing status, exposed via IndexStatus for the info modal.
	scanTarget    string
	scanStartedAt time.Time
	scanEndedAt   time.Time
	scanBatched   int // entries reconciled during the current/last scan
	scanLastErr   string

	// yearPreviewDirs holds the date subdirs whose union is shown when
	// currentDir is a YearViewPrefix sentinel. refreshFromIndex reads this
	// to recompute entries when scan flushes trigger a refresh.
	yearPreviewDirs []string

	thumbs *thumbCache

	// dirCounts maps absolute path → file count (recursive, current filter
	// applied) for the rows the sidebar renders: library root, parent of
	// treeDir, and each immediate subdir. Recomputed by refreshFromIndex.
	dirCounts map[string]int

	// Coalesces refreshFromIndex calls so back-to-back scan flushes don't
	// pile up dozens of concurrent refresh goroutines (each one re-runs the
	// per-subdir count queries and hits the index mutex). At most one
	// refresh runs at a time; if more are requested while it's running, a
	// single follow-up runs once with the latest target dir.
	refreshMu      sync.Mutex
	refreshRunning bool
	refreshPending bool
	refreshDir     string

	// Selection state
	SelectionMode bool
	SelectedPaths map[string]bool

	invalidate func()

	// processes tracks long-running background work surfaced in the
	// main-screen process bar. Optional — when nil, scan/warm-up just
	// skip the registration calls.
	processes *ProcessRegistry
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
		SelectedPaths: make(map[string]bool),
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

// Scanning reports whether at least one scanInto goroutine is currently
// walking the filesystem and reconciling entries into the index.
func (c *Controller) Scanning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanning > 0
}

// CancelScan stops any in-flight scan or warm-up. The toolbar's Cancel
// button is the only place that should cancel a warm-up — directory changes
// only abort the scan context.
func (c *Controller) CancelScan() {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
		c.scanCancel = nil
	}
	if c.warmUpCancel != nil {
		c.warmUpCancel()
		c.warmUpCancel = nil
	}
	c.mu.Unlock()
}

// IndexStatus is a snapshot of the indexing pipeline state for the info modal.
type IndexStatus struct {
	LibraryRoot string
	CacheDir    string
	DBPath      string
	Active      bool
	Target      string
	StartedAt   time.Time
	EndedAt     time.Time
	Batched     int
	TotalRows   int
	LastError   string
}

// IndexStatus returns a snapshot of the current/last indexing run plus
// pointers to the on-disk state. Safe to call from any goroutine.
func (c *Controller) IndexStatus() IndexStatus {
	c.mu.Lock()
	st := IndexStatus{
		LibraryRoot: c.libraryRoot,
		CacheDir:    c.cacheDir,
		DBPath:      filepath.Join(c.libraryRoot, ".photo-viewer.db"),
		Active:      c.scanning > 0,
		Target:      c.scanTarget,
		StartedAt:   c.scanStartedAt,
		EndedAt:     c.scanEndedAt,
		Batched:     c.scanBatched,
		LastError:   c.scanLastErr,
	}
	idx := c.index
	c.mu.Unlock()
	if idx != nil {
		st.TotalRows = idx.CountDir(st.LibraryRoot)
	}
	return st
}

// Rebuild wipes the on-disk cache (sqlite db + thumbs), reopens the index,
// and kicks off a full rescan of the library root. The active grid view is
// preserved — the rescan surfaces only as a process-bar entry, and entries
// are repopulated as scan batches land via the usual refresh path.
func (c *Controller) Rebuild() error {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	// Rebuild wipes the thumbs+db that an in-flight warm-up would be writing
	// to, so cancel the warm-up as well — otherwise it races the Wipe call.
	if c.warmUpCancel != nil {
		c.warmUpCancel()
		c.warmUpCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.mu.Unlock()

	dbPath := filepath.Join(c.libraryRoot, ".photo-viewer.db")
	if err := cache.Wipe(dbPath, c.cacheDir); err != nil {
		return err
	}
	idx, err := cache.Load(dbPath)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.index = idx
	c.mu.Unlock()
	go c.scanInto(ctx, c.libraryRoot)
	return nil
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

// DeletePath soft-deletes path: it atomically renames the file into the
// trash dir (which lives on the same filesystem as the library, so the move
// is O(1) regardless of file size), drops its row from the index, and
// transfers its thumbnail to the new path's id so the Trash view can show
// it without regenerating. The active view is refreshed so the deletion is
// visible immediately. If the rename fails for any reason — including the
// trash dir living on a different filesystem — we fall back to an
// asynchronous os.Remove so the UI still updates instantly. Use EmptyTrash
// to actually free the disk space later.
//
// When path is itself already inside the trash dir, DeletePath performs an
// actual unlink (and forgets the thumbnail) — i.e. the user is permanently
// removing one item from the trash. Useful for clearing individual items
// without emptying the whole trash.
//
// Returns any error encountered while moving/removing the file; index/thumb
// cleanup and the refresh happen regardless.
func (c *Controller) DeletePath(path string) error {
	if path == "" {
		return nil
	}

	if c.isInTrash(path) {
		// Inside the Trash view, the "delete" gesture is repurposed as
		// "restore" — the only way to permanently delete is Empty Trash.
		// This matches the convention every desktop file manager uses.
		restored, err := cache.RestoreFromTrash(path, c.store)
		if err != nil {
			log.Printf("trash: restore failed for %s: %v", path, err)
			c.scheduleRefresh(c.activeDir())
			return err
		}
		// Put the restored entry back in the index so it shows up in its
		// original directory without waiting for a rescan. Stat-based —
		// duration/etc. will get backfilled on the next scan if needed.
		if info, sErr := os.Stat(restored); sErr == nil {
			c.index.ReconcileBatch([]scan.Result{{
				Path:    restored,
				Type:    scan.DetectType(restored),
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}})
		}
		c.scheduleRefresh(c.activeDir())
		return nil
	}

	_ = c.index.RemoveEntry(path)
	c.scheduleRefresh(c.activeDir())

	if c.trashDir != "" {
		dst, err := cache.MoveToTrash(path, c.trashDir)
		if err == nil {
			// Carry the thumbnail over to the new path's id so the Trash
			// view doesn't regenerate it. Best-effort — generation will
			// just rerun on demand if rename fails.
			if c.store != nil {
				_ = c.store.Rename(cache.ThumbIDFor(path), cache.ThumbIDFor(dst))
			}
			return nil
		}
		log.Printf("trash: rename failed for %s (%v); falling back to async remove", path, err)
	}
	if c.store != nil {
		c.store.Forget(cache.ThumbIDFor(path))
	}
	var rerr error
	go func() {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("trash: async remove failed for %s: %v", path, rmErr)
		}
	}()
	return rerr
}

// isInTrash reports whether path lives under the trash dir. Used so a
// "delete" gesture inside the Trash view permanently removes the item
// instead of trying to soft-delete it (which would be a no-op rename onto
// itself).
func (c *Controller) isInTrash(path string) bool {
	if c.trashDir == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	prefix := c.trashDir + string(filepath.Separator)
	return strings.HasPrefix(abs, prefix)
}

// TrashDir returns the directory where soft-deleted files live, or "" if no
// writable location was found at startup.
func (c *Controller) TrashDir() string { return c.trashDir }

// TrashStats reports the number of items and total bytes currently in the
// trash. Safe to call from the UI goroutine — it's a cheap stat walk.
func (c *Controller) TrashStats() (count int, bytes int64) {
	return cache.TrashStats(c.trashDir)
}

// EmptyTrash permanently removes every item in the trash dir along with
// each item's cached thumbnail. Runs on a background goroutine so the UI
// isn't blocked by large files; done is invoked on that goroutine once the
// wipe finishes (nil if no callback is wanted).
func (c *Controller) EmptyTrash(done func(count int, bytes int64, err error)) {
	dir := c.trashDir
	if dir == "" {
		if done != nil {
			done(0, 0, nil)
		}
		return
	}
	go func() {
		// Forget thumbnails first so we don't leak them when the source
		// files are gone. List before wiping; the entries' ThumbIDs are
		// derived from the trash paths themselves.
		if c.store != nil {
			for _, e := range cache.ListTrash(dir) {
				c.store.Forget(e.ThumbID)
			}
		}
		count, bytes, err := cache.EmptyTrash(dir)
		// The trash view (if currently shown) needs a refresh so the now-
		// empty list reflects on screen.
		c.scheduleRefresh(c.activeDir())
		if done != nil {
			done(count, bytes, err)
		}
		if c.invalidate != nil {
			c.invalidate()
		}
	}()
}

// ToggleFavorite flips the favorite flag for path and refreshes the active
// view so the change is visible immediately. Returns the new favorite state.
func (c *Controller) ToggleFavorite(path string) bool {
	if path == "" {
		return false
	}
	cur := c.index.IsFavorite(path)
	if err := c.index.SetFavorite(path, !cur); err != nil {
		return cur
	}
	c.scheduleRefresh(c.activeDir())
	return !cur
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
	var entries []cache.Entry
	switch {
	case dir == FavoritesView:
		entries = c.index.ListFavorites()
	case dir == TrashView:
		entries = cache.ListTrash(c.trashDir)
	case strings.HasPrefix(dir, YearViewPrefix):
		c.mu.Lock()
		yearDirs := append([]string(nil), c.yearPreviewDirs...)
		c.mu.Unlock()
		for _, d := range yearDirs {
			entries = append(entries, c.index.ListDir(d)...)
		}
	default:
		entries = c.index.ListDir(dir)
	}

	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	sortMode := c.sortMode
	treeDir := c.treeDir
	root := c.libraryRoot
	prevSubdirs := c.subdirs
	c.mu.Unlock()

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
		trashCount, _ := cache.TrashStats(c.trashDir)
		counts[TrashView] = trashCount
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
	}
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
}

func (c *Controller) scanInto(ctx context.Context, dir string) {
	c.mu.Lock()
	c.scanning++
	c.scanTarget = dir
	c.scanStartedAt = time.Now()
	c.scanEndedAt = time.Time{}
	c.scanBatched = 0
	c.scanLastErr = ""
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}

	// Register with the process bar so the user can pause / resume /
	// cancel from the main screen. Cancellation reuses the existing
	// scanCancel hook so we don't need a second context.
	var proc *Process
	if c.processes != nil {
		proc = c.processes.Begin(ProcScan, "Indexing", c.CancelScan, true)
		proc.SetStatus("Indexing " + filepath.Base(dir))
	}

	defer func() {
		c.mu.Lock()
		c.scanning--
		c.scanEndedAt = time.Now()
		c.mu.Unlock()
		if proc != nil {
			proc.End()
		}
		if c.invalidate != nil {
			c.invalidate()
		}
	}()

	// Pass an index-backed duration lookup so videos that already have a
	// stored duration_ms skip the ffprobe fork on incremental rescans.
	results := scan.WalkWith(ctx, dir, scan.WalkOptions{
		KnownDurationMs: func(path string) int64 {
			if e, ok := c.index.GetEntry(path); ok {
				return e.DurationMs
			}
			return 0
		},
	})
	var batch []scan.Result
	flushAt := time.Now().Add(time.Second)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		n := len(batch)
		c.index.ReconcileBatch(batch)
		batch = batch[:0]
		flushAt = time.Now().Add(time.Second)
		c.mu.Lock()
		c.scanBatched += n
		c.mu.Unlock()
		if proc != nil {
			proc.AddDone(int64(n))
		}
		c.scheduleRefresh(c.activeDir())
	}
	for r := range results {
		if proc != nil {
			proc.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		batch = append(batch, r)
		if len(batch) >= 1000 || (len(batch)%100 == 0 && time.Now().After(flushAt)) {
			flush()
		}
	}
	flush()
	if err := c.index.Save(); err != nil {
		log.Printf("save index: %v", err)
		c.mu.Lock()
		c.scanLastErr = err.Error()
		c.mu.Unlock()
	}
}

// ToggleSelection adds or removes path from the selected set.
func (c *Controller) ToggleSelection(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SelectedPaths[path] {
		delete(c.SelectedPaths, path)
	} else {
		c.SelectedPaths[path] = true
	}
}

// Select adds path to the selected set.
func (c *Controller) Select(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SelectedPaths[path] = true
}

// ClearSelection empties the selected set and exits selection mode.
func (c *Controller) ClearSelection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SelectionMode = false
	c.SelectedPaths = make(map[string]bool)
}

// IsSelected returns whether path is in the selected set.
func (c *Controller) IsSelected(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SelectedPaths[path]
}

func (c *Controller) activeDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentDir
}

// WarmUp iterates over every entry in the index and ensures its thumbnail
// is generated. It runs in the background under its own cancel context, so
// switching directories or running an incremental scan does not abort it —
// only an explicit Cancel (via CancelScan) or another WarmUp invocation
// stops it.
func (c *Controller) WarmUp() {
	c.mu.Lock()
	if c.warmUpCancel != nil {
		c.warmUpCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.warmUpCancel = cancel
	c.warmUpGen++
	myGen := c.warmUpGen
	c.warmUpRunning = true
	c.scanning++
	c.scanTarget = "Thumbnail Warm-up"
	c.scanStartedAt = time.Now()
	c.scanEndedAt = time.Time{}
	c.scanBatched = 0
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}

	go func() {
		defer cancel()
		var proc *Process
		if c.processes != nil {
			proc = c.processes.Begin(ProcWarmUp, "Thumbnails", cancel, true)
			proc.SetStatus("Generating thumbnails…")
		}
		defer func() {
			c.mu.Lock()
			c.scanning--
			c.scanEndedAt = time.Now()
			if c.warmUpGen == myGen {
				c.warmUpRunning = false
				c.warmUpCancel = nil
			}
			c.mu.Unlock()
			if proc != nil {
				proc.End()
			}
			if c.invalidate != nil {
				c.invalidate()
			}
		}()

		total := c.index.Count()
		if proc != nil {
			proc.SetTotal(int64(total))
		}
		// Throttle the redraw notifications so a burst of fast thumb decodes
		// doesn't pile dozens of frame requests onto Gio's loop. The progress
		// bar still ticks at full granularity via proc.SetDone.
		var lastInv time.Time
		var i int
		c.index.ForEachEntry(func(e cache.Entry) bool {
			if proc != nil {
				proc.Wait()
			}
			if ctx.Err() != nil {
				return false
			}
			_, _ = c.store.Path(e)
			i++
			if proc != nil {
				proc.SetDone(int64(i))
			}
			if i%50 == 0 || i == total {
				c.mu.Lock()
				c.scanBatched = i
				c.mu.Unlock()
			}
			if c.invalidate != nil && time.Since(lastInv) >= 33*time.Millisecond {
				c.invalidate()
				lastInv = time.Now()
			}
			return true
		})
	}()
}

// sortEntries orders the slice in place according to mode. SortByDuration
// puts the longest videos first; entries without a duration (photos, RAW,
// HEIC, or videos whose duration couldn't be probed) fall to the end ordered
// by path so the listing stays stable.
func sortEntries(entries []cache.Entry, mode string) {
	switch normalizeSort(mode) {
	case SortByDuration:
		sort.SliceStable(entries, func(i, j int) bool {
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
	default:
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	}
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
	sort.Strings(out)
	return out
}
