package ui

import (
	"image"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// drawToast draws msg as a pill centred at the bottom of area, with the undo
// hint: every people edit shows one ("Moved 3 faces to Ana · Ctrl+Z undo").
func drawToast(gtx layout.Context, th *Theme, msg string, area image.Rectangle) {
	if area.Empty() {
		return
	}
	text := msg + "   ·   Ctrl+Z undo"
	lbl := material.Label(th.Theme, unit.Sp(13), text)
	lbl.Color = th.Foreground
	m := op.Record(gtx.Ops)
	g2 := gtx
	g2.Constraints = layout.Constraints{Max: image.Pt(area.Dx()-gtx.Dp(32), area.Dy())}
	dims := layout.UniformInset(unit.Dp(10)).Layout(g2, lbl.Layout)
	call := m.Stop()
	x := area.Min.X + (area.Dx()-dims.Size.X)/2
	y := area.Max.Y - dims.Size.Y - gtx.Dp(16)
	defer op.Offset(image.Pt(x, y)).Push(gtx.Ops).Pop()
	r := gtx.Dp(8)
	st := clip.RRect{Rect: image.Rectangle{Max: dims.Size}, NE: r, NW: r, SE: r, SW: r}.Push(gtx.Ops)
	paint.ColorOp{Color: th.SelectionBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	st.Pop()
	call.Add(gtx.Ops)
}
