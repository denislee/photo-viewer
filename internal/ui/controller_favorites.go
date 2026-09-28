package ui

import (
	"context"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/export"
)

// ExportFavoritesOptions carries the user-tunable knobs for ExportFavorites.
// Defaults (zero values) mean: copy, preserve subfolders, no recompression.
type ExportFavoritesOptions struct {
	Dst         string
	Move        bool
	Flatten     bool
	MaxLongEdge int // > 0 enables image/video recompression
}

// ExportFavorites copies (or moves) every entry currently flagged as a
// favorite into opts.Dst. When opts.MaxLongEdge > 0, images and videos
// are recompressed to fit that pixel limit. Runs on a goroutine; appears
// in the process bar with Cancel support, and done is invoked once the
// work finishes (nil if no callback is wanted).
func (c *Controller) ExportFavorites(opts ExportFavoritesOptions, done func(res export.Result, err error)) {
	if opts.Dst == "" {
		if done != nil {
			done(export.Result{}, nil)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var proc *Process
		if c.processes != nil {
			title := "Export favorites: copy"
			switch {
			case opts.Move:
				title = "Export favorites: move"
			case opts.MaxLongEdge > 0:
				title = "Export favorites: recompress"
			}
			proc = c.processes.Begin(ProcExportFavorites, title, cancel, false)
			proc.SetStatus("Starting…")
			defer proc.End()
		}
		progress := func(step, total int, action, msg string) {
			if proc != nil {
				proc.SetTotal(int64(total))
				proc.SetDone(int64(step))
				proc.SetStatus(action + " " + msg)
			}
		}
		res, err := export.Favorites(ctx, c.index, export.Options{
			Root:        c.libraryRoot,
			Dst:         opts.Dst,
			Flatten:     opts.Flatten,
			Move:        opts.Move,
			MaxLongEdge: opts.MaxLongEdge,
		}, progress)
		if done != nil {
			done(res, err)
		}
		if c.invalidate != nil {
			c.invalidate()
		}
	}()
}

// favWrite is one queued favorite write: set path's flag to want.
type favWrite struct {
	path string
	want bool
}

// FavoriteRevert reports a favorite toggle whose write failed and was undone:
// Favorite is the restored value.
type FavoriteRevert struct {
	Path     string
	Favorite bool
}

// favoriteWarnFn tells the user a favorite could not be saved. A package var
// only so tests can capture the message instead of spawning zenity.
var favoriteWarnFn = func(msg string) {
	_, _ = runZenity("--warning", "--no-markup", "--title=Favorite not saved", "--text="+msg)
}

// ToggleFavorite flips path's favorite flag from current and returns the new
// value. The grid and sidebar counts update at once (patchFavorite, an
// in-memory patch rather than a refreshFromIndex, so rating a 10k-photo shoot
// with `f` doesn't re-query 10k rows per keypress); the DB write runs in the
// background (G-14) so a WAL checkpoint or slow disk can't stall the frame
// goroutine. Writes store the absolute value and are applied in order, so the
// DB ends up matching the last press. If a write fails, the flip is undone,
// the viewer is told via TakeFavoriteReverts, and the user gets a warning.
func (c *Controller) ToggleFavorite(path string, current bool) bool {
	if path == "" {
		return current
	}
	want := !current
	c.patchFavorite(path, want)
	c.favMu.Lock()
	c.favQueue = append(c.favQueue, favWrite{path: path, want: want})
	start := !c.favRunning
	c.favRunning = true
	c.favMu.Unlock()
	if start {
		go c.drainFavorites()
	}
	return want
}

// drainFavorites applies queued favorite writes until the queue is empty.
func (c *Controller) drainFavorites() {
	for {
		c.favMu.Lock()
		if len(c.favQueue) == 0 {
			c.favRunning = false
			c.favMu.Unlock()
			return
		}
		fw := c.favQueue[0]
		c.favQueue = c.favQueue[1:]
		c.favMu.Unlock()
		c.writeFavorite(fw)
	}
}

func (c *Controller) writeFavorite(fw favWrite) {
	err := c.index.SetFavorite(fw.path, fw.want)
	if err == nil {
		if c.activeDir() == FavoritesView {
			// Un-favoriting here removes the row from the favorites listing,
			// which only a full re-query can do — patch-in-place can't drop rows.
			c.scheduleRefresh(FavoritesView)
		}
		return
	}
	log.Printf("favorite %s: %v", fw.path, err)
	c.patchFavorite(fw.path, !fw.want)
	c.favMu.Lock()
	c.favReverts = append(c.favReverts, FavoriteRevert{Path: fw.path, Favorite: !fw.want})
	c.favMu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
	go favoriteWarnFn(fmt.Sprintf("Could not save the favorite flag for %s: %v", filepath.Base(fw.path), err))
}

// TakeFavoriteReverts returns (and clears) the favorite toggles undone since
// the last call, for the UI goroutine to apply to state it owns (the viewer).
func (c *Controller) TakeFavoriteReverts() []FavoriteRevert {
	c.favMu.Lock()
	defer c.favMu.Unlock()
	r := c.favReverts
	c.favReverts = nil
	return r
}

// FlushFavorites waits (up to timeout) for queued favorite writes to finish.
// Called on window close so a press right before quitting isn't lost.
func (c *Controller) FlushFavorites(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.favMu.Lock()
		busy := c.favRunning
		c.favMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// patchFavorite is the in-memory mirror of "path's favorite flag flipped to
// newVal" — it flips Favorite on the matching c.entries element and adjusts the
// cached FavoritesView count by ±1 without a full refresh. Mirrors
// patchLocalDeletion's clone-swap discipline: c.entries gets a fresh backing
// array (Snapshot hands the slice header out and other goroutines may still be
// reading the old one) and dirCounts is clone-swapped (the sidebar reads the
// published map lock-free every frame, so mutating it in place would race that
// reader). Wakes the UI.
func (c *Controller) patchFavorite(path string, newVal bool) {
	c.mu.Lock()
	// Flip Favorite on the matching displayed entry so its star repaints
	// without a ListDir round-trip. New backing array — Snapshot returns
	// c.entries directly and callers may still be reading the old slice header.
	for idx := range c.entries {
		if c.entries[idx].Path == path {
			if c.entries[idx].Favorite != newVal {
				next := make([]cache.Entry, len(c.entries))
				copy(next, c.entries)
				next[idx].Favorite = newVal
				c.entries = next
			}
			break
		}
	}
	// The DB flip changed the favorites total by exactly ±1; mirror that delta
	// into the sidebar's cached count. Clone-swap rather than mutate in place —
	// see the c.entries note above and bumpTrashCount.
	if c.dirCounts != nil {
		next := maps.Clone(c.dirCounts)
		if newVal {
			next[FavoritesView]++
		} else {
			next[FavoritesView]--
			if next[FavoritesView] < 0 {
				next[FavoritesView] = 0
			}
		}
		c.dirCounts = next
		c.dirCountsVer++
	}
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}
}
