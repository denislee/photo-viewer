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
	TotalRows   int
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
		LastError:   c.scanLastErr,
		WarmUp: WarmUpStatus{
			Active:    c.warmUpRunning,
			StartedAt: c.warmUpStartedAt,
			EndedAt:   c.warmUpEndedAt,
			Done:      c.warmUpDone,
			Total:     c.warmUpTotal,
		},
	}
	idx := c.index
	c.mu.Unlock()
	if idx != nil {
		st.TotalRows = c.cachedTotalRows(idx, st.LibraryRoot)
	}
	return st
}

// indexStatusTTL bounds how stale the index-info modal's row count may be. A
// few hundred ms is imperceptible for a status readout but collapses a
// per-frame COUNT storm into at most one COUNT per interval.
const indexStatusTTL = 400 * time.Millisecond

// cachedTotalRows returns the index row count under st.LibraryRoot, recomputing
// the expensive CountDir COUNT at most once per indexStatusTTL. Called only
// from IndexStatus (the info modal); the cache is invalidated implicitly when
// the library root changes.
func (c *Controller) cachedTotalRows(idx *cache.Index, root string) int {
	c.idxStatusMu.Lock()
	defer c.idxStatusMu.Unlock()
	now := time.Now()
	if root == c.idxStatusRoot && now.Before(c.idxStatusExpiry) {
		return c.idxStatusRows
	}
	n := idx.CountDir(root)
	c.idxStatusRows = n
	c.idxStatusRoot = root
	c.idxStatusExpiry = now.Add(indexStatusTTL)
	return n
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
	for _, sub := range []string{"hls", "display"} {
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
		if len(batch) >= 1000 || (len(batch) > 0 && time.Now().After(flushAt)) {
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
			proc = c.processes.Begin(ProcWarmUp, "Thumbnails", cancel, true)
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

		// Throttle the redraw notifications so a burst of fast thumb decodes
		// doesn't pile dozens of frame requests onto Gio's loop. The progress
		// bar still ticks at full granularity via proc.SetDone.
		var done, generated int64
		var invMu sync.Mutex
		var lastInv time.Time
		notify := func(n int64) {
			if proc != nil {
				proc.SetDone(n)
			}
			if n%50 == 0 || n == int64(total) {
				c.mu.Lock()
				if c.warmUpGen == myGen {
					c.warmUpDone = int(n)
				}
				c.mu.Unlock()
			}
			if c.invalidate != nil {
				invMu.Lock()
				if time.Since(lastInv) >= 33*time.Millisecond {
					lastInv = time.Now()
					invMu.Unlock()
					c.invalidate()
				} else {
					invMu.Unlock()
				}
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

		if proc != nil {
			n := atomic.LoadInt64(&generated)
			proc.SetStatus(fmt.Sprintf("Generated %d new thumbnail(s), skipped %d existing",
				n, atomic.LoadInt64(&done)-n))
		}
	}()
}
