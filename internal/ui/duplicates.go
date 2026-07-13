package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"os"
	"sync"

	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// DuplicatesView is the modal "find duplicates" overlay. Open is true while
// the view should be drawn over the rest of the UI.
type DuplicatesView struct {
	Open    bool
	OnClose func()

	idx    *cache.Index
	store  *cache.ThumbStore
	thumbs *thumbCache

	// deleter is the soft-delete function (Controller.DeletePath when wired
	// from window.go). Nil falls back to plain os.Remove for tests and
	// stand-alone use of the view.
	deleter func(path string) error
	// batchDeleter is the bulk variant — used by "Delete all newer"
	// confirmations so 100 duplicates pay for one in-memory patch + one
	// index transaction instead of N. Nil falls back to repeated calls
	// to deleter (or removeOne) so tests can exercise the view without
	// a Controller.
	batchDeleter func(paths []string) error

	mu      sync.Mutex
	hashing bool
	// gen is bumped by startHash on every fresh pass. The background
	// hashAndScan goroutine captures the value at launch and drops any state
	// write once d.gen no longer matches — so a pass that Rescan superseded
	// (its ctx cancelled) can't clobber the new pass's state during teardown
	// (e.g. writing "Error: context canceled" / hashing=false over it).
	gen       int
	done      int
	total     int
	groups    []cache.DuplicateGroup
	selected  int
	statusMsg string

	cancel context.CancelFunc

	closeBtn      widget.Clickable
	rescanBtn     widget.Clickable
	acceptAllBtn  widget.Clickable
	confirmAllBtn widget.Clickable
	deleteBtn     widget.Clickable
	confirmBtn    widget.Clickable
	cancelBtn     widget.Clickable
	confirming    bool
	confirmingAll bool
	groupList     widget.List
	detailList    widget.List
	groupClicks   []*widget.Clickable

	invalidate func()

	processes *ProcessRegistry
	proc      *Process
}

// NewDuplicatesView wires the view to the index/store/thumb cache and an
// invalidate callback used to wake the Gio frame loop from background
// goroutines.
func NewDuplicatesView(idx *cache.Index, store *cache.ThumbStore, thumbs *thumbCache, invalidate func()) *DuplicatesView {
	d := &DuplicatesView{idx: idx, store: store, thumbs: thumbs, invalidate: invalidate, selected: -1}
	d.groupList.Axis = layout.Vertical
	d.detailList.Axis = layout.Vertical
	return d
}

// SetProcessRegistry wires the hashing pass into the main-screen
// process bar so the user can pause / resume / cancel without keeping
// the modal open.
func (d *DuplicatesView) SetProcessRegistry(r *ProcessRegistry) { d.processes = r }

// SetDeleter wires the soft-delete callback so duplicate-removal goes
// through the same trash flow as keyboard delete (rename → trash dir,
// instant UI update). Without it, deletions fall back to a direct unlink.
func (d *DuplicatesView) SetDeleter(f func(path string) error) { d.deleter = f }

// SetBatchDeleter wires the bulk soft-delete callback. The bulk variant
// matters for the "delete all newer" flow where dozens of duplicates need
// removing — without it, each path goes through removeOne and pays for a
// separate index DELETE and UI patch.
func (d *DuplicatesView) SetBatchDeleter(f func(paths []string) error) { d.batchDeleter = f }

// removeOne soft-deletes path through the controller (if wired) or falls
// back to a direct unlink, then drops the index row and forgets the thumb.
// Returns true when the on-disk operation succeeded (or the file was
// already gone) so the caller can keep the entry out of pruneMissing's
// "live" list.
func (d *DuplicatesView) removeOne(path string) bool {
	var err error
	if d.deleter != nil {
		err = d.deleter(path)
	} else {
		err = os.Remove(path)
		_ = d.idx.RemoveEntry(path)
		if d.store != nil {
			d.store.Forget(cache.ThumbIDFor(path))
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	return true
}

// Show opens the overlay. If a hashing pass is already running (because a
// previous Close just hid the window) the live state is preserved so the
// user can keep watching its progress; otherwise a fresh pass kicks off.
func (d *DuplicatesView) Show() {
	d.mu.Lock()
	if d.hashing {
		d.Open = true
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	d.startHash()
}

// Rescan cancels any in-flight pass and restarts hashing from scratch. Bound
// to the Rescan button, which is only useful once the user wants to discard
// the current results.
func (d *DuplicatesView) Rescan() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.mu.Unlock()
	d.startHash()
}

// startHash resets duplicate state and launches a fresh hashAndScan goroutine.
func (d *DuplicatesView) startHash() {
	d.mu.Lock()
	d.Open = true
	d.groups = nil
	d.selected = -1
	d.hashing = true
	d.done = 0
	d.total = 0
	d.confirming = false
	d.confirmingAll = false
	d.statusMsg = "Hashing files…"
	// Bump the generation before launching so any still-winding-down
	// goroutine from a prior pass (one Rescan just cancelled) recognises
	// itself as stale and drops its terminal writes instead of overwriting
	// this fresh "Hashing…" state. See hashAndScan's gen checks.
	d.gen++
	gen := d.gen
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.mu.Unlock()
	go func() {
		defer cancel()
		d.hashAndScan(ctx, gen)
	}()
}

// Close hides the overlay. A hashing pass that is still running is left
// running in the background so the user can reopen the overlay later to see
// the result without restarting the work.
func (d *DuplicatesView) Close() {
	d.Open = false
	if d.OnClose != nil {
		d.OnClose()
	}
}

func (d *DuplicatesView) hashAndScan(ctx context.Context, gen int) {
	// Publish to the main-screen process bar so the user can pause /
	// resume / cancel without keeping the modal open.
	var proc *Process
	if d.processes != nil {
		proc = d.processes.Begin(ProcDuplicates, "Duplicates", func() {
			d.mu.Lock()
			c := d.cancel
			d.mu.Unlock()
			if c != nil {
				c()
			}
		}, true)
		proc.SetStatus("Hashing files…")
		d.mu.Lock()
		// Only claim the shared d.proc slot if we're still the current pass —
		// a superseded goroutine must not overwrite the live pass's proc, nor
		// nil it out on the way down (that's why the defer is gen-gated too).
		if d.gen == gen {
			d.proc = proc
		}
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			if d.gen == gen {
				d.proc = nil
			}
			d.mu.Unlock()
			proc.End()
		}()
	}

	err := d.idx.EnsureHashes(ctx, func(done, total int) {
		d.mu.Lock()
		// Drop progress writes from a superseded pass: a Rescan bumps d.gen
		// and starts a new goroutine, but this (old) EnsureHashes may still
		// emit a few progress ticks while tearing down. Writing them would
		// stomp the new pass's freshly-published state. Our own proc updates
		// below stay unconditional — that Process belongs to this goroutine.
		if d.gen == gen {
			d.done = done
			d.total = total
			if total > 0 {
				d.statusMsg = fmt.Sprintf("Hashing %d / %d", done, total)
			} else {
				d.statusMsg = "Nothing to hash"
			}
		}
		d.mu.Unlock()
		if proc != nil {
			// EnsureHashes fires this callback once per hashed candidate — a
			// storm on a large library. proc.SetDone routes through the
			// registry's ~30Hz coalescer (with a trailing flush that still
			// lands the final 100%), so we drop the per-candidate direct
			// d.invalidate() that used to peg a core here. The terminal
			// results below still repaint immediately via d.invalidate().
			proc.SetTotal(int64(total))
			proc.SetDone(int64(done))
		} else if d.invalidate != nil {
			d.invalidate()
		}
	}, func() {
		if proc != nil {
			proc.Wait()
		}
	})
	if err != nil {
		// The error branch runs *before* the ctx.Err() check below, so a
		// Rescan-cancelled pass returns here with "context canceled". Gate the
		// write on gen: otherwise this stale teardown overwrites the new
		// pass's status/hashing flag — the Rescan-clobber bug.
		d.mu.Lock()
		if d.gen == gen {
			d.statusMsg = "Error: " + err.Error()
			d.hashing = false
		}
		d.mu.Unlock()
		if d.invalidate != nil {
			d.invalidate()
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	groups := d.pruneMissing(d.idx.FindDuplicates())
	d.mu.Lock()
	// Publish results only if we're still the current pass.
	if d.gen == gen {
		d.hashing = false
		d.groups = groups
		d.statusMsg = fmt.Sprintf("%d duplicate group(s)", len(groups))
	}
	d.mu.Unlock()
	if d.invalidate != nil {
		d.invalidate()
	}
}

// pruneMissing stats every entry in groups, drops vanished paths from the
// index (and forgets their thumbnails), and returns only groups that still
// have at least two surviving copies. This is what prevents phantom groups
// composed of stale rows from sticking around after files are deleted or
// moved outside the app.
func (d *DuplicatesView) pruneMissing(groups []cache.DuplicateGroup) []cache.DuplicateGroup {
	out := make([]cache.DuplicateGroup, 0, len(groups))
	for _, g := range groups {
		live := make([]cache.Entry, 0, len(g.Entries))
		for _, e := range g.Entries {
			_, err := os.Stat(e.Path)
			if err == nil {
				live = append(live, e)
				continue
			}
			if os.IsNotExist(err) {
				_ = d.idx.RemoveEntry(e.Path)
				if d.store != nil {
					d.store.Forget(cache.ThumbIDFor(e.Path))
				}
				continue
			}
			// Permission errors, transient I/O, etc — keep the entry so a
			// real duplicate isn't hidden behind a flaky filesystem.
			live = append(live, e)
		}
		if len(live) >= 2 {
			g.Entries = live
			out = append(out, g)
		}
	}
	return out
}

// CancelConfirm clears the pending delete confirmation if any. Returns true
// when it actually consumed a confirm prompt — handleKeys uses this to keep
// Esc from closing the whole modal while a confirm is up.
func (d *DuplicatesView) CancelConfirm() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.confirming && !d.confirmingAll {
		return false
	}
	d.confirming = false
	d.confirmingAll = false
	return true
}

// Move adjusts the keyboard selection by delta and clamps to bounds. Returns
// true if the selection changed. Used by j/k navigation in the modal.
func (d *DuplicatesView) Move(delta int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.groups)
	if n == 0 {
		return false
	}
	old := d.selected
	if old < 0 {
		d.selected = 0
	} else {
		d.selected += delta
	}
	if d.selected < 0 {
		d.selected = 0
	}
	if d.selected >= n {
		d.selected = n - 1
	}
	if d.selected != old {
		d.confirming = false
		// Keep the selected row visible. Snap First if the selection moved
		// outside the currently laid-out window.
		first := d.groupList.Position.First
		count := d.groupList.Position.Count
		if d.selected < first || (count > 0 && d.selected >= first+count) {
			d.groupList.Position.First = d.selected
			d.groupList.Position.Offset = 0
		}
		return true
	}
	return false
}

// Activate is the Enter handler for the modal. First press arms the delete
// confirmation; the second press executes it.
func (d *DuplicatesView) Activate() {
	d.mu.Lock()
	if d.selected < 0 || d.selected >= len(d.groups) {
		d.mu.Unlock()
		return
	}
	g := d.groups[d.selected]
	if len(g.Entries) < 2 {
		d.mu.Unlock()
		return
	}
	if d.confirming {
		d.mu.Unlock()
		d.deleteNewer()
		return
	}
	d.confirming = true
	d.mu.Unlock()
}

// deleteNewer removes every entry in the selected group except the oldest.
func (d *DuplicatesView) deleteNewer() {
	d.mu.Lock()
	if d.selected < 0 || d.selected >= len(d.groups) {
		d.mu.Unlock()
		return
	}
	g := d.groups[d.selected]
	d.confirming = false
	d.mu.Unlock()
	if len(g.Entries) < 2 {
		return
	}
	paths := make([]string, 0, len(g.Entries)-1)
	for _, v := range g.Entries[1:] {
		paths = append(paths, v.Path)
	}
	d.removeBatch(paths)
	// Patch the in-memory groups directly instead of re-querying
	// FindDuplicates. The controller does the actual index DELETE on a
	// background goroutine, so an immediate re-query races ahead of it and
	// re-materialises this just-resolved group as a "phantom" from rows that
	// aren't gone yet. We already know exactly which paths we removed, so
	// drop them locally; a real re-query only happens on explicit Rescan.
	d.applyDeletion(paths)
}

// applyDeletion removes the given paths from the in-memory duplicate groups,
// drops any group that falls below two surviving copies, clamps the selection,
// and republishes under the view mutex. This is the race-free alternative to
// re-running FindDuplicates right after a delete: the real index/filesystem
// removal happens asynchronously in the controller, so a re-query could still
// observe the just-deleted rows and rebuild a phantom group. Patching what we
// already know to be gone closes that window entirely.
func (d *DuplicatesView) applyDeletion(paths []string) {
	if len(paths) == 0 {
		return
	}
	deleted := make(map[string]bool, len(paths))
	for _, p := range paths {
		deleted[p] = true
	}
	d.mu.Lock()
	// Fresh backing arrays throughout — Layout snapshots d.groups (and each
	// group's Entries) under the mutex and then reads them lock-free, so a
	// prior frame may still hold the old headers. Never mutate in place.
	next := make([]cache.DuplicateGroup, 0, len(d.groups))
	for _, g := range d.groups {
		kept := make([]cache.Entry, 0, len(g.Entries))
		for _, e := range g.Entries {
			if deleted[e.Path] {
				continue
			}
			kept = append(kept, e)
		}
		// A set that drops to a single (or zero) surviving copy is no longer
		// a duplicate group — leaving it would show as a 1-item phantom.
		if len(kept) >= 2 {
			g.Entries = kept
			next = append(next, g)
		}
	}
	d.groups = next
	d.statusMsg = fmt.Sprintf("%d duplicate group(s)", len(next))
	if d.selected >= len(next) {
		d.selected = -1
	}
	d.mu.Unlock()
	if d.invalidate != nil {
		d.invalidate()
	}
}

// deleteAllNewer removes every entry in every group except the oldest.
func (d *DuplicatesView) deleteAllNewer() {
	d.mu.Lock()
	groups := d.groups
	d.confirmingAll = false
	d.mu.Unlock()

	var paths []string
	for _, g := range groups {
		if len(g.Entries) < 2 {
			continue
		}
		for _, v := range g.Entries[1:] {
			paths = append(paths, v.Path)
		}
	}
	d.removeBatch(paths)
	// In-memory patch rather than a re-query — see deleteNewer/applyDeletion.
	// Removing every newer copy drops each group to its single oldest member,
	// so applyDeletion empties the list; a fresh scan only runs on Rescan.
	d.applyDeletion(paths)
}

// removeBatch routes through batchDeleter when wired so the Controller can
// do one in-memory patch + one index transaction; falls back to per-item
// removeOne otherwise (tests and standalone use of the view).
func (d *DuplicatesView) removeBatch(paths []string) {
	if len(paths) == 0 {
		return
	}
	if d.batchDeleter != nil {
		_ = d.batchDeleter(paths)
		return
	}
	for _, p := range paths {
		d.removeOne(p)
	}
}

// Layout draws the modal over the supplied constraints.
func (d *DuplicatesView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if d.closeBtn.Clicked(gtx) {
		d.Close()
	}
	if d.rescanBtn.Clicked(gtx) {
		d.Rescan()
	}
	if d.deleteBtn.Clicked(gtx) {
		d.mu.Lock()
		d.confirming = true
		d.mu.Unlock()
	}
	if d.cancelBtn.Clicked(gtx) {
		d.mu.Lock()
		d.confirming = false
		d.confirmingAll = false
		d.mu.Unlock()
	}
	if d.confirmBtn.Clicked(gtx) {
		d.deleteNewer()
	}
	if d.acceptAllBtn.Clicked(gtx) {
		d.mu.Lock()
		d.confirmingAll = true
		d.mu.Unlock()
	}
	if d.confirmAllBtn.Clicked(gtx) {
		d.deleteAllNewer()
	}

	d.mu.Lock()
	groups := d.groups
	selected := d.selected
	statusMsg := d.statusMsg
	hashing := d.hashing
	done := d.done
	total := d.total
	confirmingAll := d.confirmingAll
	d.mu.Unlock()

	if cap(d.groupClicks) < len(groups) {
		d.groupClicks = make([]*widget.Clickable, len(groups))
	} else {
		d.groupClicks = d.groupClicks[:len(groups)]
	}
	for i := range groups {
		if d.groupClicks[i] == nil {
			d.groupClicks[i] = &widget.Clickable{}
		}
	}
	for i, c := range d.groupClicks {
		if c.Clicked(gtx) {
			d.mu.Lock()
			if d.selected != i {
				d.confirming = false
			}
			d.selected = i
			d.mu.Unlock()
			selected = i
		}
	}

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y

	// Background fill.
	bg := image.Rectangle{Max: image.Pt(totalW, totalH)}
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	pad := layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				title := material.H6(th.Theme, "Duplicates")
				title.Color = th.Foreground
				return title.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), statusMsg)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if confirmingAll {
					lbl := material.Label(th.Theme, unit.Sp(13), "Delete ALL newer copies? This cannot be undone.")
					lbl.Color = color.NRGBA{R: 0xc0, G: 0x40, B: 0x40, A: 0xff}
					lbl.Font.Weight = 700
					yes := material.Button(th.Theme, &d.confirmAllBtn, "Yes, Delete All")
					yes.Background = color.NRGBA{R: 0xc0, G: 0x40, B: 0x40, A: 0xff}
					no := material.Button(th.Theme, &d.cancelBtn, "Cancel")
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(lbl.Layout),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Rigid(yes.Layout),
						layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(no.Layout),
					)
				}

				close := material.Button(th.Theme, &d.closeBtn, "Close (esc)")
				rescan := material.Button(th.Theme, &d.rescanBtn, "Rescan")
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(close.Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(rescan.Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if len(groups) == 0 {
							return layout.Dimensions{}
						}
						return material.Button(th.Theme, &d.acceptAllBtn, "Delete All Newer").Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if hashing {
					return d.layoutProgress(gtx, th, done, total)
				}
				return d.layoutSplit(gtx, th, groups, selected)
			}),
		)
	})
}

func (d *DuplicatesView) layoutProgress(gtx layout.Context, th *Theme, done, total int) layout.Dimensions {
	// Center the progress display vertically inside the available space —
	// the modal flex makes this a Flexed(1) child, so the parent gives us
	// the full remaining column. Without explicit centering the bar lands
	// at the very top and looks like a stray thin line.
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.X = gtx.Dp(unit.Dp(420))
		gtx.Constraints.Min = image.Point{}
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Hashing files…")
				lbl.Color = th.Foreground
				lbl.Alignment = 0
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				w := gtx.Constraints.Max.X
				barH := gtx.Dp(unit.Dp(14))
				bg := image.Rect(0, 0, w, barH)
				ca := clip.Rect(bg).Push(gtx.Ops)
				paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
				paint.PaintOp{}.Add(gtx.Ops)
				ca.Pop()
				if total > 0 {
					frac := float32(done) / float32(total)
					if frac > 1 {
						frac = 1
					}
					if frac > 0 {
						fg := image.Rect(0, 0, int(float32(w)*frac), barH)
						cf := clip.Rect(fg).Push(gtx.Ops)
						paint.ColorOp{Color: th.Accent}.Add(gtx.Ops)
						paint.PaintOp{}.Add(gtx.Ops)
						cf.Pop()
					}
				}
				return layout.Dimensions{Size: image.Pt(w, barH)}
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				var msg string
				if total <= 0 {
					msg = "Preparing…"
				} else {
					pct := int(float32(done) / float32(total) * 100)
					if pct > 100 {
						pct = 100
					}
					msg = fmt.Sprintf("%d / %d  (%d%%)", done, total, pct)
				}
				lbl := material.Label(th.Theme, unit.Sp(13), msg)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
		)
	})
}

func (d *DuplicatesView) layoutSplit(gtx layout.Context, th *Theme, groups []cache.DuplicateGroup, selected int) layout.Dimensions {
	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	listW := totalW * 35 / 100
	gap := gtx.Dp(unit.Dp(8))
	if listW < gtx.Dp(unit.Dp(260)) {
		listW = gtx.Dp(unit.Dp(260))
	}
	if listW > totalW-gap {
		listW = totalW - gap
	}
	rightW := totalW - listW - gap

	// Left list
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(listW, totalH)
		gtx2.Constraints.Min = image.Pt(listW, totalH)
		d.groupList.Layout(gtx2, len(groups), func(gtx layout.Context, i int) layout.Dimensions {
			return d.layoutGroupRow(gtx, th, groups[i], i == selected, d.groupClicks[i])
		})
	}

	// Right detail
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(rightW, totalH)
		gtx2.Constraints.Min = image.Pt(rightW, totalH)
		stack := op.Offset(image.Pt(listW+gap, 0)).Push(gtx.Ops)
		d.detailList.Layout(gtx2, 1, func(gtx layout.Context, _ int) layout.Dimensions {
			return d.layoutDetail(gtx, th, groups, selected)
		})
		stack.Pop()
	}

	return layout.Dimensions{Size: image.Pt(totalW, totalH)}
}

func (d *DuplicatesView) layoutGroupRow(gtx layout.Context, th *Theme, g cache.DuplicateGroup, active bool, click *widget.Clickable) layout.Dimensions {
	const thumbDp = 48
	thumbPx := gtx.Dp(unit.Dp(thumbDp))
	rowPad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}

	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := rowPad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return d.layoutThumbBox(gtx, th, g.Entries[0], thumbPx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					var size int64
					if len(g.Entries) > 0 {
						size = g.Entries[0].Size
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Label(th.Theme, unit.Sp(13), fmt.Sprintf("%d copies — %s each", len(g.Entries), formatBytesGio(size)))
							lbl.Color = th.Foreground
							lbl.MaxLines = 1
							lbl.Font.Weight = 600
							return lbl.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							name := ""
							if len(g.Entries) > 0 {
								name = shortPath(g.Entries[0].Path)
							}
							lbl := material.Label(th.Theme, unit.Sp(11), name)
							lbl.Color = th.Foreground
							lbl.MaxLines = 1
							return lbl.Layout(gtx)
						}),
					)
				}),
			)
		})
		textCall := macro.Stop()

		w := gtx.Constraints.Max.X
		h := dims.Size.Y
		bg := image.Rect(0, 0, w, h)
		clipArea := clip.Rect(bg).Push(gtx.Ops)
		col := th.Background
		if active {
			col = th.SelectionBG
		}
		paint.ColorOp{Color: col}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		textCall.Add(gtx.Ops)
		pointer.CursorPointer.Add(gtx.Ops)
		clipArea.Pop()
		return layout.Dimensions{Size: image.Pt(w, h)}
	})
}

func (d *DuplicatesView) layoutDetail(gtx layout.Context, th *Theme, groups []cache.DuplicateGroup, selected int) layout.Dimensions {
	if selected < 0 || selected >= len(groups) {
		lbl := material.Label(th.Theme, unit.Sp(13), "Select a group on the left to view details.")
		lbl.Color = th.Foreground
		return lbl.Layout(gtx)
	}
	g := groups[selected]

	d.mu.Lock()
	confirming := d.confirming
	d.mu.Unlock()

	thumbPx := gtx.Dp(unit.Dp(180))
	rows := make([]layout.FlexChild, 0, len(g.Entries)+6)

	header := fmt.Sprintf("%d copies — %s each", len(g.Entries), formatBytesGio(g.Entries[0].Size))
	rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(14), header)
		lbl.Color = th.Foreground
		lbl.Font.Weight = 600
		return lbl.Layout(gtx)
	}))
	rows = append(rows, layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout))

	for i, e := range g.Entries {
		entry := e
		isOldest := i == 0
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return d.layoutEntryCard(gtx, th, entry, isOldest, thumbPx)
		}))
		rows = append(rows, layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout))
	}

	rows = append(rows, layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout))
	if confirming {
		newer := len(g.Entries) - 1
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13),
				fmt.Sprintf("Permanently delete %d newer copies? The oldest one will be kept.", newer))
			lbl.Color = th.Foreground
			lbl.Font.Weight = 600
			return lbl.Layout(gtx)
		}))
		rows = append(rows, layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout))
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			confirm := material.Button(th.Theme, &d.confirmBtn, fmt.Sprintf("Yes, delete %d", newer))
			confirm.Background = color.NRGBA{R: 0xc0, G: 0x40, B: 0x40, A: 0xff}
			cancel := material.Button(th.Theme, &d.cancelBtn, "Cancel")
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(confirm.Layout),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(cancel.Layout),
			)
		}))
	} else if len(g.Entries) >= 2 {
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(th.Theme, &d.deleteBtn, fmt.Sprintf("Delete %d newer (keep oldest)", len(g.Entries)-1))
			return btn.Layout(gtx)
		}))
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

// layoutEntryCard draws a single duplicate-entry row with thumbnail, path,
// mtime, and a "KEEP" tag on the oldest copy.
func (d *DuplicatesView) layoutEntryCard(gtx layout.Context, th *Theme, e cache.Entry, oldest bool, thumbPx int) layout.Dimensions {
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}
	macro := op.Record(gtx.Ops)
	dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return d.layoutThumbBox(gtx, th, e, thumbPx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								tag := "NEWER"
								col := color.NRGBA{R: 0xb0, G: 0x60, B: 0x40, A: 0xff}
								if oldest {
									tag = "KEEP (oldest)"
									col = color.NRGBA{R: 0x40, G: 0x90, B: 0x50, A: 0xff}
								}
								return drawTagPill(gtx, th, tag, col)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Label(th.Theme, unit.Sp(12), e.ModTime.Format("2006-01-02 15:04:05"))
								lbl.Color = th.Foreground
								return lbl.Layout(gtx)
							}),
						)
					}),
					layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), e.Path)
						lbl.Color = th.Foreground
						lbl.MaxLines = 3
						return lbl.Layout(gtx)
					}),
				)
			}),
		)
	})
	call := macro.Stop()

	w := gtx.Constraints.Max.X
	h := dims.Size.Y
	rect := image.Rect(0, 0, w, h)
	ca := clip.Rect(rect).Push(gtx.Ops)
	bg := th.CellBG
	if oldest {
		bg = th.SelectionBG
	}
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	call.Add(gtx.Ops)
	ca.Pop()
	return layout.Dimensions{Size: image.Pt(w, h)}
}

// layoutThumbBox paints a fixed-size thumbnail box for entry e, falling back
// to a placeholder rectangle while the decode is pending or unavailable.
func (d *DuplicatesView) layoutThumbBox(gtx layout.Context, th *Theme, e cache.Entry, sizePx int) layout.Dimensions {
	rect := image.Rect(0, 0, sizePx, sizePx)
	ca := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()
	if d.thumbs != nil {
		if img, sz, ok := d.thumbs.Get(e); ok {
			drawFitted(gtx, img, sz, rect)
		}
	}
	return layout.Dimensions{Size: image.Pt(sizePx, sizePx)}
}

// drawTagPill draws a small colored rectangle with white text inside —
// used for the KEEP / NEWER labels on duplicate entries.
func drawTagPill(gtx layout.Context, th *Theme, text string, bg color.NRGBA) layout.Dimensions {
	pad := layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(6), Right: unit.Dp(6)}
	macro := op.Record(gtx.Ops)
	dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(11), text)
		lbl.Color = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
		lbl.Font.Weight = 700
		return lbl.Layout(gtx)
	})
	call := macro.Stop()
	rect := image.Rect(0, 0, dims.Size.X, dims.Size.Y)
	ca := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	call.Add(gtx.Ops)
	ca.Pop()
	return dims
}

// shortPath trims a path to its trailing component(s) for compact display.
func shortPath(p string) string {
	if len(p) <= 48 {
		return p
	}
	// Keep the last 45 chars plus a leading ellipsis.
	return "…" + p[len(p)-47:]
}

// formatBytesGio is the shared byte-size formatter for the UI overlays
// (duplicates, viewer, settings). Units are binary (1024-based), so the
// suffix is the accurate "KiB"/"MiB"/… rather than the decimal "KB"/"MB".
func formatBytesGio(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
