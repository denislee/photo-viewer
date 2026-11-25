package ui

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAsyncMemoOneWorkerNoDuplicates pins I-15 / G6-04: many paths requested
// back to back are computed by a single worker (never two lookups at once),
// each exactly once, and a repeat get while queued returns pending without
// queueing again.
func TestAsyncMemoOneWorkerNoDuplicates(t *testing.T) {
	var m asyncMemo[string]
	var inFlight, maxInFlight atomic.Int32
	var mu sync.Mutex
	calls := map[string]int{}
	compute := func(p string) string {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		calls[p]++
		mu.Unlock()
		inFlight.Add(-1)
		return "v:" + p
	}
	for i := range 50 {
		p := fmt.Sprint(i)
		if got := m.get(p, "…", compute, nil); got != "…" {
			t.Fatalf("first get(%s) = %q, want pending", p, got)
		}
		m.get(p, "…", compute, nil) // repeat while queued
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("memo never filled")
		}
		time.Sleep(time.Millisecond)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("max concurrent lookups = %d, want 1 (one shared worker)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for p, n := range calls {
		if n != 1 {
			t.Errorf("path %s computed %d times, want 1", p, n)
		}
	}
	if len(calls) != 50 {
		t.Errorf("computed %d paths, want 50", len(calls))
	}
}

// TestAsyncMemoNewestFirst: with the worker busy, the most recently requested
// path (the one on screen) is answered before older queued ones.
func TestAsyncMemoNewestFirst(t *testing.T) {
	var m asyncMemo[int]
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	compute := func(p string) int {
		if p == "busy" {
			<-release
		}
		mu.Lock()
		order = append(order, p)
		mu.Unlock()
		return 1
	}
	m.get("busy", 0, compute, nil)
	time.Sleep(10 * time.Millisecond) // let the worker pick "busy"
	m.get("old", 0, compute, nil)
	m.get("new", 0, compute, nil)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for m.get("old", 0, compute, nil) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("memo never filled")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[1] != "new" || order[2] != "old" {
		t.Errorf("compute order = %v, want [busy new old]", order)
	}
}
