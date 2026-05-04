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

	mu        sync.Mutex
	hashing   bool
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

// Show resets state and starts a hashing pass in the background.
func (d *DuplicatesView) Show() {
	d.Open = true
	d.mu.Lock()
	d.groups = nil
	d.selected = -1
	d.hashing = true
	d.done = 0
	d.total = 0
	d.confirming = false
	d.confirmingAll = false
	d.statusMsg = "Hashing files…"
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go d.hashAndScan(ctx)
}

// Close hides the overlay and cancels any in-flight hashing pass.
func (d *DuplicatesView) Close() {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.Open = false
	if d.OnClose != nil {
		d.OnClose()
	}
}

func (d *DuplicatesView) hashAndScan(ctx context.Context) {
	err := d.idx.EnsureHashes(ctx, func(done, total int) {
		d.mu.Lock()
		d.done = done
		d.total = total
		if total > 0 {
			d.statusMsg = fmt.Sprintf("Hashing %d / %d", done, total)
		} else {
			d.statusMsg = "Nothing to hash"
		}
		d.mu.Unlock()
		if d.invalidate != nil {
			d.invalidate()
		}
	})
	if err != nil {
		d.mu.Lock()
		d.statusMsg = "Error: " + err.Error()
		d.hashing = false
		d.mu.Unlock()
		if d.invalidate != nil {
			d.invalidate()
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	groups := d.idx.FindDuplicates()
	d.mu.Lock()
	d.hashing = false
	d.groups = groups
	d.statusMsg = fmt.Sprintf("%d duplicate group(s)", len(groups))
	d.mu.Unlock()
	if d.invalidate != nil {
		d.invalidate()
	}
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
	for _, v := range g.Entries[1:] {
		if err := os.Remove(v.Path); err != nil {
			continue
		}
		_ = d.idx.RemoveEntry(v.Path)
		if d.store != nil {
			d.store.Forget(cache.ThumbIDFor(v.Path))
		}
	}
	groups := d.idx.FindDuplicates()
	d.mu.Lock()
	d.groups = groups
	d.statusMsg = fmt.Sprintf("%d duplicate group(s)", len(groups))
	if d.selected >= len(groups) {
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

	for _, g := range groups {
		if len(g.Entries) < 2 {
			continue
		}
		for _, v := range g.Entries[1:] {
			if err := os.Remove(v.Path); err != nil {
				continue
			}
			_ = d.idx.RemoveEntry(v.Path)
			if d.store != nil {
				d.store.Forget(cache.ThumbIDFor(v.Path))
			}
		}
	}
	newGroups := d.idx.FindDuplicates()
	d.mu.Lock()
	d.groups = newGroups
	d.statusMsg = fmt.Sprintf("%d duplicate group(s)", len(newGroups))
	d.selected = -1
	d.mu.Unlock()
	if d.invalidate != nil {
		d.invalidate()
	}
}

// Layout draws the modal over the supplied constraints.
func (d *DuplicatesView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if d.closeBtn.Clicked(gtx) {
		d.Close()
	}
	if d.rescanBtn.Clicked(gtx) {
		d.Show()
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

// formatBytesGio is the shared byte-size formatter for the duplicates and
// import overlays.
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
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
