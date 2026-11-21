package ui

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// Rebuild empties the index in place, forgets every generated thumbnail, and
// kicks off a full rescan of the library root. The active grid view is
// preserved — the rescan surfaces only as a process-bar entry, and entries
// are repopulated as scan batches land via the usual refresh path.
func (c *Controller) Rebuild() error {
	c.mu.Lock()
	if c.scanCancel != nil {
		c.scanCancel()
	}
	// Rebuild removes the thumbs an in-flight warm-up would be writing to, so
	// cancel the warm-up as well — otherwise it races the removal below.
	if c.warmUpCancel != nil {
		c.warmUpCancel()
		c.warmUpCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.scanCancel = cancel
	idx := c.index
	c.mu.Unlock()

	// Reset the index in place instead of deleting and reopening the db.
	// Keeping the same *cache.Index handle means the duplicates view, fuzzy
	// search, and webserver — which captured the pointer at startup — keep
	// querying the live database after a rebuild, there is no orphaned WAL to
	// replay into a freshly recreated file, and c.index is never reassigned
	// (so the lock-free readers of it no longer race). cache.IndexPath's
	// read-only-root handling is irrelevant here since we touch no db path.
	if err := idx.Clear(); err != nil {
		return err
	}
	// Forget every generated thumbnail so they regenerate from the rescanned
	// originals. Thumbs live under cacheDir (always writable), so this works
	// even when the library root itself is read-only. This is a hard failure:
	// the grid renders straight from these thumbs, so pressing on with stale
	// ones would show wrong images for re-derived originals — abort so the
	// caller can surface it, with the index already cleared but no half-wiped
	// thumb store served.
	if err := os.RemoveAll(filepath.Join(c.cacheDir, "thumbs")); err != nil {
		return err
	}
	// Drop the on-the-fly HLS transcode cache and the larger browser-viewer
	// renditions (W-03) too. Both are keyed by thumb id + source mtime, so a
	// rebuild that re-derives them from changed originals must not keep serving
	// stale segments/images; this also reclaims the whole trees (including any
	// crash-orphaned .tmp files) that would otherwise grow unbounded on the
	// media drive. Unlike the index and thumbs above, these are webserver-only
	// caches whose per-request staleness check already guards correctness, so a
	// wipe failure here is best-effort: log and press on to the rescan rather
	// than aborting with a cleared index and no rebuild in flight. Each dir may
	// also simply not exist if the webserver never ran (RemoveAll no-ops then).
	for _, sub := range []string{"hls", cache.DisplayDir, cache.LargeThumbDir} {
		if err := os.RemoveAll(filepath.Join(c.cacheDir, sub)); err != nil {
			log.Printf("rebuild: clear %s cache: %v", sub, err)
		}
	}
	go func() {
		// scanInto blocks until the rescan's result channel drains and its final
		// ReconcileBatch has committed, so this returns with the index fully
		// repopulated. Only then run the opportunistic VACUUM (C-05): Clear above
		// moved the old library's pages to the freelist but never handed them back
		// to the filesystem, so the `.photo-viewer.db` next to the media keeps its
		// old high-water mark until something rewrites the file. VACUUM is done
		// here — after a full rebuild, off the UI goroutine, once the scan writes
		// are finished — rather than inside Clear (which would add its cost to
		// rebuild latency) or after an incremental SelectDir scan (nothing was
		// purged there). Skip it when this scan was cancelled: a newer scan/rebuild
		// has taken over as the writer, so vacuuming now would only contend with
		// it, and that newer run will do its own housekeeping if it's a rebuild.
		// Best-effort: log on failure, never block or crash the app.
		c.scanInto(ctx, c.libraryRoot)
		if ctx.Err() != nil {
			return
		}
		if err := c.index.Vacuum(); err != nil {
			log.Printf("rebuild: vacuum: %v", err)
		}
	}()
	return nil
}

func (c *Controller) scanInto(ctx context.Context, dir string) {
	// Give this scan its own cancellable context so the process bar's Cancel
	// (and the pause-gate release) only aborts this scan: it can't take down
	// the Thumbnails warm-up or a newer scan that has since started (U5-03).
	ctx, cancelThis := context.WithCancel(ctx)
	defer cancelThis()

	// Register with the process bar so the user can pause / resume /
	// cancel from the main screen.
	var proc *Process
	if c.processes != nil {
		proc = c.processes.Begin(ctx, ProcScan, "Indexing", cancelThis, true)
		proc.SetStatus("Indexing " + filepath.Base(dir))
	}

	defer func() {
		if proc != nil {
			proc.End()
		}
		if c.invalidate != nil {
			c.invalidate()
		}
	}()

	// Count the files alongside the scan so the process bar can show
	// "done / total (pct)" instead of a bare running count. The count only
	// reads directory entries, so it finishes long before the scan's
	// stat/ffprobe work; until then the bar stays indeterminate.
	if proc != nil {
		go func() {
			if n, err := scan.Count(ctx, dir); err == nil { // err: cancelled with the scan
				proc.SetTotal(int64(n))
			}
		}()
	}

	// Pass an index-backed duration lookup so videos that already have a
	// stored duration_ms skip the ffprobe fork on incremental rescans. The
	// index only trusts the stored duration when the file's size and mtime
	// still match the indexed version.
	results := scan.WalkWith(ctx, dir, scan.WalkOptions{
		KnownDurationMs: c.index.KnownDurationMs,
	})
	var batch []scan.Result
	flushAt := time.Now().Add(time.Second)
	// refreshPending records that some batch since the last scheduled refresh
	// changed rows. Refreshes are throttled: during a first scan the index is
	// growing fast and each refresh re-runs the grid listing plus the sidebar
	// counts, so issuing one per 1s flush lets the refresh work saturate the
	// frame loop. Spacing them by at least max(500ms, 2x the last refresh's own
	// duration) caps grid refreshes at ~2/s, or half the duty cycle when a
	// refresh is slow (U5-04).
	var refreshPending bool
	var lastSched time.Time
	flush := func(final bool) {
		if len(batch) > 0 {
			n := len(batch)
			changed := c.index.ReconcileBatchChanged(batch)
			batch = batch[:0]
			flushAt = time.Now().Add(time.Second)
			if proc != nil {
				proc.AddDone(int64(n))
			}
			// An incremental rescan of an unchanged library reconciles every
			// file but changes no row (the upsert's WHERE guard skips them);
			// refreshing the grid then would re-run ListDir + the sidebar
			// counts for nothing, back to back for the whole scan (C4-01).
			if changed > 0 {
				refreshPending = true
			}
		}
		if refreshPending && (final || time.Since(lastSched) >= max(500*time.Millisecond, 2*time.Duration(c.lastRefreshNanos.Load()))) {
			// The scan inserts rows, so the sidebar counts are stale: ask for a
			// recount, not just a grid re-query.
			c.scheduleRefreshRecount(c.activeDir())
			lastSched = time.Now()
			refreshPending = false
		}
	}
	for r := range results {
		if proc != nil {
			proc.Wait()
		}
		if ctx.Err() != nil {
			// A superseded scan: the last scheduled refresh may be behind its
			// throttle window, but its changes were committed, so fire one last
			// recount rather than leave them invisible.
			if refreshPending {
				c.scheduleRefreshRecount(c.activeDir())
			}
			return
		}
		batch = append(batch, r)
		if len(batch) >= 1000 || (len(batch) > 0 && time.Now().After(flushAt)) {
			flush(false)
		}
	}
	flush(true)
}

// WarmUp iterates over every entry in the index and ensures its thumbnail
// is generated. It runs in the background under its own cancel context, so
// switching directories or running an incremental scan does not abort it —
// only its process-bar Cancel, a Rebuild or another WarmUp invocation stops
// it.
func (c *Controller) WarmUp() {
	c.mu.Lock()
	if c.warmUpCancel != nil {
		c.warmUpCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.warmUpCancel = cancel
	c.warmUpGen++
	myGen := c.warmUpGen
	c.mu.Unlock()

	go func() {
		defer cancel()
		var proc *Process
		if c.processes != nil {
			proc = c.processes.Begin(ctx, ProcWarmUp, "Thumbnails", cancel, true)
			proc.SetStatus("Generating thumbnails…")
		}
		defer func() {
			// A superseded pass (another WarmUp started since) leaves
			// warmUpCancel alone: it now belongs to the newer pass.
			c.mu.Lock()
			if c.warmUpGen == myGen {
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

		if proc != nil {
			proc.SetTotal(int64(c.index.Count()))
		}

		// Fan out across a worker pool. store.Path is internally bounded by
		// the CPU/external decode semaphores, but driving it from a single
		// goroutine only ever keeps one decode in flight, so the semaphores
		// sit mostly idle. Running WarmUpConcurrency workers lets both pools
		// saturate; the channel back-pressures the producer so we never hold
		// more than a small window of entries in memory.
		workers := max(c.store.WarmUpConcurrency(), 1)
		jobs := make(chan cache.Entry, workers*2)

		// proc.SetDone already goes through the registry's ~30Hz coalescer, so
		// no second throttle here (U4-09).
		var done atomic.Int64

		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for e := range jobs {
					if proc != nil {
						proc.Wait()
					}
					// Skip thumbnails that already exist and are fresh: a
					// single stat, no decode and no singleflight. On a warm
					// cache this is almost every entry, so it is what makes a
					// re-run of the warm-up cheap.
					if ctx.Err() == nil && !c.store.Has(e) {
						_, _ = c.store.Path(e)
					}
					n := done.Add(1)
					if proc != nil {
						proc.SetDone(n)
					}
				}
			})
		}

		c.index.ForEachEntry(func(e cache.Entry) bool {
			if ctx.Err() != nil {
				return false
			}
			jobs <- e
			return true
		})
		close(jobs)
		wg.Wait()
	}()
}
