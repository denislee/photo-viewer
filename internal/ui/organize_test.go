package ui

import (
	"context"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestOrganizeCachesMediaDate guards U-14: the Organize scan probes each video's
// metadata date only once. The first scan persists it (SetProbedMediaDate); the
// second scan reads it back from the index (ProbedMediaDate) without re-forking
// the prober. We swap the package-level mediaDateProber for a counting stub and
// assert the count is stable across the second pass.
func TestOrganizeCachesMediaDate(t *testing.T) {
	dir := t.TempDir()
	idx, err := cache.Load(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	const nVideos = 5
	results := make([]scan.Result, 0, nVideos)
	for i := range nVideos {
		results = append(results, scan.Result{
			Path:    filepath.Join(dir, "2024-03-15", "clip"+strconv.Itoa(i)+".mp4"),
			Type:    scan.TypeVideo,
			Size:    int64(1000 + i),
			ModTime: time.Unix(1_600_000_000, 0),
		})
	}
	idx.ReconcileBatch(results)

	var probes atomic.Int64
	orig := mediaDateProber
	mediaDateProber = func(string) time.Time {
		probes.Add(1)
		return time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() { mediaDateProber = orig })

	// No ProcessRegistry wired: scanForMismatched runs its worker pool and drains
	// results synchronously, so calling it directly blocks until the pass ends.
	v := NewOrganizeView(func() {})

	v.scanForMismatched(context.Background(), idx, dir)
	if got := probes.Load(); got != nVideos {
		t.Fatalf("first pass probed %d videos, want %d (every row is a cache miss)", got, nVideos)
	}

	v.scanForMismatched(context.Background(), idx, dir)
	if got := probes.Load(); got != nVideos {
		t.Fatalf("second pass raised probe count to %d, want it stable at %d (all cache hits)", got, nVideos)
	}
}

// TestOrganizeBumpProgressRoutesThroughCoalescer guards U-11: organize's
// per-file wakeups (bumpProgress, appendLog) must route through the process
// registry's ~30Hz coalescer, never fire a direct v.invalidate() on top of the
// already-coalesced proc.AddDone. A regression here re-creates the I-10 storm
// (a redundant frame-loop wakeup per file) the coalescer was built to remove.
func TestOrganizeBumpProgressRoutesThroughCoalescer(t *testing.T) {
	var directInvalidates atomic.Int64 // v.invalidate — must NOT fire per file
	var registryInvalidates atomic.Int64

	v := NewOrganizeView(func() { directInvalidates.Add(1) })
	r := NewProcessRegistry(func() { registryInvalidates.Add(1) })
	v.SetProcessRegistry(r)

	proc := r.Begin(ProcOrganize, "test", nil, true)
	v.mu.Lock()
	v.scope.attachLocked(proc, nil)
	v.mu.Unlock()

	// Baseline: Begin's structural forceNotify already fired one registry
	// invalidate; none of them should be direct.
	if got := directInvalidates.Load(); got != 0 {
		t.Fatalf("Begin caused %d direct invalidates, want 0", got)
	}

	const burst = 2000
	for range burst {
		v.bumpProgress()
	}
	for range burst {
		v.appendLog("line")
	}

	// The tight burst finishes well within one throttle interval, so the
	// registry coalescer collapses it to a handful of wakeups...
	if got := registryInvalidates.Load(); int(got) >= burst {
		t.Fatalf("registry wakeups not coalesced: %d for %d per-file updates", got, 2*burst)
	}
	// ...and the direct invalidate path is never taken while a process is
	// active — that redundant per-file wakeup is exactly what U-11 removed.
	if got := directInvalidates.Load(); got != 0 {
		t.Fatalf("per-file paths fired %d direct invalidates while a process was active, want 0", got)
	}

	proc.End()
}

// TestOrganizeAppendLogCapsBuffer guards U-16.5: organize's appendLog must cap
// the pending logBuf at 500 lines (like import's), so a huge move pass whose
// modal is never laid out — drainLog never runs to flush logBuf into
// logVisible — can't grow logBuf without bound. It keeps the most recent 500
// lines and drops the oldest.
func TestOrganizeAppendLogCapsBuffer(t *testing.T) {
	v := NewOrganizeView(func() {})

	const n = 700
	for i := range n {
		v.appendLog("line " + strconv.Itoa(i))
	}

	v.log.mu.Lock()
	defer v.log.mu.Unlock()
	if len(v.log.pending) != 500 {
		t.Fatalf("logBuf grew to %d lines, want it capped at 500", len(v.log.pending))
	}
	// The cap keeps the tail (newest lines), dropping the oldest.
	if first, want := v.log.pending[0], "line "+strconv.Itoa(n-500); first != want {
		t.Errorf("oldest retained line = %q, want %q", first, want)
	}
	if last, want := v.log.pending[len(v.log.pending)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest line = %q, want %q", last, want)
	}
}
