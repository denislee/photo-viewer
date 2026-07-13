package ui

import (
	"image"
	"sync"
	"sync/atomic"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

// This file holds the plumbing the three modal task views (Import, Organize,
// Duplicates) used to hand-roll a copy of each:
//
//   - taskLog       — a bounded log buffer with a UI-thread drain (Import, Organize)
//   - progressModel — done/max progress state + the thin-bar painter (Import, Organize)
//   - procScope     — the Process begin/attach/nil-on-end lifecycle (all three)
//
// Extracting them (finding U-15) kills the drift that had already crept in
// between the copies — e.g. organize's logBuf growing unbounded before U-16.
// The Duplicates view keeps its own int-based, centred progress display
// (layoutProgress) because it is genuinely different from the thin Import/
// Organize bar; only its Process lifecycle is shared via procScope.

const (
	// taskLogPendingCap bounds the not-yet-drained buffer so a long pass whose
	// modal is never laid out (drainLog never runs) can't grow it without bound.
	// This is the 500-line cap import always had; organize gained it in U-16.
	taskLogPendingCap = 500
	// taskLogVisibleCap bounds the drained, on-screen slice — the 1000-line cap
	// both views already used (import's importLogVisibleLines / organize's literal).
	taskLogVisibleCap = 1000
)

// taskLog is a bounded, goroutine-safe log buffer. Worker goroutines append
// lines; the UI thread drains them into the visible slice once per frame so
// rendering reads them without contending on the append path. It owns its own
// mutex (separate from the view's) so a per-file appendLog storm never blocks on
// the view lock — exactly the "avoid mutex thrash" the original comment cited —
// and so the helper is a self-contained, race-testable unit.
type taskLog struct {
	mu      sync.Mutex
	pending []string // appended by workers, not yet shown
	visible []string // drained into on the UI thread, read during render
}

// append adds one line and trims the pending buffer to the newest
// taskLogPendingCap lines. Safe to call from any goroutine.
func (l *taskLog) append(line string) {
	l.mu.Lock()
	l.pending = append(l.pending, line)
	if len(l.pending) > taskLogPendingCap {
		l.pending = l.pending[len(l.pending)-taskLogPendingCap:]
	}
	l.mu.Unlock()
}

// drain moves the pending lines into the visible slice (capped at
// taskLogVisibleCap, keeping the newest) and clears pending. Call from the UI
// goroutine at the top of Layout.
func (l *taskLog) drain() {
	l.mu.Lock()
	if len(l.pending) > 0 {
		l.visible = append(l.visible, l.pending...)
		l.pending = l.pending[:0]
		if len(l.visible) > taskLogVisibleCap {
			l.visible = l.visible[len(l.visible)-taskLogVisibleCap:]
		}
	}
	l.mu.Unlock()
}

// lines returns the visible slice header under the lock. The UI thread passes
// it straight to the log widget; workers only ever touch pending, so the
// returned backing array is never mutated out from under a render.
func (l *taskLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.visible
}

// reset clears both buffers, used when a modal reopens for a fresh run.
func (l *taskLog) reset() {
	l.mu.Lock()
	l.pending = nil
	l.visible = nil
	l.mu.Unlock()
}

// progressModel is the shared done/max progress state for the modal task views.
// Workers advance it lock-free (set/bump); the UI thread snapshots it once per
// frame with load() and paints the bar with layoutProgressBar, passing that same
// snapshot so the bar and any adjacent counter label never disagree within a
// frame.
type progressModel struct {
	done atomic.Int64
	max  atomic.Int64
}

// set stores an absolute done/max pair (a new batch's bounds).
func (p *progressModel) set(done, max int64) {
	p.done.Store(done)
	p.max.Store(max)
}

// bump advances done by one — the per-file tick.
func (p *progressModel) bump() {
	p.done.Add(1)
}

// reset zeroes both counters.
func (p *progressModel) reset() {
	p.done.Store(0)
	p.max.Store(0)
}

// load returns a consistent-enough snapshot for a frame (two atomic loads).
func (p *progressModel) load() (done, max int64) {
	return p.done.Load(), p.max.Load()
}

// layoutProgressBar paints the thin 6dp progress bar shared by the Import and
// Organize modals: a full-width CellBG track with an Accent fill proportional to
// done/max. clamp caps the fill at the track width — Import always clamped;
// Organize never did (its done never exceeds max), so the flag preserves each
// view's exact rendering.
func layoutProgressBar(gtx layout.Context, th *Theme, done, max int64, clamp bool) layout.Dimensions {
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(unit.Dp(6))
	bg := image.Rect(0, 0, w, barH)
	clipBg := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipBg.Pop()
	if max > 0 {
		frac := float32(done) / float32(max)
		if clamp && frac > 1 {
			frac = 1
		}
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		clipFg := clip.Rect(fg).Push(gtx.Ops)
		paint.ColorOp{Color: th.Accent}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		clipFg.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

// procScope owns the single "live Process" slot a modal task view publishes to
// the process bar. The slot is guarded by the owning view's own mutex, not one
// of its own: attach/detach/current all assume the caller holds it, matching the
// explicit lock/unlock each view already wrote around its v.proc writes. begin
// registers with the registry (or returns nil when none is wired, e.g. tests)
// and takes no lock — the caller configures status/total and then attaches,
// preserving each view's begin→configure→attach order exactly.
type procScope struct {
	reg  *ProcessRegistry
	proc *Process // guarded by the owning view's mutex
}

// begin registers a process with the bar, or returns nil when no registry is
// wired. supportsPause is always true for the modal views. Takes no lock.
func (s *procScope) begin(kind ProcKind, title string, cancel func()) *Process {
	if s.reg == nil {
		return nil
	}
	return s.reg.Begin(kind, title, cancel, true)
}

// attachLocked publishes proc as the live process. keep, when non-nil, gates the
// store on the caller's generation guard (the Duplicates Rescan supersession
// check); a nil keep always stores. The caller must hold the view mutex.
func (s *procScope) attachLocked(proc *Process, keep func() bool) {
	if keep == nil || keep() {
		s.proc = proc
	}
}

// detachLocked clears the live-process slot under the same keep guard. The
// caller must hold the view mutex, then call proc.End() outside the lock.
func (s *procScope) detachLocked(keep func() bool) {
	if keep == nil || keep() {
		s.proc = nil
	}
}

// currentLocked returns the live process (nil when none). The caller must hold
// the view mutex.
func (s *procScope) currentLocked() *Process {
	return s.proc
}
