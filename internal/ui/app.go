package ui

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"

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
	grid    *ThumbGrid

	mu          sync.Mutex
	currentDir  string
	scanCancel  context.CancelFunc
}

func NewController(window fyne.Window, libraryRoot string, idx *cache.Index, store *cache.ThumbStore, cacheDir string) *Controller {
	c := &Controller{
		window:      window,
		libraryRoot: libraryRoot,
		cacheDir:    cacheDir,
		index:       idx,
		store:       store,
		currentDir:  libraryRoot,
	}
	c.toolbar = NewToolbar(c.rebuildIndex, c.showSettings, c.runImport)
	c.grid = NewThumbGrid(store, func(index int, entries []cache.Entry) {
		Open(c.window, index, entries, c.store)
	})
	
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyMinus, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(false)
	})
	window.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyEqual, Modifier: fyne.KeyModifierShortcutDefault}, func(shortcut fyne.Shortcut) {
		c.grid.HandleZoom(true)
	})
	
	c.toolbar.SetPath(libraryRoot)
	return c
}

func (c *Controller) Sidebar() fyne.CanvasObject {
	return NewSidebar(c.libraryRoot, c.SelectDir)
}

func (c *Controller) Build() fyne.CanvasObject {
	right := container.NewBorder(c.toolbar.Widget(), nil, nil, nil, c.grid.Widget())
	split := container.NewHSplit(c.Sidebar(), right)
	split.SetOffset(0.22)
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
	c.mu.Unlock()

	fyne.Do(func() {
		c.toolbar.SetPath(path)
		c.refreshFromIndex(path)
	})
	go c.scanInto(ctx, path, false)
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

// refreshFromIndex pushes the cached entries that live under dir to the grid.
func (c *Controller) refreshFromIndex(dir string) {
	filtered := c.index.ListDir(dir)
	sortEntries(filtered)
	c.grid.SetEntries(filtered)
	c.toolbar.SetCount(len(filtered))
}

// scanInto walks dir, reconciles results into the index, updates the grid,
// and persists the index. Always runs in a background goroutine.
func (c *Controller) scanInto(ctx context.Context, dir string, fullRebuild bool) {
	fyne.Do(func() { c.toolbar.ShowBusy(true) })
	defer fyne.Do(func() { c.toolbar.ShowBusy(false) })

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
		
		entries := c.index.ReconcileBatch(resultsBatch)
		
		active := c.activeDir()
		needsRefresh := false
		prefix := withSep(active)
		for _, e := range entries {
			if strings.HasPrefix(e.Path, prefix) || filepath.Dir(e.Path) == active {
				needsRefresh = true
				break
			}
		}

		if needsRefresh {
			fyne.Do(func() { c.refreshFromIndex(active) })
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
