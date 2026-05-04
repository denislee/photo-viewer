package ui

import (
	"fmt"
	"image"
	"math"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const toolbarHeightDp = 48

// Toolbar is the slim top bar above the main split. Shows the active path,
// the entry count, and a row of buttons that mirror the Fyne build's actions
// (filter, RAW toggle, import, duplicates, settings, rebuild).
type Toolbar struct {
	allBtn        widget.Clickable
	photosBtn     widget.Clickable
	videosBtn     widget.Clickable
	rawCheck      widget.Bool
	yearsCheck    widget.Bool
	importBtn     widget.Clickable
	duplicatesBtn widget.Clickable
	settingsBtn   widget.Clickable
	organizeBtn   widget.Clickable
	rebuildBtn    widget.Clickable
	spinnerBtn    widget.Clickable

	OnFilter      func(string)
	OnShowRAW     func(bool)
	OnGroupByYear func(bool)
	OnImport      func()
	OnDuplicates  func()
	OnOrganize    func()
	OnSettings    func()
	OnRebuild     func()
	OnIndexInfo   func()

	// State mirrored from the controller; the active filter button is
	// rendered with HighImportance so it stands out.
	Filter      string
	ShowRAW     bool
	GroupByYear bool
	Busy        bool

	spinnerStart time.Time
}

func NewToolbar() *Toolbar { return &Toolbar{Filter: "All", spinnerStart: time.Now()} }

// Layout draws the toolbar background, labels, and action buttons. Drains
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
	// Sync the checkbox state to the controller mirror so external changes
	// (e.g. config reload) are reflected. Only fire the callback when the
	// user actually toggles it via Update.
	t.rawCheck.Value = t.ShowRAW
	if t.rawCheck.Update(gtx) {
		t.ShowRAW = t.rawCheck.Value
		if t.OnShowRAW != nil {
			t.OnShowRAW(t.rawCheck.Value)
		}
	}
	t.yearsCheck.Value = t.GroupByYear
	if t.yearsCheck.Update(gtx) {
		t.GroupByYear = t.yearsCheck.Value
		if t.OnGroupByYear != nil {
			t.OnGroupByYear(t.yearsCheck.Value)
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
	if t.rebuildBtn.Clicked(gtx) && t.OnRebuild != nil {
		t.OnRebuild()
	}
	if t.spinnerBtn.Clicked(gtx) && t.OnIndexInfo != nil {
		t.OnIndexInfo()
	}

	h := gtx.Dp(unit.Dp(toolbarHeightDp))
	w := gtx.Constraints.Max.X

	bg := image.Rect(0, 0, w, h)
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	filterBtn := func(btn *widget.Clickable, label, name string) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			b := material.Button(th.Theme, btn, label)
			if t.Filter != name {
				b.Background = th.CellBG
				b.Color = th.Foreground
			}
			return b.Layout(gtx)
		})
	}

	pad := layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(12), Right: unit.Dp(12)}
	pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), path)
				lbl.Color = th.Foreground
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), fmt.Sprintf("%d items", count))
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !t.Busy {
					return layout.Dimensions{}
				}
				return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return t.spinnerBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return t.layoutSpinner(gtx, th)
					})
				})
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
			filterBtn(&t.allBtn, "All", "All"),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			filterBtn(&t.photosBtn, "Photos", "Photos"),
			layout.Rigid(layout.Spacer{Width: unit.Dp(2)}.Layout),
			filterBtn(&t.videosBtn, "Videos", "Videos"),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &t.rawCheck, "RAW")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &t.yearsCheck, "Years")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
			layout.Rigid(material.Button(th.Theme, &t.importBtn, "Import").Layout),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(material.Button(th.Theme, &t.duplicatesBtn, "Duplicates").Layout),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(material.Button(th.Theme, &t.organizeBtn, "Organize").Layout),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(material.Button(th.Theme, &t.settingsBtn, "Settings").Layout),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(material.Button(th.Theme, &t.rebuildBtn, "Rebuild").Layout),
		)
	})

	return layout.Dimensions{Size: image.Pt(w, h)}
}

// layoutSpinner draws a rotating ring of fading dots, sized to fit alongside
// the toolbar text. Schedules an immediate redraw so the rotation animates.
func (t *Toolbar) layoutSpinner(gtx layout.Context, th *Theme) layout.Dimensions {
	sz := gtx.Dp(unit.Dp(16))
	cx := float32(sz) / 2
	cy := float32(sz) / 2
	radius := float32(sz)/2 - 1.5
	dotR := float32(sz) / 7

	const N = 8
	elapsed := time.Since(t.spinnerStart).Seconds()
	step := int(elapsed*float64(N)) % N // discrete rotation; one full turn per second

	for i := 0; i < N; i++ {
		// i==0 is the leading bright dot, then the trail fades.
		idx := (step - i + N) % N
		ang := float64(idx) * 2 * math.Pi / float64(N)
		x := cx + radius*float32(math.Cos(ang))
		y := cy + radius*float32(math.Sin(ang))

		col := th.Foreground
		col.A = uint8(220 - i*26)

		ell := image.Rect(int(x-dotR), int(y-dotR), int(x+dotR+1), int(y+dotR+1))
		stack := clip.Ellipse(ell).Push(gtx.Ops)
		paint.ColorOp{Color: col}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		stack.Pop()
	}

	gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(80 * time.Millisecond)})
	return layout.Dimensions{Size: image.Pt(sz, sz)}
}
