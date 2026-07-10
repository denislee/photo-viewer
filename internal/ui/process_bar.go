package ui

import (
	"fmt"
	"image"
	"image/color"
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

// ProcessBar is the strip below the toolbar that lists every in-flight
// background task (import, refresh scan, thumbnail warm-up, duplicates
// hashing, organize) with a progress bar and Pause / Resume / Cancel /
// Open controls per row. Collapsed it shows a one-line summary; expanded
// shows the per-process detail.
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

// Layout draws the bar. Returns its dimensions so the caller knows how
// much vertical space it consumed. Pass an already-taken snapshot so
// height and content agree within a frame.
func (pb *ProcessBar) Layout(gtx layout.Context, th *Theme, snap []ProcessSnapshot) layout.Dimensions {
	// Prune stale row-button entries. Runs before the early return so that
	// when the last process finishes (snap becomes empty) any leftover row
	// entries are cleaned up rather than accumulating indefinitely.
	live := make(map[int64]bool, len(snap))
	for _, s := range snap {
		live[s.ID] = true
	}
	for id := range pb.row {
		if !live[id] {
			delete(pb.row, id)
		}
	}

	if len(snap) == 0 {
		return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, 0)}
	}

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
			if p := pb.registry.Get(s.ID); p != nil {
				if p.IsPaused() {
					p.Resume()
				} else {
					p.Pause()
				}
			}
		}
		if rb.cancel.Clicked(gtx) {
			if p := pb.registry.Get(s.ID); p != nil {
				p.Cancel()
			}
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
	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	// Header row (always present).
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(w, headerH)
		gtx2.Constraints.Min = image.Pt(w, headerH)
		pb.layoutHeader(gtx2, th, snap)
	}

	if pb.Expanded {
		rowH := gtx.Dp(unit.Dp(processBarRowDp))
		y := headerH
		for _, s := range snap {
			stack := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
			gtx2 := gtx
			gtx2.Constraints.Max = image.Pt(w, rowH)
			gtx2.Constraints.Min = image.Pt(w, rowH)
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
	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	pauseGlyph := "⏸"
	if s.Paused {
		pauseGlyph = "▶"
	}
	pauseBG := th.CellBG
	if s.Paused {
		pauseBG = th.Accent
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
							st = processCountText(s)
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
						lbl := material.Label(th.Theme, unit.Sp(10), processCountText(s))
						lbl.Color = th.Muted
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			// Square icon buttons sized to fit the row height.
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layoutIconButton(gtx, th, &rb.pause, pauseGlyph, pauseBG, th.Foreground)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layoutIconButton(gtx, th, &rb.cancel, "✕", th.Destructive, th.Foreground)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layoutIconButton(gtx, th, &rb.open, "↗", th.CellBG, th.Foreground)
			}),
		)
	})
}

// processBarIconSizeDp is the side length of the square icon buttons in the
// process bar. Chosen to fit inside processBarRowDp minus the row's top and
// bottom inset (4dp each) so glyphs aren't clipped.
const processBarIconSizeDp = 26

// layoutIconButton renders a square, fixed-size clickable cell with a centered
// glyph. Used in the process bar so Pause/Resume/Cancel/Open fit cleanly inside
// the row without the min-size padding that material.Button enforces.
func layoutIconButton(gtx layout.Context, th *Theme, click *widget.Clickable, glyph string, bg, fg color.NRGBA) layout.Dimensions {
	side := gtx.Dp(unit.Dp(processBarIconSizeDp))
	gtx.Constraints.Min = image.Pt(side, side)
	gtx.Constraints.Max = image.Pt(side, side)
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		rect := image.Rectangle{Max: image.Pt(side, side)}
		rr := clip.UniformRRect(rect, gtx.Dp(unit.Dp(4))).Push(gtx.Ops)
		paint.ColorOp{Color: bg}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		rr.Pop()
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13), glyph)
			lbl.Color = fg
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		})
	})
}

// drawProcessBar renders a thin progress bar. Indeterminate (Total=0)
// renders as a muted full-width stripe so the user still sees an active
// indicator.
func drawProcessBar(gtx layout.Context, th *Theme, s ProcessSnapshot) layout.Dimensions {
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(unit.Dp(6))
	bg := image.Rect(0, 0, w, barH)
	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()

	fill := th.Accent
	if s.Paused {
		fill = color.NRGBA{R: 0xb0, G: 0x8a, B: 0x40, A: 0xff}
	}

	if s.Total > 0 {
		frac := float32(s.Done) / float32(s.Total)
		if frac > 1 {
			frac = 1
		}
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		cf := clip.Rect(fg).Push(gtx.Ops)
		paint.ColorOp{Color: fill}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		cf.Pop()
	} else {
		// Indeterminate: faint full-width stripe.
		col := fill
		col.A = 0x55
		cf := clip.Rect(bg).Push(gtx.Ops)
		paint.ColorOp{Color: col}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		cf.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

func processCountText(s ProcessSnapshot) string {
	if s.Total > 0 {
		pct := min(int(float32(s.Done)/float32(s.Total)*100), 100)
		return fmt.Sprintf("%d / %d  (%d%%)", s.Done, s.Total, pct)
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
			pct := min(int(float32(s.Done)/float32(s.Total)*100), 100)
			fmt.Fprintf(&b, "%s %d%%", label, pct)
		} else if s.Done > 0 {
			fmt.Fprintf(&b, "%s %d", label, s.Done)
		} else {
			b.WriteString(label)
			b.WriteString(" …")
		}
	}
	return b.String()
}
