package ui

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// drawBackground fills the available constraint area with a flat color.
func drawBackground(gtx layout.Context, c color.NRGBA) {
	rect := image.Rectangle{Max: gtx.Constraints.Max}
	paint.FillShape(gtx.Ops, c, clip.Rect(rect).Op())
}

// drawEditorBox renders an editor inside a tinted rectangle that fills the
// caller's available width. The editor is laid out into a recorded macro so
// the background can be sized to the editor's actual height (plus padding)
// instead of guessing a fixed pixel value.
func drawEditorBox(gtx layout.Context, bg color.NRGBA, pad layout.Inset, w layout.Widget) layout.Dimensions {
	w2 := gtx.Constraints.Max.X
	if w2 == 0 {
		w2 = gtx.Constraints.Min.X
	}
	w2 = max(w2, 200)
	cgtx := gtx
	cgtx.Constraints.Max.X = w2
	cgtx.Constraints.Min.X = w2
	cgtx.Constraints.Min.Y = 0

	macro := op.Record(gtx.Ops)
	dims := pad.Layout(cgtx, w)
	call := macro.Stop()

	rect := image.Rectangle{Max: image.Pt(w2, dims.Size.Y)}
	paint.FillShape(gtx.Ops, bg, clip.Rect(rect).Op())
	call.Add(gtx.Ops)
	return layout.Dimensions{Size: image.Pt(w2, dims.Size.Y)}
}
