package ui

import (
	"fmt"
	"image"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// IndexInfoView is a small modal that shows the indexer's current status,
// reachable by clicking the spinner in the toolbar while a scan is running.
type IndexInfoView struct {
	Open    bool
	OnClose func()

	source   func() IndexStatus
	closeBtn widget.Clickable
}

// NewIndexInfoView builds the modal. source is called each frame while the
// modal is open, so the displayed values stay fresh.
func NewIndexInfoView(source func() IndexStatus) *IndexInfoView {
	return &IndexInfoView{source: source}
}

func (v *IndexInfoView) Show() { v.Open = true }
func (v *IndexInfoView) Close() {
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

// Layout draws the modal over the supplied constraints.
func (v *IndexInfoView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	rect := image.Rectangle{Max: image.Pt(totalW, totalH)}
	bg := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()

	st := v.source()

	pad := layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(20), Right: unit.Dp(20)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Indexing status")
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			v.row(th, "State", stateString(st)),
			v.row(th, "Library root", st.LibraryRoot),
			v.row(th, "Database", st.DBPath),
			v.row(th, "Cache directory", st.CacheDir),
			v.row(th, "Indexed entries", fmt.Sprintf("%d", st.TotalRows)),
			v.row(th, "Current target", fallback(st.Target, "—")),
			v.row(th, "Started", fmtTime(st.StartedAt)),
			v.row(th, "Ended", fmtTime(st.EndedAt)),
			v.row(th, "Elapsed", elapsedString(st)),
			v.row(th, "Reconciled this run", fmt.Sprintf("%d", st.Batched)),
			v.row(th, "Last error", fallback(st.LastError, "—")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
		)
	})
}

func (v *IndexInfoView) row(th *Theme, label, value string) layout.FlexChild {
	return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(170))
					lbl := material.Label(th.Theme, unit.Sp(12), label)
					lbl.Color = th.Foreground
					lbl.Font.Weight = 700
					return lbl.Layout(gtx)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(12), value)
					lbl.Color = th.Foreground
					return lbl.Layout(gtx)
				}),
			)
		})
	})
}

func stateString(s IndexStatus) string {
	if s.Active {
		return "Scanning"
	}
	if !s.EndedAt.IsZero() {
		return "Idle (last run finished)"
	}
	return "Idle"
}

func elapsedString(s IndexStatus) string {
	if s.StartedAt.IsZero() {
		return "—"
	}
	end := s.EndedAt
	if s.Active || end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(s.StartedAt).Round(time.Millisecond)
	return d.String()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("15:04:05")
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
