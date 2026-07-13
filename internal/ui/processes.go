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
	ProcExportFavorites
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
	case ProcExportFavorites:
		return "Export favorites"
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
	done   atomic.Int64
	total  atomic.Int64

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
	p.total.Store(t)
	p.registry.notify()
}

func (p *Process) SetDone(d int64) {
	p.done.Store(d)
	p.registry.notify()
}

func (p *Process) AddDone(delta int64) {
	p.done.Add(delta)
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
	// User-driven one-shot: force an immediate repaint so the paused state
	// shows without waiting on the progress-notify throttle.
	p.registry.forceNotify()
}

func (p *Process) Resume() {
	if p.gate != nil {
		p.gate.Resume()
	}
	p.registry.forceNotify()
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
	p.registry.forceNotify()
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

// processNotifyInterval is the minimum spacing between frame-loop wakeups
// triggered by per-item progress updates (~30Hz). See ProcessRegistry.notify.
const processNotifyInterval = 33 * time.Millisecond

// ProcessRegistry is the central index of in-flight background tasks.
// Views (Import, Duplicates, Organize, Controller scans/warm-up) Begin
// a process when they start work and End it when the work goroutine
// returns. The main window draws a status bar from Snapshot().
type ProcessRegistry struct {
	mu         sync.Mutex
	nextID     int64
	procs      []*Process
	invalidate func()

	// notify coalescer. Workers call notify() once per processed item via
	// SetStatus/SetTotal/SetDone/AddDone. On a warm-cache warm-up or a fast
	// same-device import that fires thousands of times/sec, and each call
	// would wake the Gio frame loop — so the loop redraws continuously and
	// pegs a core even though the per-frame delta is a sliver of progress
	// bar. We coalesce those wakeups to at most one per processNotifyInterval:
	// the first notify in an idle window invalidates immediately (leading
	// edge) and arms a timer; further notifies inside the window only set
	// notifyPending; when the timer fires it invalidates once more so the
	// coalesced-away updates are drawn. That trailing flush is what
	// guarantees the FINAL state (100%, "done", the last status) always
	// lands even though the intermediate storm was dropped — a naive
	// drop-if-too-soon throttle would freeze the bar just short of the end.
	// All of notifyLast/notifyPending/notifyTimer are touched only under
	// notifyMu (notify is called from background goroutines).
	notifyMu      sync.Mutex
	notifyLast    time.Time
	notifyPending bool
	notifyTimer   *time.Timer
}

func NewProcessRegistry(invalidate func()) *ProcessRegistry {
	return &ProcessRegistry{invalidate: invalidate}
}

// notify requests a redraw, rate-limited to ~30Hz. Safe to call from any
// goroutine and safe to call thousands of times per second — that's the whole
// point. The trailing timer guarantees the last update is never lost; see the
// ProcessRegistry.notifyMu comment.
func (r *ProcessRegistry) notify() {
	if r.invalidate == nil {
		return
	}
	r.notifyMu.Lock()
	now := time.Now()
	if now.Sub(r.notifyLast) >= processNotifyInterval {
		// Leading edge: nothing was drawn in the last interval, fire now.
		r.notifyLast = now
		r.notifyPending = false
		r.notifyMu.Unlock()
		r.invalidate()
		return
	}
	// Too soon since the last paint. Remember the update and make sure a timer
	// is armed to flush it once the interval elapses, so this update can't be
	// the terminal one that gets silently dropped.
	if !r.notifyPending {
		r.notifyPending = true
		delay := processNotifyInterval - now.Sub(r.notifyLast)
		if r.notifyTimer == nil {
			r.notifyTimer = time.AfterFunc(delay, r.flushNotify)
		} else {
			r.notifyTimer.Reset(delay)
		}
	}
	r.notifyMu.Unlock()
}

// flushNotify is the trailing edge of the coalescer: it fires the one redraw
// that stands in for every notify() coalesced away during the last window.
func (r *ProcessRegistry) flushNotify() {
	r.notifyMu.Lock()
	if !r.notifyPending {
		r.notifyMu.Unlock()
		return
	}
	r.notifyPending = false
	r.notifyLast = time.Now()
	r.notifyMu.Unlock()
	if r.invalidate != nil {
		r.invalidate()
	}
}

// forceNotify requests an immediate redraw, bypassing (and resetting) the
// throttle window. Used for structural changes — a process added or removed,
// or a user-driven pause/resume/cancel — which are rare (no storm risk) and
// must repaint promptly so the process list never looks stale.
func (r *ProcessRegistry) forceNotify() {
	if r.invalidate == nil {
		return
	}
	r.notifyMu.Lock()
	r.notifyLast = time.Now()
	r.notifyPending = false
	if r.notifyTimer != nil {
		r.notifyTimer.Stop()
	}
	r.notifyMu.Unlock()
	r.invalidate()
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
	// A new row appeared — force an immediate repaint rather than throttling a
	// structural change behind the per-item coalescer.
	r.forceNotify()
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
	// A row disappeared — repaint promptly so a finished process doesn't
	// linger on screen until the next throttled progress tick (which may
	// never come, since its worker has stopped calling notify()).
	r.forceNotify()
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
			Done:    p.done.Load(),
			Total:   p.total.Load(),
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
