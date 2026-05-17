package ui

import (
	"sync"
	"sync/atomic"
	"time"
)

// ProcKind identifies the long-running background task surfaced in the
// running-processes bar.
type ProcKind int

const (
	ProcImport ProcKind = iota
	ProcDuplicates
	ProcOrganize
	ProcScan
	ProcWarmUp
)

func (k ProcKind) String() string {
	switch k {
	case ProcImport:
		return "Import"
	case ProcDuplicates:
		return "Duplicates"
	case ProcOrganize:
		return "Organize"
	case ProcScan:
		return "Indexing"
	case ProcWarmUp:
		return "Thumbnails"
	}
	return "Process"
}

// PauseGate is a small sync.Cond wrapper that workers call Wait() on at
// loop boundaries. Pause() flips the gate closed; Resume() lets every
// waiter through. Cancel paths must Resume() so workers can observe the
// context cancellation and exit instead of sleeping forever.
type PauseGate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

func NewPauseGate() *PauseGate {
	g := &PauseGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *PauseGate) Wait() {
	g.mu.Lock()
	for g.paused {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

func (g *PauseGate) Pause() {
	g.mu.Lock()
	g.paused = true
	g.mu.Unlock()
}

func (g *PauseGate) Resume() {
	g.mu.Lock()
	g.paused = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *PauseGate) IsPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Process is a single tracked background task. Workers update Done/Total/
// Status from any goroutine; the UI reads via ProcessRegistry.Snapshot.
type Process struct {
	ID      int64
	Kind    ProcKind
	Title   string
	Started time.Time

	mu     sync.Mutex
	status string
	done   int64
	total  int64

	gate   *PauseGate
	cancel func()

	registry *ProcessRegistry
}

func (p *Process) SetStatus(s string) {
	p.mu.Lock()
	p.status = s
	p.mu.Unlock()
	p.registry.notify()
}

func (p *Process) SetTotal(t int64) {
	atomic.StoreInt64(&p.total, t)
	p.registry.notify()
}

func (p *Process) SetDone(d int64) {
	atomic.StoreInt64(&p.done, d)
	p.registry.notify()
}

func (p *Process) AddDone(delta int64) {
	atomic.AddInt64(&p.done, delta)
	p.registry.notify()
}

// Wait blocks while the process is paused. Workers should call this at
// loop boundaries so pause requests take effect within one iteration.
func (p *Process) Wait() {
	if p.gate != nil {
		p.gate.Wait()
	}
}

func (p *Process) Pause() {
	if p.gate != nil {
		p.gate.Pause()
	}
	p.registry.notify()
}

func (p *Process) Resume() {
	if p.gate != nil {
		p.gate.Resume()
	}
	p.registry.notify()
}

func (p *Process) IsPaused() bool {
	if p.gate == nil {
		return false
	}
	return p.gate.IsPaused()
}

// Cancel runs the registered cancel function. Also resumes the pause
// gate so any worker blocked inside Wait() can observe the cancellation
// and exit promptly.
func (p *Process) Cancel() {
	if p.gate != nil {
		p.gate.Resume()
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.registry.notify()
}

// End removes the process from the registry. Safe to call from any
// goroutine, typically from a defer in the worker.
func (p *Process) End() {
	if p.gate != nil {
		// Make sure no worker is stuck waiting on the gate when we
		// drop the process — End usually means the work goroutine is
		// done so this is a belt-and-braces guarantee.
		p.gate.Resume()
	}
	p.registry.remove(p.ID)
}

// ProcessSnapshot is a read-only copy of a Process used by the UI so
// rendering doesn't need to hold the registry lock.
type ProcessSnapshot struct {
	ID      int64
	Kind    ProcKind
	Title   string
	Status  string
	Done    int64
	Total   int64
	Paused  bool
	Started time.Time
}

// ProcessRegistry is the central index of in-flight background tasks.
// Views (Import, Duplicates, Organize, Controller scans/warm-up) Begin
// a process when they start work and End it when the work goroutine
// returns. The main window draws a status bar from Snapshot().
type ProcessRegistry struct {
	mu         sync.Mutex
	nextID     int64
	procs      []*Process
	invalidate func()
}

func NewProcessRegistry(invalidate func()) *ProcessRegistry {
	return &ProcessRegistry{invalidate: invalidate}
}

func (r *ProcessRegistry) notify() {
	if r.invalidate != nil {
		r.invalidate()
	}
}

// Begin registers a new process. cancel is invoked when the user clicks
// the Cancel button. Returns a handle the worker calls Wait/SetStatus/
// AddDone/End on. supportsPause=false produces a process with no gate
// (Wait is a no-op and Pause/Resume are inert).
func (r *ProcessRegistry) Begin(kind ProcKind, title string, cancel func(), supportsPause bool) *Process {
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	p := &Process{
		ID:       id,
		Kind:     kind,
		Title:    title,
		Started:  time.Now(),
		cancel:   cancel,
		registry: r,
	}
	if supportsPause {
		p.gate = NewPauseGate()
	}
	r.procs = append(r.procs, p)
	r.mu.Unlock()
	r.notify()
	return p
}

func (r *ProcessRegistry) remove(id int64) {
	r.mu.Lock()
	for i, p := range r.procs {
		if p.ID == id {
			r.procs = append(r.procs[:i], r.procs[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	r.notify()
}

// Snapshot returns a sorted, read-only view of the current processes.
// Ordering is by start time so the UI is stable across frames.
func (r *ProcessRegistry) Snapshot() []ProcessSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.procs) == 0 {
		return nil
	}
	out := make([]ProcessSnapshot, len(r.procs))
	for i, p := range r.procs {
		p.mu.Lock()
		status := p.status
		p.mu.Unlock()
		out[i] = ProcessSnapshot{
			ID:      p.ID,
			Kind:    p.Kind,
			Title:   p.Title,
			Status:  status,
			Done:    atomic.LoadInt64(&p.done),
			Total:   atomic.LoadInt64(&p.total),
			Paused:  p.IsPaused(),
			Started: p.Started,
		}
	}
	return out
}

// Get returns the process with the given ID, or nil if it's already
// finished. Used by the bar's Cancel/Pause buttons to find the live
// Process for the snapshot row the user clicked.
func (r *ProcessRegistry) Get(id int64) *Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.procs {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Count returns the number of in-flight processes.
func (r *ProcessRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.procs)
}
