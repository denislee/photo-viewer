package ui

import (
	"image"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

const shortcutBarHeightDp = 22

// shortcutHints returns the list of "key: action" labels for the active pane.
func shortcutHints(viewerOpen, sidebarFocus, selectionMode bool) []string {
	switch {
	case viewerOpen:
		return []string{
			"h/l ←/→: prev/next",
			"j/k ↓/↑: prev/next",
			"f: favorite",
			"d: delete",
			"o: open mpv",
			"esc/q/ctrl+[: close",
		}
	case sidebarFocus:
		return []string{
			"j/k ↓/↑: select dir",
			"ctrl f/b: page",
			"enter: open",
			"l/→: focus grid",
			"esc: cancel",
			"q: quit",
		}
	case selectionMode:
		return []string{
			"h/j/k/l: navigate/select",
			"v: exit selection",
			"enter: open selected",
			"e: export selected",
			"o: open mpv",
			"q: quit",
		}
	default:
		return []string{
			"h/j/k/l: navigate",
			"v: selection mode",
			"enter: open",
			"f: favorite",
			"d: delete",
			"o: open mpv",
			"h at left: focus tree",
			"ctrl k: search",
			"ctrl +/-: zoom",
			"ctrl f/b: page",
			"ctrl i: import",
			"ctrl d: duplicates",
			"q: quit",
		}
	}
}

// drawShortcutBar paints the bottom hint strip with the active pane's
// shortcuts. It assumes gtx.Constraints constrains to the bar's row.
func drawShortcutBar(gtx layout.Context, th *Theme, viewerOpen, sidebarFocus, selectionMode bool) layout.Dimensions {
	w := gtx.Constraints.Max.X
	h := gtx.Constraints.Max.Y

	bg := image.Rect(0, 0, w, h)
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	hints := shortcutHints(viewerOpen, sidebarFocus, selectionMode)
	text := strings.Join(hints, "   ·   ")

	pad := layout.Inset{Left: unit.Dp(10), Right: unit.Dp(10), Top: unit.Dp(3), Bottom: unit.Dp(3)}
	pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(11), text)
		lbl.Color = th.Foreground
		lbl.MaxLines = 1
		return lbl.Layout(gtx)
	})

	return layout.Dimensions{Size: image.Pt(w, h)}
}
