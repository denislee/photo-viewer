package ui

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

// TestTaskLogAppendCap locks the pending-buffer bound: appending past the cap
// keeps the newest taskLogPendingCap lines and drops the oldest, so a pass whose
// modal is never laid out (drain never runs) can't grow memory without bound.
func TestTaskLogAppendCap(t *testing.T) {
	var l taskLog
	const n = taskLogPendingCap + 200
	for i := range n {
		l.append("line " + strconv.Itoa(i))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != taskLogPendingCap {
		t.Fatalf("pending grew to %d, want capped at %d", len(l.pending), taskLogPendingCap)
	}
	if first, want := l.pending[0], "line "+strconv.Itoa(n-taskLogPendingCap); first != want {
		t.Errorf("oldest retained line = %q, want %q", first, want)
	}
	if last, want := l.pending[len(l.pending)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest line = %q, want %q", last, want)
	}
}

// TestTaskLogDrainMovesPending checks drain moves pending into visible and
// clears pending, so a subsequent render reads the drained lines with no
// pending backlog left over.
func TestTaskLogDrainMovesPending(t *testing.T) {
	var l taskLog
	l.append("a")
	l.append("b")
	l.append("c")

	l.drain()

	got := l.lines()
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("visible after drain = %v, want [a b c]", got)
	}
	l.mu.Lock()
	pendingLen := len(l.pending)
	l.mu.Unlock()
	if pendingLen != 0 {
		t.Fatalf("pending after drain = %d, want 0 (drain must clear it)", pendingLen)
	}

	// A second drain with nothing pending is a no-op and leaves visible intact.
	l.drain()
	if got := l.lines(); len(got) != 3 {
		t.Fatalf("visible after empty drain = %d lines, want 3", len(got))
	}
}

// TestTaskLogDrainCapsVisible checks the drained, on-screen slice is bounded at
// taskLogVisibleCap (keeping the newest) even when many appends accumulate
// across several drains.
func TestTaskLogDrainCapsVisible(t *testing.T) {
	var l taskLog
	const n = taskLogVisibleCap + 350
	for i := range n {
		l.append("line " + strconv.Itoa(i))
		if i%7 == 0 { // interleave drains so it isn't one big flush
			l.drain()
		}
	}
	l.drain()

	got := l.lines()
	if len(got) != taskLogVisibleCap {
		t.Fatalf("visible = %d lines, want capped at %d", len(got), taskLogVisibleCap)
	}
	// The tail is retained: the very last appended line must be present.
	if last, want := got[len(got)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest visible line = %q, want %q", last, want)
	}
}

// TestTaskLogReset clears both buffers for a fresh modal open.
func TestTaskLogReset(t *testing.T) {
	var l taskLog
	l.append("x")
	l.drain()
	l.append("y")
	l.reset()
	if got := l.lines(); got != nil {
		t.Errorf("visible after reset = %v, want nil", got)
	}
	l.mu.Lock()
	pending := l.pending
	l.mu.Unlock()
	if pending != nil {
		t.Errorf("pending after reset = %v, want nil", pending)
	}
}

// TestTaskLogConcurrentAppendDrain is the -race safety net: many workers append
// while the UI thread drains, exercising the exact worker→UI handoff the modal
// views rely on. No assertion beyond "the race detector stays quiet"; run with
// `go test -race`.
func TestTaskLogConcurrentAppendDrain(t *testing.T) {
	var l taskLog
	var wg sync.WaitGroup
	const appenders = 6
	const perAppender = 4000

	for a := range appenders {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			for i := range perAppender {
				l.append("w" + strconv.Itoa(a) + "-" + strconv.Itoa(i))
			}
		}(a)
	}

	done := make(chan struct{})
	var drainWG sync.WaitGroup
	drainWG.Go(func() {
		for {
			select {
			case <-done:
				l.drain()
				_ = l.lines()
				return
			default:
				l.drain()
				_ = l.lines()
			}
		}
	})

	wg.Wait()
	close(done)
	drainWG.Wait()
}

// TestProgressModel locks the set/bump/reset/load math: set stores an absolute
// pair, bump advances done monotonically, and reset zeroes both.
func TestProgressModel(t *testing.T) {
	var p progressModel

	p.set(0, 10)
	if done, max := p.load(); done != 0 || max != 10 {
		t.Fatalf("after set(0,10) = (%d,%d), want (0,10)", done, max)
	}

	for range 10 {
		p.bump()
	}
	if done, max := p.load(); done != 10 || max != 10 {
		t.Fatalf("after 10 bumps = (%d,%d), want (10,10)", done, max)
	}

	// A fresh batch replaces both bounds, not accumulates.
	p.set(0, 3)
	if done, max := p.load(); done != 0 || max != 3 {
		t.Fatalf("after set(0,3) = (%d,%d), want (0,3)", done, max)
	}

	p.reset()
	if done, max := p.load(); done != 0 || max != 0 {
		t.Fatalf("after reset = (%d,%d), want (0,0)", done, max)
	}
}

// TestProgressModelConcurrentBump is the -race net for the per-file tick: many
// workers bump concurrently and every increment must land (no lost updates).
func TestProgressModelConcurrentBump(t *testing.T) {
	var p progressModel
	p.set(0, 0)

	var wg sync.WaitGroup
	const workers = 8
	const perWorker = 1000
	for range workers {
		wg.Go(func() {
			for range perWorker {
				p.bump()
			}
		})
	}
	wg.Wait()

	if done, _ := p.load(); done != workers*perWorker {
		t.Fatalf("done after concurrent bumps = %d, want %d (lost updates)", done, workers*perWorker)
	}
}

// TestProcScopeBeginNilRegistry: with no registry wired (tests, standalone use)
// beginProc returns a nil process and a no-op end.
func TestProcScopeBeginNilRegistry(t *testing.T) {
	var mu sync.Mutex
	s := procScope{mu: &mu}
	proc, end := s.beginProc(context.Background(), ProcImport, "x", nil, "status", nil)
	if proc != nil {
		t.Fatalf("beginProc with no registry = %v, want nil", proc)
	}
	end()
	if s.proc != nil {
		t.Fatal("slot set with no registry")
	}
}

// TestProcScopeBeginEnd: beginProc registers and publishes the process with its
// status, and end clears the slot and ends it — the lifecycle Import and
// Organize share (keep==nil, always store/clear).
func TestProcScopeBeginEnd(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg, mu: &mu}

	proc, end := s.beginProc(context.Background(), ProcImport, "test", nil, "working", nil)
	if proc == nil {
		t.Fatal("beginProc with a registry returned nil")
	}
	mu.Lock()
	got := s.proc
	mu.Unlock()
	if got != proc {
		t.Fatalf("slot after beginProc = %v, want the begun proc", got)
	}
	if snap := reg.Snapshot(); len(snap) != 1 || snap[0].Status != "working" {
		t.Fatalf("registry snapshot = %+v, want one process with status %q", snap, "working")
	}

	end()
	mu.Lock()
	got = s.proc
	mu.Unlock()
	if got != nil {
		t.Fatalf("slot after end = %v, want nil", got)
	}
	if n := reg.Count(); n != 0 {
		t.Fatalf("registry has %d live processes after end, want 0", n)
	}
}

// TestProcScopeGenGuard mirrors the Duplicates Rescan supersession: a stale pass
// (keep()==false) must neither claim the slot from the live pass nor nil it out
// on the way down. Only the current pass (keep()==true) may write the slot.
func TestProcScopeGenGuard(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg, mu: &mu}
	current := true

	live, endLive := s.beginProc(context.Background(), ProcDuplicates, "live", nil, "", func() bool { return current })
	// A superseded pass must not overwrite the live pass's slot.
	_, endStale := s.beginProc(context.Background(), ProcDuplicates, "stale", nil, "", func() bool { return false })
	if s.proc != live {
		t.Fatalf("stale beginProc overwrote the slot: got %v, want the live proc", s.proc)
	}
	// ...nor clear it on the way down.
	endStale()
	if s.proc != live {
		t.Fatal("stale end cleared the live pass's slot")
	}
	// The live pass clears its own slot.
	endLive()
	if s.proc != nil {
		t.Fatal("live end did not clear the slot")
	}
	if n := reg.Count(); n != 0 {
		t.Fatalf("registry left with %d live processes, want 0", n)
	}
}

// TestProcScopeConcurrent is the -race net for the lifecycle under load: many
// passes begin, read, drive progress (notify) and end against one
// scope+registry, with the view mutex serializing the slot writes exactly as
// the real views do.
func TestProcScopeConcurrent(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg, mu: &mu}

	var wg sync.WaitGroup
	const goroutines = 40
	for range goroutines {
		wg.Go(func() {
			_, end := s.beginProc(context.Background(), ProcImport, "t", nil, "", nil)
			mu.Lock()
			cur := s.proc
			mu.Unlock()
			cur.AddDone(1) // nil-safe; routes through registry.notify()
			end()
		})
	}
	wg.Wait()

	if n := reg.Count(); n != 0 {
		t.Fatalf("registry left with %d live processes, want 0", n)
	}
}

// TestClampProgressDone: the shared progress bar clamps done into [0,max] for
// every caller, so Organize's done (which can exceed max) can no longer paint
// past the track, and a negative tick cannot paint backwards. max <= 0 means
// "no known total" and yields 0 (an empty track).
func TestClampProgressDone(t *testing.T) {
	cases := []struct {
		done, max int64
		want      int64
	}{
		{0, 0, 0},
		{5, 0, 0},
		{0, 10, 0},
		{5, 10, 5},
		{10, 10, 10},
		{11, 10, 10},
		{1000, 10, 10},
		{-1, 10, 0},
		{-1, 0, 0},
	}
	for _, tc := range cases {
		if got := clampProgressDone(tc.done, tc.max); got != tc.want {
			t.Errorf("clampProgressDone(%d, %d) = %d, want %d", tc.done, tc.max, got, tc.want)
		}
	}
}
