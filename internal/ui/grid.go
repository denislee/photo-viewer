package ui

import (
	"image"
	"image/color"
	"time"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

const (
	defaultCellDp = 160
	cellGapDp     = 6
	selBorderPx   = 3
	minCellDp     = 64
	maxCellDp     = 384
	zoomStepDp    = 24
)

// Grid renders the thumbnail grid in the right pane. It owns the scroll state
// and a per-cell widget.Clickable so clicks are detected reliably alongside
// the list's scroll gestures.
type Grid struct {
	list     widget.List
	cellSize unit.Dp
	cells    []*widget.Clickable
	OnOpen   func(index int)

	// Selection (set by Controller / window key handlers).
	Selected     int
	cols         int // last computed; used for hjkl movement
	viewportRows int // last computed; used for page-up/down jumps

	// pendingScroll is set whenever the selection moves; the next Layout
	// call will scroll just enough to keep the selected row in view.
	pendingScroll bool

	// GPending tracks whether the previous keystroke was a lone 'g', so a
	// follow-up 'g' completes the vim-style "gg" jump-to-top. Window's key
	// handler clears it on any other key.
	GPending bool

	// Confirming is true while the delete-confirmation modal is up over the
	// grid. ConfirmPath is the absolute path captured when the modal opened
	// so it remains stable across refreshes that may reorder entries.
	Confirming  bool
	ConfirmPath string

	// lastSelByDir remembers the selected index for each directory the user
	// has visited so revisiting restores their place. In-memory only — not
	// persisted across launches.
	lastSelByDir map[string]int

	// zoomSaveTimer debounces SaveConfig calls on zoom so rapid
	// scroll-wheel zooming doesn't hammer the config file.
	zoomSaveTimer *time.Timer
}

// RequestDelete opens the delete-confirmation modal for the entry at idx.
// No-op when the index is out of range.
func (g *Grid) RequestDelete(entries []cache.Entry, idx int) {
	if idx < 0 || idx >= len(entries) {
		return
	}
	g.Confirming = true
	g.ConfirmPath = entries[idx].Path
}

// CancelDelete dismisses the confirmation modal without deleting anything.
func (g *Grid) CancelDelete() {
	g.Confirming = false
	g.ConfirmPath = ""
}

// ConfirmDelete invokes deleter against the captured path. The grid's
// selection is left clamped by the next Layout pass via SelectedIndex.
func (g *Grid) ConfirmDelete(deleter func(path string) error) {
	path := g.ConfirmPath
	g.Confirming = false
	g.ConfirmPath = ""
	if path == "" {
		return
	}
	_ = deleter(path)
}

func NewGrid() *Grid {
	size := unit.Dp(defaultCellDp)
	if v := GetConfig().GridCellDp; v > 0 {
		size = unit.Dp(v)
		if size < minCellDp {
			size = minCellDp
		}
		if size > maxCellDp {
			size = maxCellDp
		}
	}
	g := &Grid{cellSize: size, lastSelByDir: map[string]int{}}
	g.list.Axis = layout.Vertical
	return g
}

// gridSelMemCap is the cap on lastSelByDir. Beyond this the oldest-by-insertion
// entries are dropped so a deep tree of browsed directories doesn't grow the
// map unboundedly across a long session.
const gridSelMemCap = 512

// RememberFor stores the current selection index against dir so a later
// RestoreFor(dir) lands the user back where they left off. No-op when dir
// is empty.
func (g *Grid) RememberFor(dir string) {
	if dir == "" {
		return
	}
	if g.lastSelByDir == nil {
		g.lastSelByDir = map[string]int{}
	}
	if _, ok := g.lastSelByDir[dir]; !ok && len(g.lastSelByDir) >= gridSelMemCap {
		// Drop one arbitrary entry. Map iteration order is randomized, so
		// this is effectively random eviction — cheap and good enough for
		// what is just a UX nicety. Full LRU isn't worth the bookkeeping.
		for k := range g.lastSelByDir {
			delete(g.lastSelByDir, k)
			break
		}
	}
	g.lastSelByDir[dir] = g.Selected
}

// RestoreFor sets Selected to the previously remembered index for dir, or 0
// if none. A pending scroll is queued so the next Layout brings it into view.
func (g *Grid) RestoreFor(dir string) {
	idx := 0
	if g.lastSelByDir != nil {
		if v, ok := g.lastSelByDir[dir]; ok {
			idx = v
		}
	}
	g.Selected = idx
	g.pendingScroll = true
}

func (g *Grid) ensureCells(n int) {
	if cap(g.cells) < n {
		// Grow with ~20% slack so repeated viewport stretches and scroll
		// past the end of large lists don't trigger a realloc on every
		// frame. The minimum bump keeps the very-small case sane.
		grown := max(n+n/5, n+8)
		next := make([]*widget.Clickable, n, grown)
		copy(next, g.cells)
		g.cells = next
	} else {
		g.cells = g.cells[:n]
	}
	// Clickables are lazily allocated by cellAt — off-screen cells (the
	// 99% case on a 100k-entry library) stay nil.
}

// cellAt returns the Clickable for index i, allocating on first access. Safe
// to call only from the layout goroutine (Grid has no internal locking).
func (g *Grid) cellAt(i int) *widget.Clickable {
	if g.cells[i] == nil {
		g.cells[i] = &widget.Clickable{}
	}
	return g.cells[i]
}

// Cols returns the most recently computed column count. Used by hjkl handlers
// to compute up/down moves.
func (g *Grid) Cols() int {
	if g.cols < 1 {
		return 1
	}
	return g.cols
}

// Move adjusts the selection by dx columns and dy rows, clamping to bounds.
// Returns true if the selection moved (so the caller can invalidate).
func (g *Grid) Move(dx, dy, total int) bool {
	if total == 0 {
		return false
	}
	cols := g.Cols()
	old := g.Selected
	row := old / cols
	col := old % cols
	col += dx
	row += dy
	if col < 0 {
		col = 0
	}
	if row < 0 {
		row = 0
	}
	idx := row*cols + col
	if idx >= total {
		idx = total - 1
	}
	if idx == old {
		return false
	}
	g.Selected = idx
	g.pendingScroll = true
	return true
}

// Zoom adjusts cell size by one step. dir>0 zooms in (larger cells), dir<0
// zooms out. Clamped to [minCellDp, maxCellDp].
func (g *Grid) Zoom(dir int) bool {
	old := g.cellSize
	if dir > 0 {
		g.cellSize += zoomStepDp
	} else if dir < 0 {
		g.cellSize -= zoomStepDp
	}
	if g.cellSize < minCellDp {
		g.cellSize = minCellDp
	}
	if g.cellSize > maxCellDp {
		g.cellSize = maxCellDp
	}
	if g.cellSize != old {
		if g.zoomSaveTimer != nil {
			g.zoomSaveTimer.Stop()
		}
		sz := g.cellSize
		g.zoomSaveTimer = time.AfterFunc(500*time.Millisecond, func() {
			c := GetConfig()
			c.GridCellDp = int(sz)
			_ = SaveConfig(c)
		})
		return true
	}
	return false
}

// PageMove jumps the selection by one viewport-worth of rows. dir>0 moves
// down, dir<0 moves up. The viewport row count is computed from the last
// drawn list size; if unknown, falls back to 4 rows.
func (g *Grid) PageMove(dir, total int) bool {
	rows := g.viewportRows
	if rows < 1 {
		rows = 4
	}
	var moved bool
	if dir > 0 {
		moved = g.Move(0, rows, total)
	} else {
		moved = g.Move(0, -rows, total)
	}
	if moved {
		g.list.ScrollTo(g.Selected / g.Cols())
	}
	return moved
}

// JumpTop moves the selection to the very first cell. Returns true if it
// changed.
func (g *Grid) JumpTop(total int) bool {
	if total == 0 || g.Selected == 0 {
		return false
	}
	g.Selected = 0
	g.pendingScroll = true
	return true
}

// JumpBottom moves the selection to the very last cell.
func (g *Grid) JumpBottom(total int) bool {
	if total == 0 || g.Selected == total-1 {
		return false
	}
	g.Selected = total - 1
	g.pendingScroll = true
	return true
}

// SelectedIndex clamps and returns the current selection.
func (g *Grid) SelectedIndex(total int) int {
	if g.Selected < 0 {
		g.Selected = 0
	}
	if g.Selected >= total && total > 0 {
		g.Selected = total - 1
	}
	return g.Selected
}

// Layout draws the grid for the supplied entries.
func (g *Grid) Layout(gtx layout.Context, th *Theme, entries []cache.Entry, ctrl *Controller) layout.Dimensions {
	g.ensureCells(len(entries))

	// Drain click events for the cells that were drawn last frame. Cells
	// outside the visible window can't have received new pointer events
	// (they weren't laid out), so polling them is wasted work — and on
	// large grids that's tens of thousands of Clicked() calls per frame.
	first, count := g.list.Position.First, g.list.Position.Count
	cols := max(g.cols, 1)
	clickStart := max(first*cols, 0)
	clickEnd := min((first+count+1)*cols, len(g.cells))
	for i := clickStart; i < clickEnd; i++ {
		if g.cells[i] == nil {
			continue
		}
		if g.cells[i].Clicked(gtx) {
			g.Selected = i
			if ctrl.SelectionMode {
				ctrl.ToggleSelection(entries[i].Path)
			} else if g.OnOpen != nil {
				g.OnOpen(i)
			}
		}
	}

	cellPx := gtx.Dp(g.cellSize)
	gapPx := gtx.Dp(cellGapDp)
	width := gtx.Constraints.Max.X
	cols = max((width+gapPx)/(cellPx+gapPx), 1)
	g.cols = cols
	rowH := cellPx + gapPx
	if rowH > 0 {
		g.viewportRows = gtx.Constraints.Max.Y / rowH
	}
	// Keep the thumbnail cache large enough to hold everything on screen plus a
	// couple of scroll buffer rows. On a big display zoomed all the way out the
	// visible cell count can exceed the default cap; without this the cache
	// would evict and re-decode cells every frame.
	ctrl.Thumbs().EnsureCapacity(cols * (g.viewportRows + 4))
	rowCount := (len(entries) + cols - 1) / cols
	g.SelectedIndex(len(entries))

	if g.pendingScroll && rowCount > 0 {
		const margin = 1
		selRow := g.Selected / cols
		first := g.list.Position.First
		count := g.list.Position.Count
		switch {
		case count == 0:
			// First frame — defer until we know the viewport.
		case selRow-margin < first:
			g.list.ScrollTo(max(selRow-margin, 0))
			g.pendingScroll = false
		case selRow+margin >= first+count:
			// ScrollTo aligns the row to the top; bias by the visible row
			// count so the selection lands one row above the bottom edge.
			target := max(selRow+margin-count+1, 0)
			g.list.ScrollTo(target)
			g.pendingScroll = false
		default:
			g.pendingScroll = false
		}
	}

	return g.list.Layout(gtx, rowCount, func(gtx layout.Context, row int) layout.Dimensions {
		return g.layoutRow(gtx, th, entries, ctrl, row, cols, cellPx, gapPx)
	})
}

func (g *Grid) layoutRow(gtx layout.Context, th *Theme, entries []cache.Entry, ctrl *Controller, row, cols, cellPx, gapPx int) layout.Dimensions {
	start := row * cols
	end := min(start+cols, len(entries))
	// Hoist loop-invariants so the inner loop only does per-cell work:
	// offset and the IsSelected map lookup. cellSize is fixed for the row,
	// as are the thumb cache, selection mode, and stride.
	rowH := cellPx + gapPx
	stride := cellPx + gapPx
	yOff := gapPx / 2
	cellSize := image.Pt(cellPx, cellPx)
	cellGtx := gtx
	cellGtx.Constraints.Max = cellSize
	cellGtx.Constraints.Min = cellSize
	thumbs := ctrl.Thumbs()
	selectionMode := ctrl.SelectionMode
	selected := g.Selected
	for i := start; i < end; i++ {
		x := (i - start) * stride
		stack := op.Offset(image.Pt(x, yOff)).Push(gtx.Ops)
		e := entries[i]
		isSelected := selectionMode && ctrl.IsSelected(e.Path)
		drawCell(cellGtx, th, e, thumbs, g.cellAt(i), cellPx, i == selected, isSelected, selectionMode)
		stack.Pop()
	}
	return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, rowH)}
}

func drawCell(gtx layout.Context, th *Theme, e cache.Entry, tc *thumbCache, click *widget.Clickable, sizePx int, focused, selected, selectionMode bool) layout.Dimensions {
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		rect := image.Rectangle{Max: image.Pt(sizePx, sizePx)}
		clipArea := clip.Rect(rect).Push(gtx.Ops)
		// background
		paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)

		opImg, imgSz, ok := tc.Get(e)
		if ok {
			drawFitted(gtx, opImg, imgSz, rect)
		}

		if selectionMode && selected {
			// Dim the cell if selected in multi-selection mode.
			paint.ColorOp{Color: color.NRGBA{A: 0x60}}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
		}

		clipArea.Pop()

		if e.Favorite {
			drawFavoriteBadge(gtx, th, rect)
		}
		if focused {
			drawBorder(gtx, rect, selBorderPx, th.Accent)
		} else if selectionMode && selected {
			drawBorder(gtx, rect, selBorderPx, th.Muted)
		}
		if selectionMode && selected {
			drawSelectionBadge(gtx, th, rect)
		}
		if e.Type == scan.TypeVideo {
			drawVideoBadge(gtx, th, rect, e.DurationMs)
		}

		return layout.Dimensions{Size: rect.Max}
	})
}

// favoriteGold is the badge fill colour used to mark a favorite. Hard-coded
// rather than added to the theme because it should remain readable on top of
// arbitrary thumbnail content.
var (
	favoriteGold       = color.NRGBA{R: 0xff, G: 0xc8, B: 0x3d, A: 0xff}
	favoriteShadow     = color.NRGBA{R: 0, G: 0, B: 0, A: 0xb0}
	favoriteBadgeSize  = unit.Dp(26)
	favoriteBadgeInset = unit.Dp(6)
)

// drawFavoriteBadge renders a gold ★ in the top-right corner of cell. The
// glyph is centered in a dark rounded square so it stays legible over any
// thumbnail content.
func drawFavoriteBadge(gtx layout.Context, th *Theme, cell image.Rectangle) {
	pad := gtx.Dp(favoriteBadgeInset)
	size := gtx.Dp(favoriteBadgeSize)
	x1 := cell.Max.X - pad
	y0 := cell.Min.Y + pad
	x0 := x1 - size
	y1 := y0 + size
	bg := image.Rect(x0, y0, x1, y1)

	// Dark backdrop so the gold star reads on bright thumbs.
	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: favoriteShadow}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	// Star glyph, sized to roughly fill the badge.
	stack := op.Offset(image.Pt(x0, y0)).Push(gtx.Ops)
	gtx2 := gtx
	gtx2.Constraints.Max = image.Pt(size, size)
	gtx2.Constraints.Min = image.Pt(size, size)
	layout.Center.Layout(gtx2, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(20), "★")
		lbl.Color = favoriteGold
		return lbl.Layout(gtx)
	})
	stack.Pop()
}

func drawSelectionBadge(gtx layout.Context, th *Theme, cell image.Rectangle) {
	pad := gtx.Dp(favoriteBadgeInset)
	size := gtx.Dp(favoriteBadgeSize)
	x0 := cell.Min.X + pad
	y0 := cell.Min.Y + pad
	x1 := x0 + size
	y1 := y0 + size
	bg := image.Rect(x0, y0, x1, y1)

	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: favoriteShadow}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	stack := op.Offset(image.Pt(x0, y0)).Push(gtx.Ops)
	gtx2 := gtx
	gtx2.Constraints.Max = image.Pt(size, size)
	gtx2.Constraints.Min = image.Pt(size, size)
	layout.Center.Layout(gtx2, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(16), "✓")
		lbl.Color = th.Accent
		return lbl.Layout(gtx)
	})
	stack.Pop()
}

func drawVideoBadge(gtx layout.Context, th *Theme, cell image.Rectangle, durationMs int64) {
	pad := gtx.Dp(favoriteBadgeInset)
	size := gtx.Dp(favoriteBadgeSize)
	x1 := cell.Max.X - pad
	y1 := cell.Max.Y - pad
	x0 := x1 - size
	y0 := y1 - size
	bg := image.Rect(x0, y0, x1, y1)

	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: favoriteShadow}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	stack := op.Offset(image.Pt(x0, y0)).Push(gtx.Ops)
	gtx2 := gtx
	gtx2.Constraints.Max = image.Pt(size, size)
	gtx2.Constraints.Min = image.Pt(size, size)
	layout.Center.Layout(gtx2, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(16), "▶")
		lbl.Color = th.Foreground
		return lbl.Layout(gtx)
	})
	stack.Pop()

	if durationMs > 0 {
		drawDurationBadge(gtx, th, cell, x0, y1, formatDuration(durationMs))
	}
}

// drawDurationBadge renders a small "MM:SS" pill in the bottom-left of the
// cell, immediately to the left of the ▶ play badge. badgeLeftX is the left
// edge of the play badge so the duration pill butts up against it with a
// small gap. bottomY is the bottom edge shared with the play badge.
func drawDurationBadge(gtx layout.Context, th *Theme, cell image.Rectangle, badgeLeftX, bottomY int, text string) {
	pad := gtx.Dp(favoriteBadgeInset)
	size := gtx.Dp(favoriteBadgeSize)
	hGap := gtx.Dp(unit.Dp(4))

	// Measure label first so the pill hugs the text width.
	macro := op.Record(gtx.Ops)
	mgtx := gtx
	mgtx.Constraints.Min = image.Point{}
	mgtx.Constraints.Max.X = cell.Dx()
	lbl := material.Label(th.Theme, unit.Sp(11), text)
	lbl.Color = th.Foreground
	lbl.MaxLines = 1
	lblDims := lbl.Layout(mgtx)
	labelCall := macro.Stop()

	padX := gtx.Dp(unit.Dp(6))
	pillH := size
	pillW := lblDims.Size.X + padX*2

	x1 := badgeLeftX - hGap
	x0 := x1 - pillW
	if x0 < cell.Min.X+pad {
		// Not enough room in the cell — fall back to drawing inside the play
		// badge column. Shrink rather than overlap.
		x0 = cell.Min.X + pad
		if x1 <= x0 {
			x1 = x0 + pillW
		}
	}
	y1 := bottomY
	y0 := y1 - pillH
	rect := image.Rect(x0, y0, x1, y1)

	ca := clip.UniformRRect(rect, gtx.Dp(unit.Dp(4))).Push(gtx.Ops)
	paint.ColorOp{Color: favoriteShadow}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	textY := max(y0+(pillH-lblDims.Size.Y)/2, y0)
	stack := op.Offset(image.Pt(x0+padX, textY)).Push(gtx.Ops)
	labelCall.Add(gtx.Ops)
	stack.Pop()
}

// formatDuration renders milliseconds as M:SS, MM:SS, or H:MM:SS.
func formatDuration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	total := ms / 1000
	if ms%1000 >= 500 {
		total++
	}
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmtDur3(h, m, s)
	}
	return fmtDur2(m, s)
}

func fmtDur2(m, s int64) string {
	buf := [5]byte{'0', '0', ':', '0', '0'}
	buf[0] = byte('0' + m/10)
	buf[1] = byte('0' + m%10)
	buf[3] = byte('0' + s/10)
	buf[4] = byte('0' + s%10)
	if m < 10 {
		return string(buf[1:])
	}
	return string(buf[:])
}

func fmtDur3(h, m, s int64) string {
	// H:MM:SS — hours can be multi-digit for very long files.
	hs := []byte{}
	if h == 0 {
		hs = append(hs, '0')
	} else {
		for h > 0 {
			hs = append([]byte{byte('0' + h%10)}, hs...)
			h /= 10
		}
	}
	out := append(hs, ':')
	out = append(out, byte('0'+m/10), byte('0'+m%10), ':', byte('0'+s/10), byte('0'+s%10))
	return string(out)
}

// drawBorder strokes a rectangular border `width` px thick on the inside of
// rect using four filled rectangles. Used to highlight the keyboard-selected cell.
func drawBorder(gtx layout.Context, rect image.Rectangle, width int, col color.NRGBA) {
	stroke := func(r image.Rectangle) {
		ca := clip.Rect(r).Push(gtx.Ops)
		paint.ColorOp{Color: col}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		ca.Pop()
	}
	stroke(image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+width))
	stroke(image.Rect(rect.Min.X, rect.Max.Y-width, rect.Max.X, rect.Max.Y))
	stroke(image.Rect(rect.Min.X, rect.Min.Y+width, rect.Min.X+width, rect.Max.Y-width))
	stroke(image.Rect(rect.Max.X-width, rect.Min.Y+width, rect.Max.X, rect.Max.Y-width))
}

// drawFitted scales an image op to fit inside dst (preserving aspect ratio)
// and centers it.
func drawFitted(gtx layout.Context, img paint.ImageOp, imgSz image.Point, dst image.Rectangle) {
	dstW := dst.Dx()
	dstH := dst.Dy()
	if imgSz.X == 0 || imgSz.Y == 0 || dstW == 0 || dstH == 0 {
		return
	}
	sx := float32(dstW) / float32(imgSz.X)
	sy := float32(dstH) / float32(imgSz.Y)
	s := sx
	if sy < s {
		s = sy
	}
	drawnW := int(float32(imgSz.X) * s)
	drawnH := int(float32(imgSz.Y) * s)
	offX := dst.Min.X + (dstW-drawnW)/2
	offY := dst.Min.Y + (dstH-drawnH)/2

	defer op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(s, s)).Offset(f32.Pt(float32(offX), float32(offY)))).Push(gtx.Ops).Pop()
	img.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}
