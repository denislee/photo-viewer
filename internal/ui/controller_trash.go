package ui

import (
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// DeletePath soft-deletes path: drops it from the grid + sidebar counts
// in-memory (so the UI updates instantly even when the active directory
// holds 10k entries), then dispatches the actual rename-into-trash and the
// index DELETE to a background goroutine. The in-memory patch keeps the
// sidebar counts (root, parent, subdirs, favorites, trash) consistent
// without re-running the per-refresh COUNT queries.
//
// If the trash dir is on a different filesystem (EXDEV) the background
// path falls back to a plain os.Remove. EmptyTrash actually frees the
// disk space later.
//
// When path is itself already inside the trash dir, DeletePath restores it
// instead — on a goroutine through the same restoreBatch the multi-select
// path uses, so the sidecar read, MkdirAll, renames, stat and index write
// never run on the frame goroutine (U4-11). Callers patch their own view
// optimistically (as for delete); restoreBatch's refresh repaints from the
// index, which re-shows the entry if the restore failed.
func (c *Controller) DeletePath(path string) { c.DeletePaths([]string{path}) }

// DeletePaths soft-deletes every path in one batch: one in-memory patch +
// one index transaction + a single background goroutine handling all the
// renames. Matters for duplicate-removal and multi-select flows where the
// per-item refresh + per-item DB write would otherwise dominate.
func (c *Controller) DeletePaths(paths []string) {
	if len(paths) == 0 {
		return
	}
	// Split: trashed items take the restore branch, the rest the delete
	// branch. Both run their heavy I/O on a background goroutine and
	// collapse to a single index write + count adjust + refresh.
	var toDelete, toRestore []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if c.isInTrash(p) {
			toRestore = append(toRestore, p)
		} else {
			toDelete = append(toDelete, p)
		}
	}
	if len(toDelete) > 0 {
		allLocal := c.patchLocalDeletion(toDelete, c.trashDir != "")
		c.deletions.Go(func() { c.performDeletion(toDelete, !allLocal) })
	}
	if len(toRestore) > 0 {
		c.deletions.Go(func() { c.restoreBatch(toRestore) })
	}
}

// DrainDeletions waits up to timeout for every delete/restore goroutine that
// DeletePaths started, so their index writes land before the index is closed
// on exit. It reports whether they all finished in time.
func (c *Controller) DrainDeletions(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		c.deletions.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// restoreBatch moves a batch of trashed files back to their recorded original
// locations, then collapses the whole batch into one index write, one trash-
// count adjustment, and one refresh. Mirrors performDeletion: the slow per-item
// I/O (sidecar read + JSON parse, MkdirAll, rename, thumb rename, stat) runs
// here on its own goroutine so a 200-item multi-select restore doesn't block the
// Gio frame loop on 200 renames + 200 single-row DB transactions + 200 dirCounts
// clone-swaps + 200 refreshes. A per-item failure is logged and skipped, never
// aborting the batch. Never mutates widget state directly — the single recount
// request at the end repaints via the Controller's refresh path.
func (c *Controller) restoreBatch(paths []string) {
	results, restored, renamed := collectRestores(paths, c.store)
	for orig, dst := range renamed {
		if err := c.index.MoveTags(orig, dst); err != nil {
			log.Printf("trash: move face tags %s: %v", orig, err)
		}
	}
	if len(results) > 0 {
		c.index.ReconcileBatchChanged(results)
		// The restored files lost their face rows on delete; their durable
		// tags re-attach when the pass re-detects them.
		if c.facesRunning() {
			c.DetectFaces()
		}
	}
	if restored > 0 {
		c.bumpTrashCount(-restored)
	}
	c.scheduleRefreshRecount(c.activeDir())
}

// collectRestores performs the filesystem half of a trash restore for every
// path — moving each file back to its original location and gathering the
// scan.Results that need reconciling into the index. It touches neither the
// index nor the trash count, so it's a pure, headless-testable unit: the
// caller issues the single batched writes. restored counts every successful
// RestoreFromTrash (what the trash count must drop by); it can exceed
// len(results) if a restored file vanished before its post-restore Stat.
//
// renamed maps original path → restored path for files that came back under
// a new name ("x (restored 1).jpg"), so their face tags can be re-keyed.
func collectRestores(paths []string, store *cache.ThumbStore) (results []scan.Result, restored int, renamed map[string]string) {
	results = make([]scan.Result, 0, len(paths))
	for _, p := range paths {
		orig, _ := cache.TrashOriginalPath(p)
		dst, err := cache.RestoreFromTrash(p, store)
		if err != nil {
			log.Printf("trash: restore failed for %s: %v", p, err)
			continue
		}
		restored++
		if orig != "" && orig != dst {
			if renamed == nil {
				renamed = map[string]string{}
			}
			renamed[orig] = dst
		}
		info, sErr := os.Stat(dst)
		if sErr != nil {
			continue
		}
		results = append(results, scan.Result{
			Path:    dst,
			Type:    scan.DetectType(dst),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return results, restored, renamed
}

// patchLocalDeletion is the in-memory mirror of "these paths went away" —
// drops them from c.entries, decrements every recursive dirCounts key
// they live under, and (if intoTrash) bumps the cached trash count by
// the same number. Wakes the UI. The actual filesystem + DB work runs
// asynchronously in performDeletion; this function is what makes a
// 10k-entry directory's delete feel instant.
//
// It reports whether every path was in the live entry list. A path that
// wasn't (a Duplicates delete in another directory) decremented no count, so
// the caller must ask performDeletion for a recount (U6-B3).
func (c *Controller) patchLocalDeletion(paths []string, intoTrash bool) (allLocal bool) {
	if len(paths) == 0 {
		return true
	}
	deleted := make(map[string]bool, len(paths))
	for _, p := range paths {
		deleted[p] = true
	}

	c.mu.Lock()
	favCount := 0
	// present holds only the paths we actually removed from the live entry
	// list this call. Idempotency guard: a second delete of the same path
	// (e.g. clicking delete on a duplicates "phantom" that reappeared before
	// the async index DELETE landed) finds it already gone from c.entries, so
	// it lands in neither present nor favCount — and therefore can't
	// double-decrement dirCounts into a low/negative drift that only a
	// restart clears. Counts are only ever adjusted for real removals.
	present := make(map[string]bool, len(paths))
	if len(c.entries) > 0 {
		// New backing array — Snapshot returns c.entries directly and
		// callers may still be reading the old slice header.
		kept := make([]cache.Entry, 0, len(c.entries))
		for _, e := range c.entries {
			if deleted[e.Path] {
				present[e.Path] = true
				if e.Favorite {
					favCount++
				}
				continue
			}
			kept = append(kept, e)
		}
		c.entries = kept
	}
	// Clone-swap rather than mutate in place: the sidebar reads the map
	// returned by DirCountsWithVersion() lock-free every frame, so
	// decrementing the published map here would race that reader (fatal
	// error: concurrent map read and map write).
	c.swapCountsLocked(func(next map[string]int) {
		// Walk each *actually-removed* path up its ancestor chain and
		// decrement the matching dirCounts keys. Iterating present (not
		// deleted) is what makes the decrement idempotent. This is
		// O(present × depth) map lookups rather than O(dirCounts × deleted)
		// prefix comparisons — the old approach rescanned every sidebar dir
		// for every deleted file, which stalled the UI on large deletions in
		// deep trees.
		for p := range present {
			for cur := p; ; {
				if cur == "" || cur == FavoritesView || cur == TrashView {
					break
				}
				if _, ok := next[cur]; ok {
					next[cur]--
					next[cur] = max(next[cur], 0)
				}
				parent := filepath.Dir(cur)
				if parent == cur {
					break
				}
				cur = parent
			}
		}
		if favCount > 0 {
			next[FavoritesView] -= favCount
			next[FavoritesView] = max(next[FavoritesView], 0)
		}
	})
	allLocal = len(present) == len(paths)
	c.mu.Unlock()

	if intoTrash {
		c.bumpTrashCount(len(paths))
	}
	if c.invalidate != nil {
		c.invalidate()
	}
	return allLocal
}

// performDeletion does the slow I/O work: a single batch DB delete plus
// one rename per path. Runs on its own goroutine so a bulk delete of 100
// items doesn't block the UI thread on 100 syscalls. Order is rename
// first / DB delete second so an in-flight scan can't re-discover a path
// whose row was already removed.
//
// recount asks for a sidebar recount once the index write lands; the caller
// sets it when its local count patch could not cover every path. An index
// failure forces one too, so the next paint resurfaces the index's truth
// instead of phantom rows with broken cells.
func (c *Controller) performDeletion(paths []string, recount bool) {
	// failed collects the paths that must be unlinked: every path when there
	// is no trash dir, otherwise those whose rename into the trash failed
	// (e.g. EXDEV).
	failed := paths
	if c.trashDir != "" {
		failed = nil
		for _, p := range paths {
			dst, err := cache.MoveToTrash(p, c.trashDir)
			if err == nil {
				if c.store != nil {
					_ = c.store.Rename(cache.ThumbIDFor(p), cache.ThumbIDFor(dst))
				}
				continue
			}
			log.Printf("trash: rename failed for %s (%v); falling back to remove", p, err)
			failed = append(failed, p)
		}
		if len(failed) > 0 {
			// Undo the optimistic trash bump for the items that didn't
			// actually make it into the trash dir.
			c.bumpTrashCount(-len(failed))
		}
	}
	for _, p := range failed {
		if c.store != nil {
			c.store.Forget(cache.ThumbIDFor(p))
		}
		if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("delete: remove failed for %s: %v", p, rmErr)
		}
	}
	if err := c.index.RemoveEntries(paths); err != nil {
		log.Printf("delete: index cleanup failed for %d path(s): %v", len(paths), err)
		recount = true
	}
	if recount {
		c.scheduleRefreshRecount(c.activeDir())
	}
}

// isInTrash reports whether path lives under the trash dir. Used so a
// "delete" gesture inside the Trash view restores the item instead of trying
// to soft-delete it (which would be a no-op rename onto itself).
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

// TrashStats reports the number of items and total bytes currently in the
// trash. Both come from a single walk of the trash dir, which is fine because
// the settings modal opens rarely. The in-memory mirror still serves
// refreshFromIndex, which must not re-walk the trash dir on every delete.
func (c *Controller) TrashStats() (count int, bytes int64) {
	if c.trashDir != "" {
		count, bytes = cache.TrashStats(c.trashDir)
	} else {
		count = c.cachedTrashCount()
	}
	return count, bytes
}

// cachedTrashCount returns the in-memory mirror of the trash item count,
// seeding it from disk on the first read. Cheap on every subsequent call —
// the field is bumped by MoveToTrash / RestoreFromTrash / EmptyTrash.
func (c *Controller) cachedTrashCount() int {
	c.mu.Lock()
	if c.trashCountValid {
		n := c.trashCount
		c.mu.Unlock()
		return n
	}
	c.mu.Unlock()
	var n int
	if c.trashDir != "" {
		n, _ = cache.TrashStats(c.trashDir)
	}
	c.mu.Lock()
	if !c.trashCountValid {
		c.trashCount = n
		c.trashCountValid = true
	}
	n = c.trashCount
	c.mu.Unlock()
	return n
}

// swapCountsLocked clone-swaps the published dirCounts: fn mutates a copy,
// which then replaces the map and bumps dirCountsVer. The sidebar reads the
// published map lock-free every frame, so it must never be written in place
// (fatal concurrent map read/write). No-op when no counts are published yet.
// The caller must hold c.mu.
func (c *Controller) swapCountsLocked(fn func(next map[string]int)) {
	if c.dirCounts == nil {
		return
	}
	next := maps.Clone(c.dirCounts)
	fn(next)
	c.dirCounts = next
	c.dirCountsVer++
}

// bumpTrashCount adjusts the cached trash count by delta. Seeds the count
// from disk if it hasn't been loaded yet so the delta lands on a real value.
func (c *Controller) bumpTrashCount(delta int) {
	c.cachedTrashCount()
	c.mu.Lock()
	c.trashCount += delta
	c.trashCount = max(c.trashCount, 0)
	c.swapCountsLocked(func(next map[string]int) {
		next[TrashView] = c.trashCount
	})
	c.mu.Unlock()
}

// setTrashCount sets the cached count to n (used by EmptyTrash with 0).
func (c *Controller) setTrashCount(n int) {
	c.mu.Lock()
	c.trashCount = n
	c.trashCountValid = true
	c.swapCountsLocked(func(next map[string]int) {
		next[TrashView] = n
	})
	c.mu.Unlock()
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
		c.setTrashCount(0)
		// The trash view (if currently shown) needs a refresh so the now-
		// empty list reflects on screen, and the sidebar's counts changed.
		c.scheduleRefreshRecount(c.activeDir())
		if done != nil {
			done(count, bytes, err)
		}
		if c.invalidate != nil {
			c.invalidate()
		}
	}()
}
