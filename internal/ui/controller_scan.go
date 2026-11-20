package ui

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

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

// IndexStatus is a snapshot of the indexing pipeline state. No UI reads it
// since the index-info modal was removed; tests use it to observe scan,
// warm-up and surfaced-error state.
// Active/Target/StartedAt/EndedAt/Batched describe directory scans only; the
// thumbnail warm-up, which runs independently, is reported in WarmUp.
type IndexStatus struct {
	LibraryRoot string
	CacheDir    string
	DBPath      string
	Active      bool
	Target      string
	StartedAt   time.Time
	EndedAt     time.Time
	Batched     int
	ScanTotal   int // files the scan will visit (scan.Count); 0 while still counting
	LastError   string
	WarmUp      WarmUpStatus
}

// WarmUpStatus is the current/last thumbnail warm-up pass.
type WarmUpStatus struct {
	Active    bool
	StartedAt time.Time
	EndedAt   time.Time
	Done      int // entries checked so far
	Total     int // entries in the index when the pass started
}

// IndexStatus returns a snapshot of the current/last indexing run plus
// pointers to the on-disk state. Safe to call from any goroutine.
func (c *Controller) IndexStatus() IndexStatus {
	c.mu.Lock()
	st := IndexStatus{
		LibraryRoot: c.libraryRoot,
		CacheDir:    c.cacheDir,
		DBPath:      c.indexPath,
		Active:      c.scanning > 0,
		Target:      c.scanTarget,
		StartedAt:   c.scanStartedAt,
		EndedAt:     c.scanEndedAt,
		Batched:     c.scanBatched,
		ScanTotal:   c.scanTotal,
		LastError:   c.scanLastErr,
		WarmUp: WarmUpStatus{
			Active:    c.warmUpRunning,
			StartedAt: c.warmUpStartedAt,
			EndedAt:   c.warmUpEndedAt,
			Done:      c.warmUpDone,
			Total:     c.warmUpTotal,
		},
	}
	c.mu.Unlock()
	return st
}

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
	// (and the pause-gate release) only aborts this scan. Previously the row
	// called c.CancelScan, which also cancelled the separate Thumbnails
	// warm-up, and a stale row could cancel whatever scan was current.
	ctx, cancelThis := context.WithCancel(ctx)
	defer cancelThis()

	c.mu.Lock()
	c.scanning++
	c.scanTarget = dir
	c.scanStartedAt = time.Now()
	c.scanEndedAt = time.Time{}
	c.scanBatched = 0
	c.scanTotal = 0
	c.scanGen++
	gen := c.scanGen
	c.scanLastErr = ""
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}

	// Register with the process bar so the user can pause / resume /
	// cancel from the main screen. Cancellation is scoped to this scan's
	// own ctx (cancelThis), not c.CancelScan, so it can't take down the
	// Thumbnails warm-up or a newer scan that has since started.
	var proc *Process
	if c.processes != nil {
		proc = c.processes.Begin(ctx, ProcScan, "Indexing", cancelThis, true)
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

	// Count the files alongside the scan so the process bar and the info
	// modal can show "done / total (pct)" instead of a bare running count.
	// The count only reads directory entries, so it finishes long before the
	// scan's stat/ffprobe work; until then the bar stays indeterminate.
	go func() {
		n, err := scan.Count(ctx, dir)
		if err != nil {
			return // cancelled along with the scan
		}
		c.mu.Lock()
		if c.scanGen == gen {
			c.scanTotal = n
		}
		c.mu.Unlock()
		if proc != nil {
			proc.SetTotal(int64(n))
		}
	}()

	// Pass an index-backed duration lookup so videos that already have a
	// stored duration_ms skip the ffprobe fork on incremental rescans. The
	// index only trusts the stored duration when the file's size and mtime
	// still match the indexed version.
	results := scan.WalkWith(ctx, dir, scan.WalkOptions{
		KnownDurationMs: c.index.KnownDurationMs,
	})
	var batch []scan.Result
	flushAt := time.Now().Add(time.Second)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		n := len(batch)
		changed := c.index.ReconcileBatchChanged(batch)
		batch = batch[:0]
		flushAt = time.Now().Add(time.Second)
		c.mu.Lock()
		c.scanBatched += n
		c.mu.Unlock()
		if proc != nil {
			proc.AddDone(int64(n))
		}
		// An incremental rescan of an unchanged library reconciles every file
		// but changes no row (the upsert's WHERE guard skips them); refreshing
		// the grid then would re-run ListDir + the sidebar counts for nothing,
		// back to back for the whole scan (C4-01).
		if changed > 0 {
			c.scheduleRefresh(c.activeDir())
		}
	}
	for r := range results {
		if proc != nil {
			proc.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		batch = append(batch, r)
		if len(batch) >= 1000 || (len(batch) > 0 && time.Now().After(flushAt)) {
			flush()
		}
	}
	flush()
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
	c.warmUpStartedAt = time.Now()
	c.warmUpEndedAt = time.Time{}
	c.warmUpDone = 0
	c.warmUpTotal = 0
	c.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
	}

	go func() {
		defer cancel()
		var proc *Process
		if c.processes != nil {
			proc = c.processes.Begin(ctx, ProcWarmUp, "Thumbnails", cancel, true)
			proc.SetStatus("Generating thumbnails…")
		}
		defer func() {
			// A superseded pass (another WarmUp started since) leaves the
			// status alone: it now belongs to the newer pass.
			c.mu.Lock()
			if c.warmUpGen == myGen {
				c.warmUpRunning = false
				c.warmUpCancel = nil
				c.warmUpEndedAt = time.Now()
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
		c.mu.Lock()
		if c.warmUpGen == myGen {
			c.warmUpTotal = total
		}
		c.mu.Unlock()

		// Fan out across a worker pool. store.Path is internally bounded by
		// the CPU/external decode semaphores, but driving it from a single
		// goroutine only ever keeps one decode in flight, so the semaphores
		// sit mostly idle. Running WarmUpConcurrency workers lets both pools
		// saturate; the channel back-pressures the producer so we never hold
		// more than a small window of entries in memory.
		workers := max(c.store.WarmUpConcurrency(), 1)
		jobs := make(chan cache.Entry, workers*2)

		// proc.SetDone already goes through the registry's ~30Hz coalescer, so
		// no second throttle here (U4-09). Without a registry (tests) invalidate
		// directly. warmUpDone is published every 50 items for IndexStatus
		// and once more, exactly, after the pool drains.
		var done, generated int64
		notify := func(n int64) {
			if proc != nil {
				proc.SetDone(n)
			} else if c.invalidate != nil {
				c.invalidate()
			}
			if n%50 == 0 {
				c.mu.Lock()
				if c.warmUpGen == myGen {
					c.warmUpDone = int(n)
				}
				c.mu.Unlock()
			}
		}

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
						if _, err := c.store.Path(e); err == nil {
							atomic.AddInt64(&generated, 1)
						}
					}
					notify(atomic.AddInt64(&done, 1))
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
		// Concurrent workers can publish their n out of order and a cancel can
		// stop at any count, so the final value is stored unconditionally
		// (U4-09: it used to be stored only when n%50==0 or n==total, so a
		// cancelled pass could show a count up to 49 too low forever).
		final := atomic.LoadInt64(&done)
		if proc != nil {
			proc.SetDone(final)
		}
		c.mu.Lock()
		if c.warmUpGen == myGen {
			c.warmUpDone = int(final)
		}
		c.mu.Unlock()

		if proc != nil {
			n := atomic.LoadInt64(&generated)
			proc.SetStatus(fmt.Sprintf("Generated %d new thumbnail(s), skipped %d existing",
				n, final-n))
		}
	}()
}
