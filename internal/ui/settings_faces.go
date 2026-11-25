package ui

import (
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/face"
)

// faceStatusText renders the helper probe result for the Settings row. probed
// is false until the first probe finished, so an empty Status means "still
// checking" rather than "missing". The ordering matters: a stale Status from
// before the first probe must not claim the helper is absent.
func faceStatusText(st face.Status, probed bool) string {
	if !probed {
		return "Checking the face helper…"
	}
	switch {
	case st.Working:
		return "Helper: OK (" + st.BinaryPath + ")"
	case st.BinaryPath == "":
		return "Helper not installed. Install with: pipx install face_recognition"
	default:
		return "Helper broken: " + st.Error
	}
}

// normaliseStrictness maps a config value to the radio key shown in Settings.
// Empty (never set) and unknown values fall back to normal, which is the same
// default the clusterer uses.
func normaliseStrictness(s string) string {
	switch s {
	case StrictnessStrict, StrictnessNormal, StrictnessLoose:
		return s
	}
	return StrictnessNormal
}

// facesRow draws the "Face recognition" section: the detect toggle, the
// helper status with a Recheck button, the strictness presets and the
// two-step "Forget all face data" confirmation. Hidden when no controller is
// wired.
func (v *SettingsView) facesRow(th *Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if v.ctrl == nil {
			return layout.Dimensions{}
		}
		if v.facesOn.Update(gtx) {
			v.ctrl.SetFacesEnabled(v.facesOn.Value)
		}
		// A strictness change persists the preset and regroups every face.
		if v.strictness.Update(gtx) {
			v.ctrl.SetFaceStrictness(v.strictness.Value)
		}
		if v.facesRecheck.Clicked(gtx) {
			v.ctrl.RecheckFaces()
		}
		if v.forgetBtn.Clicked(gtx) {
			v.forgetStage = 1
		}
		if v.forgetKeepBtn.Clicked(gtx) {
			v.forgetStage = 0
			v.ctrl.ForgetFaces(false)
		}
		if v.forgetAllBtn.Clicked(gtx) {
			v.forgetStage = 0
			v.ctrl.ForgetFaces(true)
		}
		if v.forgetCancelBtn.Clicked(gtx) {
			v.forgetStage = 0
		}
		st, probed := v.ctrl.FaceStatus()
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), "Face recognition")
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.facesOn,
					"Detect and group faces (runs locally with pv-face-detect)")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), faceStatusText(st, probed))
						lbl.Color = th.Foreground
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &v.facesRecheck, "Recheck").Layout),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						rb := material.RadioButton(th.Theme, &v.strictness, StrictnessStrict, "Strict (0.50)")
						rb.Color = th.Foreground
						rb.IconColor = th.Accent
						return rb.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						rb := material.RadioButton(th.Theme, &v.strictness, StrictnessNormal, "Normal (0.60)")
						rb.Color = th.Foreground
						rb.IconColor = th.Accent
						return rb.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						rb := material.RadioButton(th.Theme, &v.strictness, StrictnessLoose, "Loose (0.65)")
						rb.Color = th.Foreground
						rb.IconColor = th.Accent
						return rb.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(2)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(11),
					"Changing it regroups every face; names and fixes are kept.")
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if v.forgetStage == 0 {
					return layout.Flex{}.Layout(gtx, layout.Rigid(material.Button(th.Theme, &v.forgetBtn, "Forget all face data…").Layout))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(11),
							"Delete detected faces and groups? Names and your fixes can be kept or deleted too.")
						lbl.Color = th.Muted
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(material.Button(th.Theme, &v.forgetKeepBtn, "Forget detections (keep names)").Layout),
							layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
							layout.Rigid(material.Button(th.Theme, &v.forgetAllBtn, "Forget everything incl. names").Layout),
							layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
							layout.Rigid(material.Button(th.Theme, &v.forgetCancelBtn, "Cancel").Layout),
						)
					}),
				)
			}),
		)
	}
}
