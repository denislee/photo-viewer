package ui

import (
	"context"
	"errors"
	"os"
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

// TestOrganizeCollisionNeverOverwrites is the U-18 guard for the Organize
// move: a video whose target date folder already holds a same-named file lands
// on the next free _N name, the occupant stays intact, and applyMove is told
// the name actually used.
func TestOrganizeCollisionNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	want := time.Date(2024, 3, 5, 0, 0, 0, 0, time.Local)
	wrongDir := filepath.Join(root, "2023-01-01")
	rightDir := filepath.Join(root, want.Format("2006-01-02"))
	for _, d := range []string{wrongDir, rightDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	occupant := filepath.Join(rightDir, "clip.mov")
	if err := os.WriteFile(occupant, []byte("occupant"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(wrongDir, "clip.mov")
	if err := os.WriteFile(src, []byte("misfiled"), 0o644); err != nil {
		t.Fatal(err)
	}

	var movedTo atomic.Value
	v := &OrganizeView{
		mismatched: []MismatchedVideo{{Entry: cache.Entry{Path: src, Type: scan.TypeVideo}, ExpectedDate: want}},
		applyMove: func(oldPath, newPath string) error {
			movedTo.Store(newPath)
			return nil
		},
	}
	v.startOrganize(root)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		running := v.running
		v.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("organize did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	dest := filepath.Join(rightDir, "clip_1.mov")
	if got, _ := movedTo.Load().(string); got != dest {
		t.Errorf("applyMove newPath = %q, want %q", got, dest)
	}
	if got, _ := os.ReadFile(dest); string(got) != "misfiled" {
		t.Errorf("moved file content = %q", got)
	}
	if got, _ := os.ReadFile(occupant); string(got) != "occupant" {
		t.Errorf("occupant clobbered: %q", got)
	}
}

// TestOrganizeReportsFailures is the U-20 regression: a pass with failed moves
// or index updates used to end on a plain "Organization complete.", hiding the
// failures in the log. The final status now counts them.
func TestOrganizeReportsFailures(t *testing.T) {
	root := t.TempDir()
	want := time.Date(2024, 3, 5, 0, 0, 0, 0, time.Local)
	wrongDir := filepath.Join(root, "2023-01-01")
	if err := os.MkdirAll(wrongDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(wrongDir, "ok.mov")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(wrongDir, "gone.mov") // never created: the move fails

	v := &OrganizeView{
		mismatched: []MismatchedVideo{
			{Entry: cache.Entry{Path: gone, Type: scan.TypeVideo}, ExpectedDate: want},
			{Entry: cache.Entry{Path: ok, Type: scan.TypeVideo}, ExpectedDate: want},
		},
		applyMove: func(oldPath, newPath string) error { return errors.New("index busy") },
	}
	v.startOrganize(root)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		running, status := v.running, v.statusMsg
		v.mu.Unlock()
		if !running {
			const wantStatus = "Organization finished. Moved 1 • 1 error(s) • 1 warning(s) — see log."
			if status != wantStatus {
				t.Errorf("status = %q, want %q", status, wantStatus)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("organize did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := organizeSummary(3, 0, 0); got != "Organization complete. Moved 3." {
		t.Errorf("clean summary = %q", got)
	}
}
