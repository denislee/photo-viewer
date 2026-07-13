package ui

import (
	"strconv"
	"sync/atomic"
	"testing"
)

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
	v.proc = proc
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

	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.logBuf) != 500 {
		t.Fatalf("logBuf grew to %d lines, want it capped at 500", len(v.logBuf))
	}
	// The cap keeps the tail (newest lines), dropping the oldest.
	if first, want := v.logBuf[0], "line "+strconv.Itoa(n-500); first != want {
		t.Errorf("oldest retained line = %q, want %q", first, want)
	}
	if last, want := v.logBuf[len(v.logBuf)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest line = %q, want %q", last, want)
	}
}
