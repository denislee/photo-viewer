package ui

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// TestTaskViewSetStatusCoalescesWithLiveProc is the U3-03 regression: with a
// process live, setStatus must not invalidate the view directly — that
// notification rides the registry's coalescer instead — while still recording
// the status line under the shared view mutex.
func TestTaskViewSetStatusCoalescesWithLiveProc(t *testing.T) {
	var direct atomic.Int64
	var registryInvalidates atomic.Int64

	reg := NewProcessRegistry(func() { registryInvalidates.Add(1) })
	v := NewOrganizeView(func() { direct.Add(1) })
	v.SetProcessRegistry(reg)

	proc := reg.Begin(context.Background(), ProcOrganize, "t", nil, true)
	v.mu.Lock()
	v.scope.proc = proc
	v.mu.Unlock()

	for i := range 100 {
		v.setStatus(fmt.Sprint(i))
	}

	// The U3-03 regression: Organize's old copy called v.invalidate() even with
	// a live process, doubling every wakeup against the registry coalescer.
	if got := direct.Load(); got != 0 {
		t.Errorf("direct invalidates = %d, want 0 (setStatus must coalesce through the live process)", got)
	}

	v.mu.Lock()
	status := v.statusMsg
	v.mu.Unlock()
	if status != "99" {
		t.Errorf("statusMsg = %q, want %q", status, "99")
	}

	proc.End()
}

// TestTaskViewSetStatusWithoutProcInvalidates: with no process registered
// (tests, or a one-shot message before a run starts) setStatus falls back to
// the raw invalidate — once per call, so the status line can't look frozen.
func TestTaskViewSetStatusWithoutProcInvalidates(t *testing.T) {
	var direct atomic.Int64
	v := NewOrganizeView(func() { direct.Add(1) })

	for range 3 {
		v.setStatus("x")
	}

	if got := direct.Load(); got != 3 {
		t.Errorf("direct invalidates = %d, want 3", got)
	}
}
