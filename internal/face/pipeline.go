package face

import (
	"context"
	"log"
	"runtime"
	"sync"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
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
	wg       sync.WaitGroup
	mu       sync.Mutex
	cancel   context.CancelFunc
	onChange OnClusterChange
	enabled  bool

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
	return &Pipeline{
		idx:      idx,
		onChange: onChange,
		enabled:  Available(),
	}
}

// Enabled reports whether the helper binary was found on construction.
func (p *Pipeline) Enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled
}

// Recheck re-probes the helper and updates Enabled. Returns the fresh
// status. Caller is responsible for calling Start afterwards if the state
// flipped from off → on; this function never starts or stops workers on
// its own, to avoid surprising the caller.
func (p *Pipeline) Recheck() Status {
	s := Probe()
	p.mu.Lock()
	p.enabled = s.Working
	p.mu.Unlock()
	return s
}

// Start spins up workers. Calling Start more than once is a no-op.
func (p *Pipeline) Start(parent context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil || !p.enabled {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	// One worker per CPU is a fine ceiling: detection is CPU-bound (HOG,
	// dlib feature extraction) and a separate Python process per call,
	// so saturating beyond NumCPU just causes context-switch overhead.
	n := max(runtime.NumCPU(), 2)
	p.jobs = make(chan Job, n*4)
	for range n {
		p.wg.Add(1)
		go p.worker(ctx)
	}
}

// Stop closes the input queue and waits for already-queued jobs to finish.
// Use this for graceful drain at end of run (e.g. pv-scan completion).
// Safe to call multiple times.
func (p *Pipeline) Stop() {
	p.mu.Lock()
	jobs := p.jobs
	p.jobs = nil
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if jobs != nil {
		close(jobs)
	}
	p.wg.Wait()
	// Cancel only after workers have drained — we don't want to interrupt
	// the last subprocess. The cancel still releases the parent ctx so
	// callers can re-Start.
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
	select {
	case <-ctx.Done():
		return false
	case jobs <- j:
		return true
	}
}

// acceptJob runs the cheap pre-checks and returns the live jobs channel.
func (p *Pipeline) acceptJob(j Job) (chan Job, bool) {
	if !p.enabled {
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

func (p *Pipeline) worker(ctx context.Context) {
	defer p.wg.Done()
	d, err := spawnDaemon()
	if err != nil {
		log.Printf("face: spawn helper: %v", err)
		return
	}
	defer d.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			p.process(ctx, d, j)
		}
	}
}

func (p *Pipeline) process(ctx context.Context, d *daemon, j Job) {
	// Re-check inside the worker — another worker may have handled this
	// path in the interim (e.g. a re-submit from a refresh).
	if p.hasFreshCached(j.Entry.Path, j.ThumbMod) {
		return
	}
	dets, err := d.Detect(ctx, j.ThumbPath)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("face: detect %s: %v", j.Entry.Path, err)
		}
		return
	}

	p.clusterMu.Lock()
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
			cands[idx].Centroid = newCentroid
			cands[idx].Count = c.Count + 1
			// Centroid changed — drop the cached unit form so the next
			// NearestCluster call recomputes against the fresh vector.
			cands[idx].Norm = nil
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
		// assignment reloads from authoritative state.
		p.cachedClusters = nil
		p.clusterCacheOK = false
		p.clusterMu.Unlock()
		return
	}
	p.markFresh(j.Entry.Path, j.ThumbMod)

	changed := false
	for i, op := range ops {
		if op.ExistingClusterID == 0 {
			p.cachedClusters[cacheSlots[i]].ID = results[i].ClusterID
			changed = true
		}
	}
	p.clusterMu.Unlock()

	if changed && p.onChange != nil {
		p.onChange()
	}
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
