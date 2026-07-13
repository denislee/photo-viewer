package face

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// Tunables for the worker pool. See Start (maxWorkers), process (detectTimeout)
// and worker (maxDaemonRespawns) for how each is used.
const (
	// maxWorkers caps the number of concurrent daemons. Each pv-face-detect
	// --server child loads dlib + numpy (~300–500 MB RSS), so on a 16-core box
	// one-per-CPU would burn 5–8 GB and thrash OpenMP threads. Detection is
	// coarse-grained (a separate process per call) so 4 in flight already keeps
	// the disk/CPU busy without the memory blow-up.
	maxWorkers = 4
	// detectTimeout bounds a single Detect call. A HOG detect on a thumbnail is
	// tens of milliseconds; 60s is a generous ceiling that still guarantees a
	// wedged helper unblocks shutdown on its own even before the parent ctx is
	// cancelled — this is what breaks the old Stop deadlock.
	detectTimeout = 60 * time.Second
	// maxDaemonRespawns caps how many times a worker will restart a crashing
	// helper before giving up, so a permanently-broken install (e.g. a dlib
	// segfault on every thumb) degrades loudly instead of spin-restarting.
	maxDaemonRespawns = 3
)

// Job is one unit of face work.
type Job struct {
	Entry     cache.Entry
	ThumbPath string
	ThumbMod  int64 // mtime of the thumbnail used for invalidation
}

// OnClusterChange is called when a new cluster is created or an existing one
// grows. The UI uses it to refresh the People list.
type OnClusterChange func()

// Pipeline owns a worker pool that detects faces and writes them into the
// index. The pool is started once via Start and fed via Submit; Stop cancels
// in-flight work and waits for workers to finish.
type Pipeline struct {
	idx      *cache.Index
	jobs     chan Job
	quit     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	cancel   context.CancelFunc
	onChange OnClusterChange
	// enabled is read lock-free by acceptJob on every Submit while Recheck can
	// flip it from another goroutine, so it's atomic rather than mu-guarded.
	enabled atomic.Bool
	// liveWorkers counts workers currently able to consume jobs. Start seeds it
	// with the worker count; each worker decrements it on exit (daemon spawn
	// failure, repeated crashes, or Stop). acceptJob refuses new work once it
	// hits zero so SubmitBlocking can't park forever on a channel that nothing
	// will ever read — the failure mode when every worker's spawnDaemon failed.
	liveWorkers atomic.Int32

	// clusterMu guards the in-memory cluster cache and serialises the
	// assignment step across workers. Detection runs in parallel; only the
	// short cluster-search/update is serialised so two workers can't both
	// invent a new cluster for the same face.
	clusterMu      sync.Mutex
	cachedClusters []cache.Cluster
	clusterCacheOK bool

	// freshnessMu guards freshness, an in-memory mirror of the latest
	// thumb_mtime stored per-path in the faces table. acceptJob consults
	// it instead of running a COUNT(*) per Submit. Populated lazily on
	// first need; updated by process() after it successfully writes faces.
	freshnessMu sync.RWMutex
	freshness   map[string]int64
}

// NewPipeline returns a pipeline. If pv-face-detect is missing, the pipeline
// is disabled — Submit becomes a no-op and Start returns immediately. This
// matches the graceful-degrade behaviour of the thumbnail backends.
func NewPipeline(idx *cache.Index, onChange OnClusterChange) *Pipeline {
	p := &Pipeline{
		idx:      idx,
		onChange: onChange,
	}
	p.enabled.Store(Available())
	return p
}

// Enabled reports whether the helper binary was found on construction.
func (p *Pipeline) Enabled() bool {
	return p.enabled.Load()
}

// Recheck re-probes the helper and updates Enabled. Returns the fresh
// status. Caller is responsible for calling Start afterwards if the state
// flipped from off → on; this function never starts or stops workers on
// its own, to avoid surprising the caller.
func (p *Pipeline) Recheck() Status {
	// refreshProbe (not cachedProbe) so a newly-installed or newly-broken helper
	// is actually re-probed and the memoised result is refreshed for the next
	// Available()/NewPipeline hot-path caller.
	s := refreshProbe()
	p.enabled.Store(s.Working)
	return s
}

// Start spins up workers. Calling Start more than once is a no-op.
func (p *Pipeline) Start(parent context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil || !p.enabled.Load() {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	// Detection is CPU-bound (HOG + dlib feature extraction) with a separate
	// process per call, so one-per-CPU is the natural ceiling; maxWorkers then
	// caps the daemon count so we don't spawn a dlib child per core on big
	// machines. Floor of 2 so even a single-core box overlaps I/O and compute.
	n := min(max(runtime.NumCPU(), 2), maxWorkers)
	jobs := make(chan Job, n*4)
	quit := make(chan struct{})
	p.jobs = jobs
	p.quit = quit
	p.liveWorkers.Store(int32(n))
	// Pass the channels in rather than reading the fields: Stop nils the fields
	// to stop accepting new work, but a worker must keep draining the exact
	// channel it started on.
	for range n {
		p.wg.Add(1)
		go p.worker(ctx, jobs, quit)
	}
}

// Stop asks the workers to drain what's already queued and exit, then waits for
// them. Use this for graceful drain at end of run (e.g. pv-scan completion).
// Safe to call multiple times.
//
// Shutdown is signalled by closing the private quit channel, NOT the jobs
// channel. Closing jobs would race any in-flight Submit/SubmitBlocking that has
// already passed acceptJob but not yet sent, panicking the whole process; the
// jobs channel is therefore never closed. Workers observe quit, best-effort
// drain the buffer, and return; the parent ctx is cancelled only afterwards so
// the drain itself isn't interrupted.
func (p *Pipeline) Stop() {
	p.mu.Lock()
	quit := p.quit
	p.quit = nil
	p.jobs = nil // reject new submissions (acceptJob sees nil)
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if quit == nil {
		return // never started, or already stopped
	}
	close(quit)
	p.wg.Wait()
	// Release the parent ctx after workers have drained so callers can re-Start
	// and any leftover subprocess is reaped.
	if cancel != nil {
		cancel()
	}
}

// Submit enqueues an entry for face detection. Non-photo types and entries
// that already have fresh face rows are skipped cheaply. Never blocks: if the
// queue is full the job is dropped (better than holding up the scan flush).
func (p *Pipeline) Submit(j Job) {
	jobs, ok := p.acceptJob(j)
	if !ok {
		return
	}
	select {
	case jobs <- j:
	default:
		// queue full; drop. The next directory open will re-submit.
	}
}

// SubmitBlocking is the variant headless callers (pv-scan) use: it blocks
// until the job is accepted by a worker. Returns false if the pipeline is
// stopped or the entry isn't a face candidate, so callers can stop early.
func (p *Pipeline) SubmitBlocking(ctx context.Context, j Job) bool {
	jobs, ok := p.acceptJob(j)
	if !ok {
		return false
	}
	// Re-poll liveWorkers while parked. acceptJob's check can go stale in a
	// narrow window: every worker can exit (all daemons crash past the respawn
	// cap) after we're already blocked on a full buffer, and the caller's ctx
	// may never cancel. The ticker guarantees we notice a drained pool and bail
	// instead of blocking forever. In the common case the send wins immediately
	// and the ticker never fires.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case jobs <- j:
			return true
		case <-ticker.C:
			if p.liveWorkers.Load() <= 0 {
				return false
			}
		}
	}
}

// acceptJob runs the cheap pre-checks and returns the live jobs channel.
func (p *Pipeline) acceptJob(j Job) (chan Job, bool) {
	if !p.enabled.Load() {
		return nil, false
	}
	switch j.Entry.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
	default:
		return nil, false
	}
	if p.hasFreshCached(j.Entry.Path, j.ThumbMod) {
		return nil, false
	}
	p.mu.Lock()
	jobs := p.jobs
	p.mu.Unlock()
	if jobs == nil {
		return nil, false
	}
	// No live workers means nothing will ever drain the channel; refuse so a
	// blocking SubmitBlocking returns instead of parking forever (the failure
	// mode when every worker's spawnDaemon failed).
	if p.liveWorkers.Load() <= 0 {
		return nil, false
	}
	return jobs, true
}

// hasFreshCached answers the freshness question from the in-memory mirror.
// On first call it lazily bulk-loads from the index — that single query
// replaces what would otherwise be one COUNT(*) per Submit on every scan.
func (p *Pipeline) hasFreshCached(path string, thumbMod int64) bool {
	p.freshnessMu.RLock()
	m := p.freshness
	p.freshnessMu.RUnlock()
	if m == nil {
		p.freshnessMu.Lock()
		if p.freshness == nil {
			p.freshness = p.idx.LoadFaceFreshness()
		}
		m = p.freshness
		p.freshnessMu.Unlock()
	}
	p.freshnessMu.RLock()
	defer p.freshnessMu.RUnlock()
	return p.freshness[path] == thumbMod
}

// markFresh records that path now has faces written against thumbMod, so the
// next Submit/process for the same (path, thumbMod) skips re-detection.
func (p *Pipeline) markFresh(path string, thumbMod int64) {
	p.freshnessMu.Lock()
	if p.freshness == nil {
		p.freshness = map[string]int64{}
	}
	p.freshness[path] = thumbMod
	p.freshnessMu.Unlock()
}

func (p *Pipeline) worker(ctx context.Context, jobs <-chan Job, quit <-chan struct{}) {
	defer p.wg.Done()
	defer p.liveWorkers.Add(-1)

	d, err := spawnDaemon()
	if err != nil {
		log.Printf("face: spawn helper: %v", err)
		return
	}
	// d is reassigned on respawn; the deferred close nils it out on the paths
	// that already closed it so we never double-close (double Wait errors).
	defer func() {
		if d != nil {
			d.Close()
		}
	}()

	respawns := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-quit:
			// Graceful stop: process whatever is already buffered, then exit.
			p.drain(ctx, d, jobs)
			return
		case j, ok := <-jobs:
			if !ok {
				return // defensive: jobs is never closed
			}
			if p.process(ctx, d, j) {
				continue
			}
			// Transport error: the helper died mid-request (a dlib segfault on
			// a corrupt thumb is the classic trigger). Reap it and spawn a
			// fresh one so this worker doesn't become a black-hole consumer
			// that silently eats every subsequent job. Cap respawns so a
			// permanently-broken helper degrades loudly instead of spinning.
			d.Close()
			d = nil
			if ctx.Err() != nil {
				return
			}
			respawns++
			if respawns > maxDaemonRespawns {
				log.Printf("face: helper crashed %d times; stopping this worker", respawns)
				return
			}
			nd, err := spawnDaemon()
			if err != nil {
				log.Printf("face: respawn helper: %v", err)
				return
			}
			d = nd
		}
	}
}

// drain best-effort processes jobs already sitting in the buffer at Stop time
// so a graceful Stop doesn't discard queued work. It never waits for new
// submissions (the default case returns the instant the buffer is empty) and
// bails if the helper dies mid-drain — completeness here is best-effort since
// the next scan re-detects anything skipped.
func (p *Pipeline) drain(ctx context.Context, d *daemon, jobs <-chan Job) {
	for {
		select {
		case j, ok := <-jobs:
			if !ok || !p.process(ctx, d, j) {
				return
			}
		default:
			return
		}
	}
}

// process handles one job: detect faces on the thumbnail, then cluster and
// persist them. It returns false only when the daemon died mid-request (a
// transport error), signalling the worker to respawn it; a per-image skip, a
// successful write, or an already-fresh path all return true (daemon healthy).
func (p *Pipeline) process(ctx context.Context, d *daemon, j Job) (daemonOK bool) {
	// Re-check inside the worker — another worker may have handled this
	// path in the interim (e.g. a re-submit from a refresh).
	if p.hasFreshCached(j.Entry.Path, j.ThumbMod) {
		return true
	}
	// Bound each detection so a wedged helper can't block Stop indefinitely.
	// On timeout Detect kills the child, so the daemon is dead and we fall
	// through to the transport-error path (respawn).
	detectCtx, cancel := context.WithTimeout(ctx, detectTimeout)
	dets, err := d.Detect(detectCtx, j.ThumbPath)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("face: detect %s: %v", j.Entry.Path, err)
		}
		// A per-image error means the daemon is healthy and just couldn't
		// handle this one file — skip it and keep the daemon. Anything else
		// (pipe write/read, timeout-kill, malformed line) means the child is
		// dead or the stream is out of sync, so the daemon must be respawned.
		var pie *perImageError
		return errors.As(err, &pie)
	}

	// The assignment step + DB write happen under clusterMu, released via defer
	// inside the helper so a panic there (WriteFacesForPath, or a fix-up index
	// bug) can't escape holding the lock and wedge every other worker at Lock().
	// onChange must stay OUTSIDE the lock, so the helper only reports whether a
	// new cluster was created and the caller fires the callback unlocked.
	changed := p.assignAndPersist(j, dets)
	if changed && p.onChange != nil {
		p.onChange()
	}
	return true
}

// assignAndPersist greedily assigns the detected faces to clusters, persists
// them with WriteFacesForPath, and fixes up the in-memory cache with the new
// cluster IDs — all under clusterMu, which is released via defer so any panic
// in the DB write or the fix-up loop still frees the lock (otherwise every
// other worker would block forever at clusterMu.Lock and face processing would
// silently stop). It returns whether a new cluster was created, so the caller
// can fire onChange outside the lock.
func (p *Pipeline) assignAndPersist(j Job, dets []Detection) (changed bool) {
	p.clusterMu.Lock()
	defer p.clusterMu.Unlock()

	if !p.clusterCacheOK {
		p.cachedClusters = p.idx.AllClusters()
		p.clusterCacheOK = true
	}

	// Build the running candidate list once and mutate it in lockstep with
	// the cache — earlier faces in the same image must influence later ones.
	cands := make([]CentroidCandidate, 0, len(p.cachedClusters)+len(dets))
	for _, c := range p.cachedClusters {
		cands = append(cands, CentroidCandidate{ID: c.ID, Centroid: c.Centroid, Count: c.Count})
	}

	ops := make([]cache.FaceOp, 0, len(dets))
	// cacheSlots[i] is the index into p.cachedClusters of the cluster chosen
	// for ops[i]. Used to fix up new-cluster IDs after the DB commit.
	cacheSlots := make([]int, 0, len(dets))

	for _, d := range dets {
		if len(d.Embedding) == 0 {
			continue
		}
		f := cache.Face{
			Path:       j.Entry.Path,
			ThumbMtime: j.ThumbMod,
			BBox:       d.BBox,
			Embedding:  d.Embedding,
		}
		idx, _ := NearestCluster(d.Embedding, cands)
		if idx >= 0 {
			c := p.cachedClusters[idx]
			newCentroid := UpdatedCentroid(c.Centroid, d.Embedding, c.Count)
			ops = append(ops, cache.FaceOp{
				Face:              f,
				ExistingClusterID: c.ID,
				UpdatedCentroid:   newCentroid,
			})
			p.cachedClusters[idx].Centroid = newCentroid
			p.cachedClusters[idx].Count = c.Count + 1
			// Keep the candidate centroid in lockstep so a later face in the
			// same image compares against the just-updated cluster.
			cands[idx].Centroid = newCentroid
			cands[idx].Count = c.Count + 1
			cacheSlots = append(cacheSlots, idx)
		} else {
			emb := append([]float32(nil), d.Embedding...)
			ops = append(ops, cache.FaceOp{
				Face:               f,
				NewClusterCentroid: emb,
			})
			p.cachedClusters = append(p.cachedClusters, cache.Cluster{
				Centroid: emb,
				Count:    1,
			})
			cands = append(cands, CentroidCandidate{Centroid: emb, Count: 1})
			cacheSlots = append(cacheSlots, len(p.cachedClusters)-1)
		}
	}

	results, err := p.idx.WriteFacesForPath(j.Entry.Path, ops)
	if err != nil {
		log.Printf("face: write %s: %v", j.Entry.Path, err)
		// Cache may be inconsistent with DB now; drop it so the next
		// assignment reloads from authoritative state. This is a DB error,
		// not a helper transport error, so the daemon stays healthy.
		p.cachedClusters = nil
		p.clusterCacheOK = false
		return false
	}
	p.markFresh(j.Entry.Path, j.ThumbMod)

	for i, op := range ops {
		if op.ExistingClusterID == 0 {
			p.cachedClusters[cacheSlots[i]].ID = results[i].ClusterID
			changed = true
		}
	}
	return changed
}

// InvalidateClusters drops the in-memory cluster cache. UI paths that mutate
// clusters out-of-band (merge, wipe) must call this so the next assign reloads
// from the index. Rename leaves centroid+count unchanged and does not need it.
func (p *Pipeline) InvalidateClusters() {
	p.clusterMu.Lock()
	p.cachedClusters = nil
	p.clusterCacheOK = false
	p.clusterMu.Unlock()
	// Cluster wipes and merges go hand in hand with the face rows being
	// rewritten, so the freshness mirror is no longer authoritative.
	p.freshnessMu.Lock()
	p.freshness = nil
	p.freshnessMu.Unlock()
}
