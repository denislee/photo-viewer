package ui

import (
	"fmt"
	"image"
	"image/color"
	"slices"
	"strings"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// processBarRowDp is the per-process row height when the bar is expanded.
const (
	processBarRowDp    = 36
	processBarHeaderDp = 26
)

// ProcessBar is the strip at the bottom of the window that lists every
// in-flight background task (import, refresh scan, thumbnail warm-up,
// duplicates hashing, organize) with a progress bar and Pause / Resume /
// Cancel / Open controls per row. Collapsed it shows a one-line summary;
// expanded shows the per-process detail.
type ProcessBar struct {
	registry *ProcessRegistry

	Expanded bool

	// OnOpen is fired when the user clicks the "Open" button on a row.
	// The host wires this to whichever modal corresponds to the kind.
	OnOpen func(kind ProcKind)

	headerBtn widget.Clickable

	// Per-process clickable buttons are keyed by process ID so they
	// persist across frames even as rows shift. Old IDs are pruned
	// at the top of each Layout.
	row map[int64]*processRowButtons
}

type processRowButtons struct {
	pause  widget.Clickable
	cancel widget.Clickable
	open   widget.Clickable
}

func NewProcessBar(reg *ProcessRegistry) *ProcessBar {
	return &ProcessBar{registry: reg, row: map[int64]*processRowButtons{}}
}

// HeightDp returns the bar's current height in dp given the active
// snapshot. Zero when no processes are running.
func (pb *ProcessBar) HeightDp(snap []ProcessSnapshot) int {
	if len(snap) == 0 {
		return 0
	}
	if !pb.Expanded {
		return processBarHeaderDp
	}
	return processBarHeaderDp + len(snap)*processBarRowDp
}

// Prune drops the row buttons of processes no longer in snap. Layout calls
// it, but the window skips Layout entirely while the bar has no height, so
// the window also calls it when snap is empty; otherwise the last rows'
// buttons would never be released (G4-13). snap holds a handful of entries,
// so a linear search beats building a set every frame.
func (pb *ProcessBar) Prune(snap []ProcessSnapshot) {
	for id := range pb.row {
		if !slices.ContainsFunc(snap, func(s ProcessSnapshot) bool { return s.ID == id }) {
			delete(pb.row, id)
		}
	}
}

// Layout draws the bar. Returns its dimensions so the caller knows how
// much vertical space it consumed. Pass an already-taken snapshot so
// height and content agree within a frame.
func (pb *ProcessBar) Layout(gtx layout.Context, th *Theme, snap []ProcessSnapshot) layout.Dimensions {
	pb.Prune(snap)

	for _, s := range snap {
		if _, ok := pb.row[s.ID]; !ok {
			pb.row[s.ID] = &processRowButtons{}
		}
	}

	if pb.headerBtn.Clicked(gtx) {
		pb.Expanded = !pb.Expanded
	}

	// Wire per-row button events.
	for _, s := range snap {
		rb := pb.row[s.ID]
		if rb.pause.Clicked(gtx) {
			p := pb.registry.Get(s.ID)
			if p.IsPaused() {
				p.Resume()
			} else {
				p.Pause()
			}
		}
		if rb.cancel.Clicked(gtx) {
			pb.registry.Get(s.ID).Cancel()
		}
		if rb.open.Clicked(gtx) {
			if pb.OnOpen != nil {
				pb.OnOpen(s.Kind)
			}
		}
	}

	w := gtx.Constraints.Max.X
	headerH := gtx.Dp(unit.Dp(processBarHeaderDp))

	// Background fill for entire bar.
	totalH := pb.HeightDp(snap)
	totalPx := gtx.Dp(unit.Dp(totalH))
	bg := image.Rect(0, 0, w, totalPx)
	paint.FillShape(gtx.Ops, th.CellBG, clip.Rect(bg).Op())

	// Header row (always present).
	{
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(w, headerH))
		pb.layoutHeader(gtx2, th, snap)
	}

	if pb.Expanded {
		rowH := gtx.Dp(unit.Dp(processBarRowDp))
		y := headerH
		for _, s := range snap {
			stack := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
			gtx2 := gtx
			gtx2.Constraints = layout.Exact(image.Pt(w, rowH))
			pb.layoutRow(gtx2, th, s, pb.row[s.ID])
			stack.Pop()
			y += rowH
		}
	}

	return layout.Dimensions{Size: image.Pt(w, totalPx)}
}

func (pb *ProcessBar) layoutHeader(gtx layout.Context, th *Theme, snap []ProcessSnapshot) layout.Dimensions {
	// Build a compact summary: "Import 42% • Refresh 12k • Thumbnails 800/2000"
	chevron := "▶"
	if pb.Expanded {
		chevron = "▼"
	}
	summary := summarizeProcesses(snap)
	return pb.headerBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Left: unit.Dp(10), Right: unit.Dp(10), Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(12), chevron+"  ")
					lbl.Color = th.Foreground
					lbl.Font.Weight = 700
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					title := fmt.Sprintf("%d running:", len(snap))
					lbl := material.Label(th.Theme, unit.Sp(12), title)
					lbl.Color = th.Accent
					lbl.Font.Weight = 700
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(12), summary)
					lbl.Color = th.Foreground
					lbl.MaxLines = 1
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := "click to expand"
					if pb.Expanded {
						hint = "click to collapse"
					}
					lbl := material.Label(th.Theme, unit.Sp(11), hint)
					lbl.Color = th.Muted
					return lbl.Layout(gtx)
				}),
			)
		})
	})
}

func (pb *ProcessBar) layoutRow(gtx layout.Context, th *Theme, s ProcessSnapshot, rb *processRowButtons) layout.Dimensions {
	// Per-row background slightly darker than the bar so rows separate.
	w := gtx.Constraints.Max.X
	h := gtx.Constraints.Max.Y
	bg := image.Rect(0, 0, w, h)
	paint.FillShape(gtx.Ops, th.Background, clip.Rect(bg).Op())

	// Pause/Resume share one branch: both the glyph and the background flip
	// together when the process is paused.
	pauseGlyph, pauseBG := "⏸", th.CellBG
	if s.Paused {
		pauseGlyph, pauseBG = "▶", th.Accent
	}
	countText := processCountText(s) // used twice below; format it once (G4-12)

	// Square icon buttons sized to fit the row height, shared with the
	// toolbar's icon cells.
	btnSide := gtx.Dp(unit.Dp(processBarIconSizeDp))
	btnRadius := gtx.Dp(unit.Dp(4))
	button := func(click *widget.Clickable, glyph string, bg color.NRGBA) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return squareButton(gtx, th, click, btnSide, btnRadius, bg, th.Foreground, nil, glyph, 0, false)
		})
	}

	return layout.Inset{Left: unit.Dp(10), Right: unit.Dp(10), Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			// Title + status (left, fixed width)
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Max.X = gtx.Dp(unit.Dp(170))
				gtx.Constraints.Min.X = gtx.Dp(unit.Dp(170))
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						title := s.Title
						if title == "" {
							title = s.Kind.String()
						}
						if s.Paused {
							title = "⏸  " + title
						}
						lbl := material.Label(th.Theme, unit.Sp(12), title)
						lbl.Color = th.Foreground
						lbl.Font.Weight = 700
						lbl.MaxLines = 1
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						st := s.Status
						if st == "" {
							st = countText
						}
						lbl := material.Label(th.Theme, unit.Sp(10), st)
						lbl.Color = th.Muted
						lbl.MaxLines = 1
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
			// Progress bar fills the middle.
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.Start}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return drawProcessBar(gtx, th, s)
					}),
					layout.Rigid(layout.Spacer{Height: unit.Dp(2)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(10), countText)
						lbl.Color = th.Muted
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			// Square icon buttons sized to fit the row height.
			button(&rb.pause, pauseGlyph, pauseBG),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			button(&rb.cancel, "✕", th.Destructive),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			button(&rb.open, "↗", th.CellBG),
		)
	})
}

// processBarIconSizeDp is the side length of the square icon buttons in the
// process bar. Chosen to fit inside processBarRowDp minus the row's top and
// bottom inset (4dp each) so glyphs aren't clipped.
const processBarIconSizeDp = 26

// drawProcessBar renders a thin progress bar. Indeterminate (Total=0)
// renders as a muted full-width stripe so the user still sees an active
// indicator.
func drawProcessBar(gtx layout.Context, th *Theme, s ProcessSnapshot) layout.Dimensions {
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(unit.Dp(6))
	bg := image.Rect(0, 0, w, barH)
	paint.FillShape(gtx.Ops, th.CellBG, clip.Rect(bg).Op())

	fill := th.Accent
	if s.Paused {
		fill = color.NRGBA{R: 0xb0, G: 0x8a, B: 0x40, A: 0xff}
	}

	if s.Total > 0 {
		frac := float32(s.Done) / float32(s.Total)
		frac = min(frac, 1)
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		paint.FillShape(gtx.Ops, fill, clip.Rect(fg).Op())
	} else {
		// Indeterminate: faint full-width stripe.
		col := fill
		col.A = 0x55
		paint.FillShape(gtx.Ops, col, clip.Rect(bg).Op())
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

// percentOf returns done as a whole percentage of total, clamped to 100. Both
// callers only ask for it when total > 0.
func percentOf(done, total int64) int {
	return min(int(float32(done)/float32(total)*100), 100)
}

func processCountText(s ProcessSnapshot) string {
	if s.Total > 0 {
		return fmt.Sprintf("%d / %d  (%d%%)", s.Done, s.Total, percentOf(s.Done, s.Total))
	}
	if s.Done > 0 {
		return fmt.Sprintf("%d processed", s.Done)
	}
	return "Working…"
}

func summarizeProcesses(snap []ProcessSnapshot) string {
	var b strings.Builder
	for i, s := range snap {
		if i > 0 {
			b.WriteString("   •   ")
		}
		label := s.Kind.String()
		if s.Paused {
			label = "⏸ " + label
		}
		if s.Total > 0 {
			fmt.Fprintf(&b, "%s %d%%", label, percentOf(s.Done, s.Total))
		} else if s.Done > 0 {
			fmt.Fprintf(&b, "%s %d", label, s.Done)
		} else {
			b.WriteString(label)
			b.WriteString(" …")
		}
	}
	return b.String()
}
