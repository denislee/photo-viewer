package ui

import (
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Shared building blocks for the Settings sections (and labeledEditor for the
// web-server panel). Each returns a layout.Widget so a section reads as a flat
// list of rows.

// sectionHeader is a section's bold title plus its one-line muted description.
func sectionHeader(th *Theme, title, desc string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(15), title)
				lbl.Color = th.Foreground
				lbl.Font.Weight = font.Bold
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if desc == "" {
					return layout.Dimensions{}
				}
				return layout.Inset{Top: unit.Dp(2)}.Layout(gtx, mutedText(th, desc))
			}),
		)
	}
}

// subHeader titles a block inside a section (e.g. Faces › Grouping).
func subHeader(th *Theme, title string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(12), title)
		lbl.Color = th.Foreground
		lbl.Font.Weight = font.Bold
		return lbl.Layout(gtx)
	}
}

// checkbox is a themed checkbox row.
func checkbox(th *Theme, b *widget.Bool, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		cb := material.CheckBox(th.Theme, b, label)
		cb.Color = th.Foreground
		return cb.Layout(gtx)
	}
}

// bodyText is a 12 sp foreground label.
func bodyText(th *Theme, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(12), s)
		lbl.Color = th.Foreground
		return lbl.Layout(gtx)
	}
}

// mutedText is an 11 sp muted hint line.
func mutedText(th *Theme, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(th.Theme, unit.Sp(11), s)
		lbl.Color = th.Muted
		return lbl.Layout(gtx)
	}
}

// statusLine is a section's status text with an optional button on the
// right (status takes the remaining width). btn may be nil.
func statusLine(th *Theme, msg string, btn *widget.Clickable, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{layout.Flexed(1, bodyText(th, msg))}
		if btn != nil {
			children = append(children,
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(material.Button(th.Theme, btn, label).Layout),
			)
		}
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
	}
}

// vspace is a vertical gap row.
func vspace(dp unit.Dp) layout.FlexChild {
	return layout.Rigid(layout.Spacer{Height: dp}.Layout)
}

// column lays rows out top to bottom.
func column(gtx layout.Context, rows ...layout.FlexChild) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

// labeledEditor is the bold label + single-line editor box used by Settings and
// the web-server panel. browse, when non-nil, adds a "Browse..." button to the
// right of the editor.
func labeledEditor(th *Theme, label string, ed *widget.Editor, hint string, browse *widget.Clickable) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		row := []layout.FlexChild{
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				ed.SingleLine = true
				editor := material.Editor(th.Theme, ed, hint)
				editor.Color = th.Foreground
				editor.HintColor = th.Muted
				return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(8)), editor.Layout)
			}),
		}
		if browse != nil {
			row = append(row,
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(material.Button(th.Theme, browse, "Browse...").Layout),
			)
		}
		return column(gtx,
			layout.Rigid(subHeader(th, label)),
			vspace(4),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, row...)
			}),
		)
	}
}
