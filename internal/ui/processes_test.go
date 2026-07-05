package ui

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestProcessRegistryNotifyCoalesces guards initiative I-10's non-negotiable
// requirement: a storm of notify() calls must collapse into far fewer
// frame-loop wakeups, AND the final update must still be delivered by the
// trailing flush — a naive drop-if-too-soon throttle would freeze a progress
// bar just short of done.
func TestProcessRegistryNotifyCoalesces(t *testing.T) {
	var calls int64
	r := NewProcessRegistry(func() { atomic.AddInt64(&calls, 1) })

	const burst = 5000
	for i := 0; i < burst; i++ {
		r.notify()
	}

	// (a) Coalescing: the whole tight loop finishes in well under one
	// interval, so only the leading-edge fire should have landed so far.
	during := atomic.LoadInt64(&calls)
	if during >= burst/10 {
		t.Fatalf("notify not coalesced: %d invalidate calls for %d notifies", during, burst)
	}

	// (b) Trailing edge: once the interval elapses the coalesced-away update
	// must be flushed, so the terminal state is never lost.
	time.Sleep(processNotifyInterval * 3)
	after := atomic.LoadInt64(&calls)
	if after <= during {
		t.Fatalf("trailing flush missing: invalidate count stayed at %d after the burst settled", during)
	}
}

// TestProcessRegistryNotifySustainedRate checks that a continuous stream of
// notifies over several intervals wakes the frame loop at roughly the throttle
// rate (a handful of times), not once per notify.
func TestProcessRegistryNotifySustainedRate(t *testing.T) {
	var calls int64
	r := NewProcessRegistry(func() { atomic.AddInt64(&calls, 1) })

	deadline := time.Now().Add(processNotifyInterval * 6)
	var n int
	for time.Now().Before(deadline) {
		r.notify()
		n++
		time.Sleep(time.Millisecond) // ~1000 notifies/sec
	}
	// Let the final trailing flush land.
	time.Sleep(processNotifyInterval * 2)

	got := atomic.LoadInt64(&calls)
	if got == 0 {
		t.Fatalf("no invalidate fired over %d notifies", n)
	}
	if int(got) > n/4 {
		t.Fatalf("insufficient coalescing: %d invalidate calls for %d notifies", got, n)
	}
}

// TestProcessRegistryForceNotify verifies structural events repaint
// immediately, bypassing the throttle, and reset the window.
func TestProcessRegistryForceNotify(t *testing.T) {
	var calls int64
	r := NewProcessRegistry(func() { atomic.AddInt64(&calls, 1) })

	// Prime the throttle window with a leading-edge notify, then confirm a
	// following throttled notify is coalesced away...
	r.notify()
	r.notify()
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected 1 leading-edge invalidate, got %d", got)
	}
	// ...but a forceNotify fires right away regardless of the window.
	r.forceNotify()
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("forceNotify should invalidate immediately, count is %d", got)
	}
}
