package ui

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
	"github.com/dns/photo-viewer/internal/scan"
)

// Controller owns the index, the active filter, and the UI widgets.
type Controller struct {
	window      fyne.Window
	libraryRoot string
	cacheDir    string

	index *cache.Index
	store *cache.ThumbStore

	toolbar *Toolbar
	sidebar *Sidebar
	people  *PeopleList
	grid    *ThumbGrid

	faces *face.Pipeline

	mu            sync.Mutex
	currentDir    string
	currentYear   int // > 0 = year-grouped view active
	mediaFilter   string
	showRAW       bool
	favoritesView bool
	peopleView    int64 // > 0 = currently filtered to a cluster id
	scanCancel    context.CancelFunc
	scanState     ScanState
	scansPaused   bool

	mainContent fyne.CanvasObject
}

// ScanState is a snapshot of an in-progress index scan.
type ScanState struct {
	Active         bool
	Dir            string
	FullRebuild    bool
	FilesSeen      int
	BatchesFlushed int
	LastBatchSize  int
	StartTime      time.Time
	EndTime        time.Time
	LastPath       string
}

func NewController(window fyne.Window, libraryRoot string, idx *cache.Index, store *cache.ThumbStore, cacheDir string) *Controller {
	showRAW := fyne.CurrentApp().Preferences().BoolWithFallback("ShowRAW", true)
	c := &Controller{
		window:      window,
		libraryRoot: libraryRoot,
		cacheDir:    cacheDir,
		index:       idx,
		store:       store,
		currentDir:  libraryRoot,
		mediaFilter: "All",
		showRAW:     showRAW,
	}
	c.toolbar = NewToolbar(c.setFilter, c.setShowRAW, showRAW, c.rebuildIndex, c.showSettings, c.runImport, c.runSDCardImport, c.showDuplicates, c.toggleFavoritesView, c.showScanInfo, c.showPeople)
	c.grid = NewThumbGrid(window, store, func(index int, entries []cache.Entry) {
		Open(c.window, index, entries, c.store, c.index, func() {
			c.mu.Lock()
			fav := c.favoritesView
			dir := c.currentDir
			c.mu.Unlock()
			if fav {
				go c.refreshFavorites()
			} else {
				go c.refreshFromIndex(dir)
			}
		})
	})

	c.grid.OnTab = func() {
		if c.sidebar != nil {
			c.window.Canvas().Focus(c.sidebar.Tree())
		}
	}
	c.grid.OnToggleFavorite = c.toggleFavoriteEntry
	c.grid.OnDelete = c.deleteEntry

	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyMinus, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(false)
	})
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyEqual, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(true)
	})
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyF, Modifier: fyne.KeyModifierControl}, func(shortcut fyne.Shortcut) {
		c.pageJump(true)
	})
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyB, Modifier: fyne.KeyModifierControl}, func(shortcut fyne.Shortcut) {
		c.pageJump(false)
	})

	c.toolbar.SetPath(libraryRoot)
	c.sidebar = NewSidebar(c.libraryRoot, c.index, c.SelectDir, c.SelectYear)
	c.sidebar.SetFilter(c.mediaFilter, c.showRAW)
	c.sidebar.Tree().OnTab = func() {
		if c.grid != nil && c.grid.grid != nil {
			c.window.Canvas().Focus(c.grid.grid)
		}
	}
	c.sidebar.Tree().OnEnterOrL = func() {
		if c.grid != nil && c.grid.grid != nil {
			c.window.Canvas().Focus(c.grid.grid)
		}
	}

	c.people = NewPeopleList(c.index, c.loadClusterAvatar, c.selectCluster, c.showClusterMenu)
	c.faces = face.NewPipeline(c.index, func() {
		fyne.Do(func() { c.people.Refresh() })
	})
	c.faces.Start(context.Background())

	return c
}

func (c *Controller) Build() fyne.CanvasObject {
	right := container.NewBorder(c.toolbar.Widget(), nil, nil, nil, c.grid.Widget())
	left := c.buildSidebar()
	split := container.NewHSplit(left, right)
	split.SetOffset(0.22)
	c.mainContent = split
	return split
}

// buildSidebar stacks the directory tree above the People panel. The People
// panel stays visible even when face detection is off — clicking the toolbar
// button surfaces install instructions.
func (c *Controller) buildSidebar() fyne.CanvasObject {
	return container.NewVSplit(c.sidebar.Widget(), c.people.Widget())
}

// SelectDir is the callback the sidebar invokes when the user picks a folder.
// May be called from any goroutine.
func (c *Controller) SelectDir(path string) {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.currentDir = path
	c.currentYear = 0
	c.favoritesView = false
	c.mu.Unlock()

	fyne.Do(func() {
		c.toolbar.SetPath(path)
		c.toolbar.SetFavoritesActive(false)
	})
	go c.refreshFromIndex(path)
	go c.scanInto(ctx, path, false)
}

// SelectYear is the sidebar callback for year-grouped browsing. It cancels
// any in-flight scan, switches the active view, and pushes the matching
// entries to the grid.
func (c *Controller) SelectYear(year int) {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
		c.scanCancel = nil
	}
	c.currentYear = year
	c.favoritesView = false
	c.peopleView = 0
	c.mu.Unlock()

	fyne.Do(func() {
		c.toolbar.SetFavoritesActive(false)
	})
	go c.refreshYear(year)
}

// refreshYear pushes entries from the given year (filtered by the active
// media-type rules) into the grid.
func (c *Controller) refreshYear(year int) {
	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	c.mu.Unlock()

	entries := c.index.ListByYear(year, filter, showRAW)
	sortEntries(entries)
	title := fmt.Sprintf("📅 %d", year)
	fyne.Do(func() {
		c.grid.SetEntries(entries)
		c.toolbar.SetCount(len(entries))
		c.toolbar.SetPath(title)
	})
}

// toggleFavoritesView switches between the directory view and a flat list of
// every entry flagged as a favorite.
func (c *Controller) toggleFavoritesView() {
	c.mu.Lock()
	c.favoritesView = !c.favoritesView
	on := c.favoritesView
	dir := c.currentDir
	c.mu.Unlock()

	fyne.Do(func() {
		c.toolbar.SetFavoritesActive(on)
	})

	if on {
		go c.refreshFavorites()
	} else {
		fyne.Do(func() { c.toolbar.SetPath(dir) })
		go c.refreshFromIndex(dir)
	}
}

// toggleFavoriteEntry flips the favorite flag for entry, updates the cell,
// and (if the favorites view is active) drops the entry from the grid when
// it's unfavorited.
func (c *Controller) toggleFavoriteEntry(entry cache.Entry, _ int) {
	newVal := !entry.Favorite
	if err := c.index.SetFavorite(entry.Path, newVal); err != nil {
		log.Printf("set favorite %s: %v", entry.Path, err)
		return
	}
	entry.Favorite = newVal

	c.mu.Lock()
	favView := c.favoritesView
	c.mu.Unlock()

	if favView && !newVal {
		go c.refreshFavorites()
		return
	}
	c.grid.UpdateEntry(entry)
}

// deleteEntry confirms with the user, removes the file from disk, drops it
// from the index/thumb store, and refreshes the active view.
func (c *Controller) deleteEntry(entry cache.Entry, _ int) {
	msg := "Delete this file?\n\n" + entry.Path
	dialog.ShowConfirm("Delete file?", msg, func(ok bool) {
		if !ok {
			return
		}
		if err := os.Remove(entry.Path); err != nil {
			dialog.ShowError(err, c.window)
			return
		}
		if err := c.index.RemoveEntry(entry.Path); err != nil {
			log.Printf("remove entry %s: %v", entry.Path, err)
		}
		c.store.Forget(cache.ThumbIDFor(entry.Path))

		c.mu.Lock()
		favView := c.favoritesView
		dir := c.currentDir
		c.mu.Unlock()
		if favView {
			go c.refreshFavorites()
		} else {
			go c.refreshFromIndex(dir)
		}
	}, c.window)
}

// refreshFavorites pushes every favorite entry into the grid.
func (c *Controller) refreshFavorites() {
	entries := c.index.ListFavorites()

	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	c.mu.Unlock()

	var filtered []cache.Entry
	for _, e := range entries {
		if !passesFilter(e, filter, showRAW) {
			continue
		}
		filtered = append(filtered, e)
	}

	sortEntries(filtered)
	fyne.Do(func() {
		c.grid.SetEntries(filtered)
		c.toolbar.SetCount(len(filtered))
		c.toolbar.SetPath("★ Favorites")
	})
}

// rebuildIndex wipes the cache and rescans the library root.
func (c *Controller) rebuildIndex() {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.mu.Unlock()

	dbPath := filepath.Join(c.libraryRoot, ".photo-viewer.db")
	if err := cache.Wipe(dbPath, c.cacheDir); err != nil {
		log.Printf("wipe cache: %v", err)
	}
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Printf("reload index: %v", err)
		return
	}
	c.index = idx
	if err := c.index.WipeFaces(); err != nil {
		log.Printf("wipe faces: %v", err)
	}
	if c.faces != nil {
		c.faces.InvalidateClusters()
	}
	c.grid.SetEntries(nil)
	c.toolbar.SetCount(0)
	if c.people != nil {
		c.people.Refresh()
	}
	go c.scanInto(ctx, c.libraryRoot, true)
}

func (c *Controller) showDuplicates() {
	c.grid.SetActive(false)
	ShowDuplicates(c.window, c.index, c.store, func() {
		c.window.SetContent(c.mainContent)
		c.grid.SetActive(true)
	})
}

// showPeople opens the dedicated People view. Picking a person closes the
// view and applies the cluster filter to the main grid (reusing
// selectCluster).
func (c *Controller) showPeople() {
	c.grid.SetActive(false)
	ShowPeople(c.window, c.index, PeopleViewActions{
		LoadAvatar: c.loadClusterAvatar,
		OnPick: func(clusterID int64, label string) {
			c.window.SetContent(c.mainContent)
			c.grid.SetActive(true)
			c.selectCluster(clusterID, label)
		},
		OnClose: func() {
			c.window.SetContent(c.mainContent)
			c.grid.SetActive(true)
		},
		OnRecheck: c.recheckFaces,
		OnRunNow:  c.runFacesOnLibrary,
	})
}

// recheckFaces re-probes the helper, restarts the worker pool if it newly
// became usable, and returns the fresh status for the UI.
func (c *Controller) recheckFaces() face.Status {
	s := c.faces.Recheck()
	if s.Working {
		c.faces.Start(context.Background())
	}
	return s
}

// runFacesOnLibrary submits every photo entry for face detection. Background
// work; subprocess concurrency is gated by the pipeline worker pool so this
// won't overwhelm the system.
func (c *Controller) runFacesOnLibrary() {
	if !c.faces.Enabled() {
		return
	}
	entries := c.index.All()
	go c.submitFaces(context.Background(), entries)
}

func (c *Controller) setFilter(f string) {
	c.mu.Lock()
	c.mediaFilter = f
	showRAW := c.showRAW
	c.mu.Unlock()
	if c.sidebar != nil {
		c.sidebar.SetFilter(f, showRAW)
	}
	go c.refreshActiveView()
}

func (c *Controller) setShowRAW(on bool) {
	c.mu.Lock()
	c.showRAW = on
	filter := c.mediaFilter
	c.mu.Unlock()
	fyne.CurrentApp().Preferences().SetBool("ShowRAW", on)
	if c.sidebar != nil {
		c.sidebar.SetFilter(filter, on)
	}
	go c.refreshActiveView()
}

// refreshActiveView re-renders whichever view is currently showing —
// favorites, year-grouped, or the active directory. Cluster view is left
// alone; selecting a person always reapplies fresh filtering.
func (c *Controller) refreshActiveView() {
	c.mu.Lock()
	fav := c.favoritesView
	dir := c.currentDir
	year := c.currentYear
	c.mu.Unlock()
	switch {
	case fav:
		c.refreshFavorites()
	case year > 0:
		c.refreshYear(year)
	default:
		c.refreshFromIndex(dir)
	}
}

// refreshFromIndex pushes the cached entries that live under dir to the grid.
func (c *Controller) refreshFromIndex(dir string) {
	entries := c.index.ListDir(dir)

	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	c.mu.Unlock()

	var filtered []cache.Entry
	for _, e := range entries {
		if !passesFilter(e, filter, showRAW) {
			continue
		}
		filtered = append(filtered, e)
	}

	sortEntries(filtered)
	fyne.Do(func() {
		c.grid.SetEntries(filtered)
		c.toolbar.SetCount(len(filtered))
	})
}

// passesFilter returns true if entry should be visible under the current
// media-type filter and RAW toggle.
func passesFilter(e cache.Entry, filter string, showRAW bool) bool {
	if !showRAW && e.Type == scan.TypeRAW {
		return false
	}
	switch filter {
	case "Photos":
		return e.Type != scan.TypeVideo
	case "Videos":
		return e.Type == scan.TypeVideo
	}
	return true
}

// pauseScans cancels any in-flight scan and prevents new scans from starting
// until resumeScans is called. Used during imports so the indexer doesn't
// fight the import for disk I/O.
func (c *Controller) pauseScans() {
	c.mu.Lock()
	c.scansPaused = true
	cancel := c.scanCancel
	c.scanCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// resumeScans clears the pause flag and kicks off a fresh incremental scan of
// the current directory so newly imported files appear.
func (c *Controller) resumeScans() {
	c.mu.Lock()
	c.scansPaused = false
	dir := c.currentDir
	if c.scanCancel != nil {
		c.scanCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	c.mu.Unlock()
	go c.scanInto(ctx, dir, false)
}

// scanInto walks dir, reconciles results into the index, updates the grid,
// and persists the index. Always runs in a background goroutine.
func (c *Controller) scanInto(ctx context.Context, dir string, fullRebuild bool) {
	c.mu.Lock()
	paused := c.scansPaused
	c.mu.Unlock()
	if paused {
		return
	}
	fyne.Do(func() { c.toolbar.ShowBusy(true) })
	c.mu.Lock()
	c.scanState = ScanState{
		Active:      true,
		Dir:         dir,
		FullRebuild: fullRebuild,
		StartTime:   time.Now(),
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.scanState.Active = false
		c.scanState.EndTime = time.Now()
		c.mu.Unlock()
		fyne.Do(func() { c.toolbar.ShowBusy(false) })
	}()

	var seen map[string]struct{}
	if fullRebuild {
		seen = make(map[string]struct{})
	}

	var resultsBatch []scan.Result
	lastFlush := time.Now()

	flush := func(force bool) {
		if len(resultsBatch) == 0 {
			return
		}
		if !force && time.Since(lastFlush) < time.Second {
			return
		}

		batchSize := len(resultsBatch)
		entries := c.index.ReconcileBatch(resultsBatch)

		c.mu.Lock()
		c.scanState.BatchesFlushed++
		c.scanState.LastBatchSize = batchSize
		filter := c.mediaFilter
		showRAW := c.showRAW
		c.mu.Unlock()

		active := c.activeDir()
		prefix := withSep(active)
		var forGrid []cache.Entry
		for _, e := range entries {
			if !strings.HasPrefix(e.Path, prefix) {
				continue
			}
			if !passesFilter(e, filter, showRAW) {
				continue
			}
			forGrid = append(forGrid, e)
		}

		if len(forGrid) > 0 {
			fyne.Do(func() {
				c.grid.MergeEntries(forGrid)
				c.toolbar.SetCount(c.grid.Count())
			})
		}

		// Face detection runs decoupled from the flush: a goroutine
		// resolves thumbnails (which is rate-limited inside ThumbStore)
		// and submits jobs to the face worker pool. Cancellation is via
		// the same scan ctx so switching directories aborts in-flight
		// resolution.
		if c.faces != nil && c.faces.Enabled() && len(entries) > 0 {
			toFace := append([]cache.Entry(nil), entries...)
			go c.submitFaces(ctx, toFace)
		}

		resultsBatch = resultsBatch[:0]
		lastFlush = time.Now()
	}

	results := scan.Walk(ctx, dir)
	for r := range results {
		if ctx.Err() != nil {
			return
		}
		if fullRebuild {
			seen[r.Path] = struct{}{}
		}
		resultsBatch = append(resultsBatch, r)
		c.mu.Lock()
		c.scanState.FilesSeen++
		c.scanState.LastPath = r.Path
		c.mu.Unlock()
		flush(false)
	}
	flush(true)

	if fullRebuild {
		removed := c.index.Prune(seen)
		for _, p := range removed {
			c.store.Forget(cache.ThumbIDFor(p))
		}
	}
	fyne.Do(func() { c.people.Refresh() })
	if err := c.index.Save(); err != nil {
		log.Printf("save index: %v", err)
	}
	active := c.activeDir()
	fyne.Do(func() { c.refreshFromIndex(active) })
}

// snapshotScanState returns a copy of the current scan state.
func (c *Controller) snapshotScanState() ScanState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanState
}

// showScanInfo opens a dialog with live indexing status. The dialog refreshes
// every 250ms while open and stops once the user closes it.
func (c *Controller) showScanInfo() {
	statusVal := widget.NewLabel("")
	dirVal := widget.NewLabel("")
	modeVal := widget.NewLabel("")
	startedVal := widget.NewLabel("")
	elapsedVal := widget.NewLabel("")
	seenVal := widget.NewLabel("")
	flushedVal := widget.NewLabel("")
	lastBatchVal := widget.NewLabel("")
	lastPathVal := widget.NewLabel("")
	lastPathVal.Wrapping = fyne.TextWrapWord

	form := container.New(layout.NewFormLayout(),
		widget.NewLabelWithStyle("Status:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), statusVal,
		widget.NewLabelWithStyle("Directory:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), dirVal,
		widget.NewLabelWithStyle("Mode:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), modeVal,
		widget.NewLabelWithStyle("Started:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), startedVal,
		widget.NewLabelWithStyle("Elapsed:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), elapsedVal,
		widget.NewLabelWithStyle("Files seen:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), seenVal,
		widget.NewLabelWithStyle("Batches flushed:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), flushedVal,
		widget.NewLabelWithStyle("Last batch size:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), lastBatchVal,
		widget.NewLabelWithStyle("Last file:", fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}), lastPathVal,
	)

	refresh := func() {
		s := c.snapshotScanState()
		if s.Active {
			statusVal.SetText("Indexing…")
		} else if s.StartTime.IsZero() {
			statusVal.SetText("Idle (no scan run yet)")
		} else {
			statusVal.SetText("Idle (last scan finished)")
		}
		dirVal.SetText(s.Dir)
		if s.FullRebuild {
			modeVal.SetText("Full rebuild")
		} else {
			modeVal.SetText("Incremental")
		}
		if s.StartTime.IsZero() {
			startedVal.SetText("—")
			elapsedVal.SetText("—")
		} else {
			startedVal.SetText(s.StartTime.Format("15:04:05"))
			end := s.EndTime
			if s.Active || end.IsZero() {
				end = time.Now()
			}
			elapsedVal.SetText(end.Sub(s.StartTime).Round(time.Millisecond).String())
		}
		seenVal.SetText(fmt.Sprintf("%d", s.FilesSeen))
		flushedVal.SetText(fmt.Sprintf("%d", s.BatchesFlushed))
		lastBatchVal.SetText(fmt.Sprintf("%d", s.LastBatchSize))
		if s.LastPath == "" {
			lastPathVal.SetText("—")
		} else {
			lastPathVal.SetText(s.LastPath)
		}
	}
	refresh()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fyne.Do(refresh)
			}
		}
	}()

	d := dialog.NewCustom("Indexing", "Close", form, c.window)
	d.Resize(fyne.NewSize(520, 360))
	d.SetOnClosed(func() { close(stop) })
	d.Show()
}

// submitFaces resolves thumbnails for the just-reconciled entries and queues
// each into the face pipeline. Runs entirely in the background.
func (c *Controller) submitFaces(ctx context.Context, entries []cache.Entry) {
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		switch e.Type {
		case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
		default:
			continue
		}
		thumb, err := c.store.Path(e)
		if err != nil || thumb == "" {
			continue
		}
		info, err := os.Stat(thumb)
		if err != nil {
			continue
		}
		c.faces.Submit(face.Job{
			Entry:     e,
			ThumbPath: thumb,
			ThumbMod:  info.ModTime().Unix(),
		})
	}
}

// selectCluster filters the grid to photos that contain a face from the
// given cluster.
func (c *Controller) selectCluster(clusterID int64, label string) {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	c.peopleView = clusterID
	c.favoritesView = false
	c.mu.Unlock()

	paths := c.index.PathsInCluster(clusterID)
	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p] = struct{}{}
	}

	c.mu.Lock()
	filter := c.mediaFilter
	showRAW := c.showRAW
	c.mu.Unlock()

	all := c.index.All()
	var entries []cache.Entry
	for _, e := range all {
		if _, ok := pathSet[e.Path]; !ok {
			continue
		}
		if !passesFilter(e, filter, showRAW) {
			continue
		}
		entries = append(entries, e)
	}
	sortEntries(entries)

	title := label
	if title == "" {
		title = fmt.Sprintf("Person #%d", clusterID)
	}
	fyne.Do(func() {
		c.toolbar.SetPath("👤 " + title)
		c.toolbar.SetFavoritesActive(false)
		c.grid.SetEntries(entries)
		c.toolbar.SetCount(len(entries))
	})
}

// loadClusterAvatar resolves a cluster's sample face into a tiny JPEG icon,
// cropping the bbox out of the source thumbnail and caching the result.
// Returns nil on any failure so the row falls back to a generic person icon.
func (c *Controller) loadClusterAvatar(cl cache.Cluster) fyne.Resource {
	if !cl.SampleFaceID.Valid {
		return nil
	}
	f, ok := c.index.SampleFace(cl.SampleFaceID.Int64)
	if !ok {
		return nil
	}
	entry, ok := c.index.GetEntry(f.Path)
	if !ok {
		return nil
	}
	srcThumb, err := c.store.Path(entry)
	if err != nil || srcThumb == "" {
		return nil
	}
	dst, err := face.EnsureThumb(c.cacheDir, srcThumb, f.ID, f.BBox)
	if err != nil {
		return nil
	}
	res, err := fyne.LoadResourceFromPath(dst)
	if err != nil {
		return nil
	}
	return res
}

// showClusterMenu pops a Rename / Merge menu at the right-click position.
func (c *Controller) showClusterMenu(clusterID int64, current string, abs fyne.Position) {
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Rename…", func() { c.renameCluster(clusterID, current) }),
		fyne.NewMenuItem("Merge into…", func() { c.mergeClusterDialog(clusterID) }),
	)
	pop := widget.NewPopUpMenu(menu, c.window.Canvas())
	pop.ShowAtPosition(abs)
}

// mergeClusterDialog asks the user which other cluster to merge into and
// performs the merge. The People list refreshes on completion.
func (c *Controller) mergeClusterDialog(srcID int64) {
	clusters := c.index.AllClusters()
	var labels []string
	var ids []int64
	for _, cl := range clusters {
		if cl.ID == srcID {
			continue
		}
		name := cl.Label.String
		if !cl.Label.Valid || name == "" {
			name = fmt.Sprintf("Person #%d", cl.ID)
		}
		labels = append(labels, fmt.Sprintf("%s (%d)", name, cl.Count))
		ids = append(ids, cl.ID)
	}
	if len(ids) == 0 {
		dialog.ShowInformation("Merge person", "No other people to merge into.", c.window)
		return
	}
	sel := widget.NewSelect(labels, nil)
	sel.PlaceHolder = "Select a person"
	form := dialog.NewForm("Merge person", "Merge", "Cancel",
		[]*widget.FormItem{{Text: "Into", Widget: sel}},
		func(ok bool) {
			if !ok {
				return
			}
			idx := -1
			for i, l := range labels {
				if l == sel.Selected {
					idx = i
					break
				}
			}
			if idx < 0 {
				return
			}
			dst := ids[idx]
			if err := c.index.MergeClusters(srcID, dst); err != nil {
				log.Printf("merge clusters %d → %d: %v", srcID, dst, err)
				return
			}
			if c.faces != nil {
				c.faces.InvalidateClusters()
			}
			c.people.Refresh()
		},
		c.window,
	)
	form.Show()
}

// renameCluster prompts the user for a new label and writes it back to the
// index, then refreshes the People list.
func (c *Controller) renameCluster(clusterID int64, current string) {
	entry := widget.NewEntry()
	entry.SetText(current)
	form := dialog.NewForm("Rename person", "Save", "Cancel",
		[]*widget.FormItem{{Text: "Name", Widget: entry}},
		func(ok bool) {
			if !ok {
				return
			}
			if err := c.index.RenameCluster(clusterID, entry.Text); err != nil {
				log.Printf("rename cluster %d: %v", clusterID, err)
				return
			}
			c.people.Refresh()
		},
		c.window,
	)
	form.Show()
}

// pageJump dispatches Ctrl+F / Ctrl+B to whichever of the grid or sidebar
// tree currently has keyboard focus.
func (c *Controller) pageJump(forward bool) {
	switch f := c.window.Canvas().Focused().(type) {
	case *customGridWrap:
		_ = f
		c.grid.PageJump(forward)
	case *SidebarTree:
		f.PageJump(forward)
	}
}

func (c *Controller) activeDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentDir
}

func withSep(dir string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir
	}
	return dir + string(filepath.Separator)
}

func sortEntries(es []cache.Entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Path < es[j].Path })
}
