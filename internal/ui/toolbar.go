package ui

import (
	"fmt"
	"image"

	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const (
	toolbarHeightDp    = 48
	toolbarIconCellDp  = 34
	toolbarTooltipPadH = 8
	toolbarTooltipPadV = 4
)

// Toolbar is the slim top bar above the main split. Shows the active path,
// the entry count, and an icon-button row that mirrors the Fyne build's
// actions (filter, RAW toggle, import, duplicates, settings, rebuild).
type Toolbar struct {
	allBtn        widget.Clickable
	photosBtn     widget.Clickable
	videosBtn     widget.Clickable
	rawBtn        widget.Clickable
	yearsBtn      widget.Clickable
	sortDurBtn    widget.Clickable
	importBtn     widget.Clickable
	duplicatesBtn widget.Clickable
	settingsBtn   widget.Clickable
	organizeBtn   widget.Clickable
	rebuildBtn    widget.Clickable
	warmUpBtn     widget.Clickable

	OnFilter        func(string)
	OnShowRAW       func(bool)
	OnGroupByYear   func(bool)
	OnSortByLength  func(bool)
	OnImport      func()
	OnDuplicates  func()
	OnOrganize    func()
	OnSettings    func()
	OnRebuild     func()
	OnWarmUp      func()

	// State mirrored from the controller; the active filter button is
	// rendered with the accent background so it stands out.
	Filter        string
	ShowRAW       bool
	GroupByYear   bool
	SortByLength  bool
	// AnyProcess is true while the process bar has at least one in-flight
	// task. Used to disable buttons (Rebuild) that would conflict with an
	// already-running job.
	AnyProcess bool

	// tooltipText is the label of whichever icon button the pointer is
	// hovering over this frame, or empty when nothing is hovered. Read by
	// the window code so the tooltip can be drawn as an overlay below the
	// toolbar (which would otherwise be clipped by the body region).
	tooltipText string
}

func NewToolbar() *Toolbar { return &Toolbar{Filter: "All"} }

// HoveredLabel returns the tooltip text for the currently hovered toolbar
// button, or "" when nothing is hovered.
func (t *Toolbar) HoveredLabel() string { return t.tooltipText }

// Layout draws the toolbar background, labels, and icon buttons. Drains
// click events on each call so callbacks fire on the same frame.
func (t *Toolbar) Layout(gtx layout.Context, th *Theme, path string, count int) layout.Dimensions {
	if t.allBtn.Clicked(gtx) {
		t.Filter = "All"
		if t.OnFilter != nil {
			t.OnFilter("All")
		}
	}
	if t.photosBtn.Clicked(gtx) {
		t.Filter = "Photos"
		if t.OnFilter != nil {
			t.OnFilter("Photos")
		}
	}
	if t.videosBtn.Clicked(gtx) {
		t.Filter = "Videos"
		if t.OnFilter != nil {
			t.OnFilter("Videos")
		}
	}
	if t.rawBtn.Clicked(gtx) {
		t.ShowRAW = !t.ShowRAW
		if t.OnShowRAW != nil {
			t.OnShowRAW(t.ShowRAW)
		}
	}
	if t.yearsBtn.Clicked(gtx) {
		t.GroupByYear = !t.GroupByYear
		if t.OnGroupByYear != nil {
			t.OnGroupByYear(t.GroupByYear)
		}
	}
	if t.sortDurBtn.Clicked(gtx) {
		t.SortByLength = !t.SortByLength
		if t.OnSortByLength != nil {
			t.OnSortByLength(t.SortByLength)
		}
	}
	if t.importBtn.Clicked(gtx) && t.OnImport != nil {
		t.OnImport()
	}
	if t.duplicatesBtn.Clicked(gtx) && t.OnDuplicates != nil {
		t.OnDuplicates()
	}
	if t.organizeBtn.Clicked(gtx) && t.OnOrganize != nil {
		t.OnOrganize()
	}
	if t.settingsBtn.Clicked(gtx) && t.OnSettings != nil {
		t.OnSettings()
	}
	if t.rebuildBtn.Clicked(gtx) && t.OnRebuild != nil && !t.AnyProcess {
		t.OnRebuild()
	}
	if t.warmUpBtn.Clicked(gtx) && t.OnWarmUp != nil {
		t.OnWarmUp()
	}

	h := gtx.Dp(unit.Dp(toolbarHeightDp))
	w := gtx.Constraints.Max.X

	bg := image.Rect(0, 0, w, h)
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	// Reset the hover tooltip; each button below sets it if hovered.
	t.tooltipText = ""

	icon := func(btn *widget.Clickable, ic *widget.Icon, label string, active, disabled bool) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			dims := t.layoutIconCell(gtx, th, btn, ic, active, disabled)
			if btn.Hovered() {
				t.tooltipText = label
			}
			return dims
		})
	}

	pad := layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(12), Right: unit.Dp(12)}
	pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			// Context: path + count. Min.Y is cleared so the labels report
			// their natural height — otherwise widget.Label inflates dims to
			// the row min and draws glyphs at the top of that inflated box,
			// which looks top-heavy. With natural height the Flex's Middle
			// alignment vertically centers the text.
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = 0
				lbl := material.Label(th.Theme, unit.Sp(13), path)
				lbl.Color = th.Foreground
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.Y = 0
				lbl := material.Label(th.Theme, unit.Sp(12), fmt.Sprintf("%d items", count))
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),

			groupSep(th),

			// Filter group: segmented All / Photos / Videos.
			icon(&t.allBtn, th.Icons.All, "All", t.Filter == "All", false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.photosBtn, th.Icons.Photos, "Photos", t.Filter == "Photos", false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.videosBtn, th.Icons.Videos, "Videos", t.Filter == "Videos", false),

			groupSep(th),

			// Display toggles.
			icon(&t.rawBtn, th.Icons.RAW, rawTooltip(t.ShowRAW), t.ShowRAW, false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.yearsBtn, th.Icons.Years, yearsTooltip(t.GroupByYear), t.GroupByYear, false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.sortDurBtn, th.Icons.SortDur, sortDurTooltip(t.SortByLength), t.SortByLength, false),

			groupSep(th),

			// Library actions.
			icon(&t.importBtn, th.Icons.Import, "Import", false, false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.duplicatesBtn, th.Icons.Duplicates, "Find duplicates", false, false),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.organizeBtn, th.Icons.Organize, "Organize", false, false),

			groupSep(th),

			// Index maintenance.
			icon(&t.rebuildBtn, th.Icons.Rebuild, "Rebuild index", false, t.AnyProcess),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			icon(&t.warmUpBtn, th.Icons.WarmUp, "Warm up thumbnails", false, false),

			groupSep(th),

			// Settings, anchored at the far right.
			icon(&t.settingsBtn, th.Icons.Settings, "Settings", false, false),
		)
	})

	return layout.Dimensions{Size: image.Pt(w, h)}
}

// layoutIconCell renders a single square icon button with a hover/active
// background. The clickable consumes pointer events for click and hover.
func (t *Toolbar) layoutIconCell(gtx layout.Context, th *Theme, btn *widget.Clickable, ic *widget.Icon, active, disabled bool) layout.Dimensions {
	side := gtx.Dp(unit.Dp(toolbarIconCellDp))
	gtx.Constraints.Min = image.Pt(side, side)
	gtx.Constraints.Max = image.Pt(side, side)

	fg := th.Foreground
	bg := th.CellBG
	switch {
	case disabled:
		fg = th.Muted
	case active:
		bg = th.Accent
	case btn.Hovered():
		bg = th.SelectionBG
	}

	return btn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		rect := image.Rectangle{Max: image.Pt(side, side)}
		rr := clip.UniformRRect(rect, gtx.Dp(unit.Dp(5))).Push(gtx.Ops)
		paint.ColorOp{Color: bg}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		rr.Pop()

		if !disabled {
			pointer.CursorPointer.Add(gtx.Ops)
		}

		iconSide := gtx.Dp(unit.Dp(20))
		if ic == nil {
			return layout.Dimensions{Size: image.Pt(side, side)}
		}
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = image.Pt(iconSide, iconSide)
			gtx.Constraints.Max = image.Pt(iconSide, iconSide)
			return ic.Layout(gtx, fg)
		})
	})
}

// groupSep renders a thin vertical divider with horizontal padding, used to
// visually separate logical button groups in the toolbar.
func groupSep(th *Theme) layout.FlexChild {
	return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		pad := gtx.Dp(unit.Dp(8))
		lineW := gtx.Dp(unit.Dp(1))
		lineH := gtx.Dp(unit.Dp(20))
		total := pad*2 + lineW
		rowH := gtx.Constraints.Max.Y
		if rowH <= 0 {
			rowH = gtx.Dp(unit.Dp(toolbarHeightDp))
		}
		yTop := max((rowH-lineH)/2, 0)
		rect := image.Rect(pad, yTop, pad+lineW, yTop+lineH)
		col := th.Muted
		col.A = 0x55
		stack := clip.Rect(rect).Push(gtx.Ops)
		paint.ColorOp{Color: col}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		stack.Pop()
		return layout.Dimensions{Size: image.Pt(total, rowH)}
	})
}

// DrawTooltipOverlay renders the current hover-tooltip label as a small
// rounded box. The caller positions the parent gtx so that (0,0) is the
// top-right corner of the tooltip; the tooltip extends down and to the
// left from that point.
func (t *Toolbar) DrawTooltipOverlay(gtx layout.Context, th *Theme, anchorRight int) layout.Dimensions {
	if t.tooltipText == "" {
		return layout.Dimensions{}
	}
	padH := gtx.Dp(unit.Dp(toolbarTooltipPadH))
	padV := gtx.Dp(unit.Dp(toolbarTooltipPadV))

	// Record the label so we can size the background to match its bounds.
	macro := op.Record(gtx.Ops)
	mgtx := gtx
	mgtx.Constraints.Min = image.Point{}
	mgtx.Constraints.Max.X = max(gtx.Constraints.Max.X, 400)
	lbl := material.Label(th.Theme, unit.Sp(12), t.tooltipText)
	lbl.Color = th.Foreground
	lbl.MaxLines = 1
	lblDims := lbl.Layout(mgtx)
	labelCall := macro.Stop()

	w := lblDims.Size.X + padH*2
	h := lblDims.Size.Y + padV*2
	x0 := anchorRight - w
	if x0 < 0 {
		x0 = 0
	}

	// Background.
	rect := image.Rect(x0, 0, x0+w, h)
	rr := clip.UniformRRect(rect, gtx.Dp(unit.Dp(4))).Push(gtx.Ops)
	bg := th.Background
	bg.A = 0xee
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	rr.Pop()

	// Place the recorded label inside the box.
	tr := op.Offset(image.Pt(x0+padH, padV)).Push(gtx.Ops)
	labelCall.Add(gtx.Ops)
	tr.Pop()

	return layout.Dimensions{Size: image.Pt(w, h)}
}

func rawTooltip(on bool) string {
	if on {
		return "RAW files visible — click to hide"
	}
	return "RAW files hidden — click to show"
}

func yearsTooltip(on bool) string {
	if on {
		return "Year grouping on — click to disable"
	}
	return "Group date folders by year"
}

func sortDurTooltip(on bool) string {
	if on {
		return "Sorted by video length — click to sort by name"
	}
	return "Sort by video length (longest first)"
}
