package ui

import (
	"context"
	"fmt"
	"log"
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
	sidebar *SidebarTree
	grid    *ThumbGrid

	mu             sync.Mutex
	currentDir     string
	mediaFilter    string
	favoritesView  bool
	scanCancel     context.CancelFunc
	scanState      ScanState

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
	c := &Controller{
		window:      window,
		libraryRoot: libraryRoot,
		cacheDir:    cacheDir,
		index:       idx,
		store:       store,
		currentDir:  libraryRoot,
		mediaFilter: "All",
	}
	c.toolbar = NewToolbar(c.setFilter, c.rebuildIndex, c.showSettings, c.runImport, c.runSDCardImport, c.showDuplicates, c.toggleFavoritesView, c.showScanInfo)
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
			c.window.Canvas().Focus(c.sidebar)
		}
	}

	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyMinus, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(false)
	})
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyEqual, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(true)
	})

	c.toolbar.SetPath(libraryRoot)
	c.sidebar = NewSidebar(c.libraryRoot, c.index, c.SelectDir)
	c.sidebar.OnTab = func() {
		if c.grid != nil && c.grid.grid != nil {
			c.window.Canvas().Focus(c.grid.grid)
		}
	}
	c.sidebar.OnEnterOrL = func() {
		if c.grid != nil && c.grid.grid != nil {
			c.window.Canvas().Focus(c.grid.grid)
		}
	}

	return c
}

func (c *Controller) Build() fyne.CanvasObject {
	right := container.NewBorder(c.toolbar.Widget(), nil, nil, nil, c.grid.Widget())
	split := container.NewHSplit(c.sidebar, right)
	split.SetOffset(0.22)
	c.mainContent = split
	return split
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
	c.favoritesView = false
	c.mu.Unlock()

	fyne.Do(func() {
		c.toolbar.SetPath(path)
		c.toolbar.SetFavoritesActive(false)
	})
	go c.refreshFromIndex(path)
	go c.scanInto(ctx, path, false)
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

// refreshFavorites pushes every favorite entry into the grid.
func (c *Controller) refreshFavorites() {
	entries := c.index.ListFavorites()
	sortEntries(entries)
	fyne.Do(func() {
		c.grid.SetEntries(entries)
		c.toolbar.SetCount(len(entries))
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
	c.grid.SetEntries(nil)
	c.toolbar.SetCount(0)
	go c.scanInto(ctx, c.libraryRoot, true)
}

func (c *Controller) showDuplicates() {
	c.grid.SetActive(false)
	ShowDuplicates(c.window, c.index, c.store, func() {
		c.window.SetContent(c.mainContent)
		c.grid.SetActive(true)
	})
}

func (c *Controller) setFilter(f string) {
	c.mu.Lock()
	c.mediaFilter = f
	c.mu.Unlock()
	go c.refreshFromIndex(c.activeDir())
}

// refreshFromIndex pushes the cached entries that live under dir to the grid.
func (c *Controller) refreshFromIndex(dir string) {
	entries := c.index.ListDir(dir)

	c.mu.Lock()
	filter := c.mediaFilter
	c.mu.Unlock()

	var filtered []cache.Entry
	for _, e := range entries {
		if filter == "Photos" && e.Type == scan.TypeVideo {
			continue
		}
		if filter == "Videos" && e.Type != scan.TypeVideo {
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

// scanInto walks dir, reconciles results into the index, updates the grid,
// and persists the index. Always runs in a background goroutine.
func (c *Controller) scanInto(ctx context.Context, dir string, fullRebuild bool) {
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
		c.mu.Unlock()

		active := c.activeDir()
		prefix := withSep(active)
		var forGrid []cache.Entry
		for _, e := range entries {
			if !strings.HasPrefix(e.Path, prefix) {
				continue
			}
			if filter == "Photos" && e.Type == scan.TypeVideo {
				continue
			}
			if filter == "Videos" && e.Type != scan.TypeVideo {
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
