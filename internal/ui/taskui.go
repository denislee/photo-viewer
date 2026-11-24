package ui

import (
	"context"
	"image"
	"sync"
	"sync/atomic"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// This file holds the plumbing the three modal task views (Import, Organize,
// Duplicates) used to hand-roll a copy of each:
//
//   - taskLog       — a bounded log buffer with a UI-thread drain (Import, Organize)
//   - progressModel — done/max progress state (Import, Organize) and the bar
//     painter layoutProgressBar (all three; Duplicates draws it 14dp tall)
//   - procScope     — the Process begin/publish/end lifecycle (all three)
//
// Extracting them (finding U-15) kills the drift that had already crept in
// between the copies — e.g. organize's logBuf growing unbounded before U-16.

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

// layoutProgressBar paints the thin progress bar shared by the Import and
// Organize modals (6dp) and by the Duplicates hashing screen (14dp): a
// full-width CellBG track with an Accent fill proportional to done/max. done is
// clamped to [0,max] so a pass that overshoots its total (both views can) never
// paints past the track width or backwards into the track.
func layoutProgressBar(gtx layout.Context, th *Theme, done, max int64, height unit.Dp) layout.Dimensions {
	done = clampProgressDone(done, max)
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(height)
	bg := image.Rect(0, 0, w, barH)
	paint.FillShape(gtx.Ops, th.CellBG, clip.Rect(bg).Op())
	if max > 0 {
		frac := float32(done) / float32(max)
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		paint.FillShape(gtx.Ops, th.Accent, clip.Rect(fg).Op())
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

// clampProgressDone bounds done to [0,total]. A total of zero or less means "no
// known total" (the bar paints an empty track), so done collapses to zero.
func clampProgressDone(done, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return min(max(done, 0), total)
}

// procScope owns the single "live Process" slot a modal task view publishes to
// the process bar. The slot is guarded by the owning view's own mutex (s.mu),
// which is why every access goes through beginProc and its returned end func
// rather than raw field writes. beginProc registers with the registry (nil when
// none is wired, e.g. tests), sets the status, and publishes the process, all in
// one call that takes the lock itself.
type procScope struct {
	reg  *ProcessRegistry
	mu   *sync.Mutex // the owning view's mutex
	proc *Process    // guarded by the owning view's mutex
}

// beginProc registers a process with the bar, sets its status, and publishes it
// as the live process under s.mu. keep, when non-nil, gates both the publish and
// the later clear on the caller's generation guard (the Duplicates Rescan
// supersession check). It returns the process (nil when no registry is wired,
// e.g. tests; *Process methods are nil-safe) and an end func that clears the
// slot (same keep gate) and then calls proc.End() outside the lock. The caller
// must NOT hold s.mu.
func (s *procScope) beginProc(ctx context.Context, kind ProcKind, title string, cancel func(), status string, keep func() bool) (*Process, func()) {
	proc := s.reg.Begin(ctx, kind, title, cancel, true)
	if proc == nil {
		return nil, func() {}
	}
	if status != "" {
		proc.SetStatus(status)
	}
	s.mu.Lock()
	if keep == nil || keep() {
		s.proc = proc
	}
	s.mu.Unlock()
	end := func() {
		s.mu.Lock()
		if (keep == nil || keep()) && s.proc == proc {
			s.proc = nil
		}
		s.mu.Unlock()
		proc.End()
	}
	return proc, end
}

// taskView is the plumbing the Import and Organize modals share: the view
// mutex, the status line, the log buffer + widget, the progress model, the
// live-Process slot and the invalidate callback, plus the methods both views
// used to hand-copy (U4-06). Each view embeds it, so v.mu, v.statusMsg, v.scope
// … resolve to these fields. mu is THE view mutex: it guards statusMsg and
// scope.proc (procScope's "caller holds the view mutex" contract) as well as the
// embedding view's own fields, so there is exactly one lock and no ordering to
// get wrong.
type taskView struct {
	mu         sync.Mutex
	statusMsg  string
	log        taskLog
	progress   progressModel
	scope      procScope
	invalidate func()
	logList    widget.List
}

// initTaskView wires the shared plumbing: the invalidate callback and the log
// list's vertical, pinned-to-bottom layout.
func (t *taskView) initTaskView(invalidate func()) {
	t.invalidate = invalidate
	t.scope.mu = &t.mu
	t.logList.Axis = layout.Vertical
	// Stick the log to the bottom so newly-appended lines stay visible. Once
	// the user manually scrolls up, BeforeEnd flips and Gio stops auto-pinning.
	t.logList.ScrollToEnd = true
}

// SetProcessRegistry wires the view to the main-screen process bar so each run
// publishes progress, pause, and cancel controls outside the modal.
func (t *taskView) SetProcessRegistry(r *ProcessRegistry) { t.scope.reg = r }

// currentProc returns the live process for this view, or nil when none is
// registered (e.g. tests). Safe to call from any goroutine.
func (t *taskView) currentProc() *Process {
	t.mu.Lock()
	proc := t.scope.proc
	t.mu.Unlock()
	return proc
}

// setStatus replaces the status line shown above the progress bar. Safe to
// call from any goroutine; the UI redraws on the next frame. When a process is
// live the status rides the registry's coalescer and returns early instead of
// also invalidating directly — that early return is what fixes U3-03 for
// Organize, whose copy used to fire both paths.
func (t *taskView) setStatus(msg string) {
	t.mu.Lock()
	t.statusMsg = msg
	proc := t.scope.proc
	t.mu.Unlock()
	if proc != nil {
		// SetStatus notifies through the registry coalescer; skip the direct
		// invalidate to avoid a redundant second wakeup for the same change.
		proc.SetStatus(msg)
		return
	}
	t.scheduleInvalidate()
}

// waitIfPaused blocks while the running task is paused via the process bar.
// Workers call this at loop boundaries; if no process is registered (e.g.
// tests) it's a no-op.
func (t *taskView) waitIfPaused() {
	t.currentProc().Wait()
}

// scheduleInvalidate wakes the Gio frame loop through the process registry's
// ~30Hz coalescer when one is wired up. The task views' per-file paths
// (appendLog, setProgress, bumpProgress) fire once per file — thousands/sec
// during a same-device rename — so calling v.invalidate directly from them
// would peg a core repainting a log that scrolls far faster than the eye can
// read. The registry coalesces to ~30Hz with a guaranteed trailing flush, so the
// final log line / progress value still lands. Falls back to the raw invalidate
// when no registry is set (e.g. tests, or one-shot messages before a run
// starts) — those paths aren't in a per-file storm.
func (t *taskView) scheduleInvalidate() {
	if t.scope.reg != nil {
		t.scope.reg.notify()
		return
	}
	if t.invalidate != nil {
		t.invalidate()
	}
}

// appendLog adds one line to the bounded log and asks for a repaint.
func (t *taskView) appendLog(msg string) {
	t.log.append(msg)
	t.scheduleInvalidate()
}

// setProgress stores an absolute done/max pair and drives the process bar. With
// no live process it falls back to the coalesced invalidate.
func (t *taskView) setProgress(done, max int64) {
	t.progress.set(done, max)
	if proc := t.currentProc(); proc != nil {
		// proc.SetDone/SetTotal already route through the registry's ~30Hz
		// coalescer, so no direct v.invalidate() here — that would re-add the
		// per-file storm this initiative removes.
		proc.SetDone(done)
		proc.SetTotal(max)
		return
	}
	t.scheduleInvalidate()
}

// bumpProgress advances done by one and drives the process bar.
func (t *taskView) bumpProgress() {
	t.progress.bump()
	if proc := t.currentProc(); proc != nil {
		// proc.AddDone already coalesces the redraw through the registry; a
		// direct v.invalidate() per file is exactly the storm we're killing.
		proc.AddDone(1)
		return
	}
	t.scheduleInvalidate()
}

// layoutLog paints the shared log pane: a CellBG fill, then the scrolled lines
// at the caller's font size (Import 14sp, Organize 12sp).
func (t *taskView) layoutLog(gtx layout.Context, th *Theme, lines []string, size unit.Sp) layout.Dimensions {
	drawBackground(gtx, th.CellBG)
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return t.logList.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
			lbl := material.Label(th.Theme, size, lines[i])
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		})
	})
}
