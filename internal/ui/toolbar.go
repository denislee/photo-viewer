package ui

import (
	"fmt"
	"image"
	"image/color"

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

// iconGap separates adjacent toolbar icon buttons.
var iconGap = layout.Spacer{Width: unit.Dp(2)}

// Toolbar is the slim top bar above the main split. Shows the active path, the
// entry count, and an icon-button row: filters, RAW, grouping, sort, import,
// duplicates, organize, export, settings, rebuild, warm-up and web server.
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
	exportFavBtn  widget.Clickable
	rebuildBtn    widget.Clickable
	warmUpBtn     widget.Clickable
	webBtn        widget.Clickable

	OnFilter          func(string)
	OnShowRAW         func(bool)
	OnGroupByYear     func(bool)
	OnSortByLength    func(bool)
	OnImport          func()
	OnDuplicates      func()
	OnOrganize        func()
	OnExportFavorites func()
	OnSettings        func()
	OnRebuild         func()
	OnWarmUp          func()
	OnWebServer       func()

	// WebServerRunning, when true, renders the webserver button in the
	// "active" accent so users can see at a glance that the server is up.
	WebServerRunning bool

	// State mirrored from the controller; the active filter button is
	// rendered with the accent background so it stands out.
	Filter       string
	ShowRAW      bool
	GroupByYear  bool
	SortByLength bool
	// AnyProcess is true while the process bar has at least one in-flight
	// task. Used to disable buttons (Rebuild) that would conflict with an
	// already-running job.
	AnyProcess bool

	// tooltipText is the label of whichever icon button the pointer is
	// hovering over this frame, or empty when nothing is hovered. Read by
	// the window code so the tooltip can be drawn as an overlay below the
	// toolbar (which would otherwise be clipped by the body region).
	tooltipText string

	// itemsCount/itemsLabel memoize the "N items" label (G4-12): the count
	// changes only when the library does, so formatting it every frame is
	// wasted work.
	itemsCount int
	itemsLabel string
}

// itemsText returns the "N items" label, formatting it only when the count
// differs from the one the memoized label was built from.
func (t *Toolbar) itemsText(count int) string {
	if t.itemsLabel != "" && t.itemsCount == count {
		return t.itemsLabel
	}
	t.itemsCount = count
	t.itemsLabel = fmt.Sprintf("%d items", count)
	return t.itemsLabel
}

func NewToolbar() *Toolbar { return &Toolbar{Filter: "All"} }

// HoveredLabel returns the tooltip text for the currently hovered toolbar
// button, or "" when nothing is hovered.
func (t *Toolbar) HoveredLabel() string { return t.tooltipText }

// Layout draws the toolbar background, labels, and icon buttons. Drains
// click events on each call so callbacks fire on the same frame.
func (t *Toolbar) Layout(gtx layout.Context, th *Theme, path string, count int) layout.Dimensions {
	// Filter group: one table for the segmented All / Photos / Videos buttons.
	for _, f := range [...]struct {
		btn  *widget.Clickable
		name string
	}{
		{&t.allBtn, "All"},
		{&t.photosBtn, "Photos"},
		{&t.videosBtn, "Videos"},
	} {
		if f.btn.Clicked(gtx) {
			t.Filter = f.name
			if t.OnFilter != nil {
				t.OnFilter(f.name)
			}
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
	if t.exportFavBtn.Clicked(gtx) && t.OnExportFavorites != nil {
		t.OnExportFavorites()
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
	if t.webBtn.Clicked(gtx) && t.OnWebServer != nil {
		t.OnWebServer()
	}

	h := gtx.Dp(unit.Dp(toolbarHeightDp))
	w := gtx.Constraints.Max.X
	items := t.itemsText(count)

	bg := image.Rect(0, 0, w, h)
	paint.FillShape(gtx.Ops, th.CellBG, clip.Rect(bg).Op())

	// Reset the hover tooltip; each button below sets it if hovered.
	t.tooltipText = ""

	icon := func(btn *widget.Clickable, ic *widget.Icon, label string, active, disabled bool) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
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
			side := gtx.Dp(unit.Dp(toolbarIconCellDp))
			dims := squareButton(gtx, th, btn, side, gtx.Dp(unit.Dp(5)), bg, fg, ic, "", gtx.Dp(unit.Dp(20)), !disabled)
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
				lbl := material.Label(th.Theme, unit.Sp(12), items)
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),

			groupSep(th),

			// Filter group: segmented All / Photos / Videos.
			icon(&t.allBtn, th.Icons.All, "All", t.Filter == "All", false),
			layout.Rigid(iconGap.Layout),
			icon(&t.photosBtn, th.Icons.Photos, "Photos", t.Filter == "Photos", false),
			layout.Rigid(iconGap.Layout),
			icon(&t.videosBtn, th.Icons.Videos, "Videos", t.Filter == "Videos", false),

			groupSep(th),

			// Display toggles.
			icon(&t.rawBtn, th.Icons.RAW, rawTooltip(t.ShowRAW), t.ShowRAW, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.yearsBtn, th.Icons.Years, yearsTooltip(t.GroupByYear), t.GroupByYear, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.sortDurBtn, th.Icons.SortDur, sortDurTooltip(t.SortByLength), t.SortByLength, false),

			groupSep(th),

			// Library actions.
			icon(&t.importBtn, th.Icons.Import, "Import", false, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.duplicatesBtn, th.Icons.Duplicates, "Find duplicates", false, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.organizeBtn, th.Icons.Organize, "Organize", false, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.exportFavBtn, th.Icons.ExportFav, "Export favorites to folder…", false, false),

			groupSep(th),

			// Index maintenance.
			icon(&t.rebuildBtn, th.Icons.Rebuild, "Rebuild index", false, t.AnyProcess),
			layout.Rigid(iconGap.Layout),
			icon(&t.warmUpBtn, th.Icons.WarmUp, "Warm up thumbnails", false, false),
			layout.Rigid(iconGap.Layout),
			icon(&t.webBtn, th.Icons.WebServer, webServerTooltip(t.WebServerRunning), t.WebServerRunning, false),

			groupSep(th),

			// Settings, anchored at the far right.
			icon(&t.settingsBtn, th.Icons.Settings, "Settings", false, false),
		)
	})

	return layout.Dimensions{Size: image.Pt(w, h)}
}

// squareButton renders a fixed-size square clickable cell with a rounded
// background and, inside it, either an icon or a centered glyph. It is the one
// square-button layout shared by the toolbar's icon row and the process bar's
// per-row Pause/Cancel/Open buttons. side, radius and iconSide are pixels;
// callers convert their dp metrics with gtx.Dp. cursor adds the pointer hand
// (the toolbar wants it, the process bar's buttons don't).
func squareButton(gtx layout.Context, th *Theme, click *widget.Clickable, side, radius int, bg, fg color.NRGBA, ic *widget.Icon, glyph string, iconSide int, cursor bool) layout.Dimensions {
	gtx.Constraints = layout.Exact(image.Pt(side, side))
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		rect := image.Rectangle{Max: image.Pt(side, side)}
		paint.FillShape(gtx.Ops, bg, clip.UniformRRect(rect, radius).Op(gtx.Ops))

		if cursor {
			pointer.CursorPointer.Add(gtx.Ops)
		}

		switch {
		case ic != nil:
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints = layout.Exact(image.Pt(iconSide, iconSide))
				return ic.Layout(gtx, fg)
			})
		case glyph != "":
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), glyph)
				lbl.Color = fg
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			})
		}
		return layout.Dimensions{Size: image.Pt(side, side)}
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
		paint.FillShape(gtx.Ops, col, clip.Rect(rect).Op())
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
	x0 := max(anchorRight-w, 0)

	// Background.
	rect := image.Rect(x0, 0, x0+w, h)
	bg := th.Background
	bg.A = 0xee
	paint.FillShape(gtx.Ops, bg, clip.UniformRRect(rect, gtx.Dp(unit.Dp(4))).Op(gtx.Ops))

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

func webServerTooltip(on bool) string {
	if on {
		return "Web server running — click to manage"
	}
	return "Share library over the web"
}

func sortDurTooltip(on bool) string {
	if on {
		return "Sorted by video length — click to sort by name"
	}
	return "Sort by video length (longest first)"
}
