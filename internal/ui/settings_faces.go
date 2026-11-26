package ui

import (
	"strconv"
	"time"

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

// faceCheckText is the status line: the probe result, or "Checking…" while a
// Recheck is in flight, with the time of the last Recheck appended.
func faceCheckText(st face.Status, probed, checking bool, checkedAt time.Time) string {
	if checking {
		return "Checking the face helper…"
	}
	s := faceStatusText(st, probed)
	if probed && !checkedAt.IsZero() {
		s += " · checked " + checkedAt.Format("15:04:05")
	}
	return s
}

// faceTaskText is the progress line of a running face task.
func faceTaskText(t ProcessSnapshot) string {
	s := t.Title
	if t.Total > 0 {
		s += ": " + strconv.FormatInt(t.Done, 10) + " / " + strconv.FormatInt(t.Total, 10)
	}
	if t.Paused {
		s += " (paused)"
	}
	return s
}

// peopleCountText is the "N people · M groups" summary line.
func peopleCountText(snap PeopleSnapshot) string {
	n, g := len(snap.People)+len(snap.Hidden), len(snap.Groups)+len(snap.SmallGroups)
	people := strconv.Itoa(n) + " people"
	if n == 1 {
		people = "1 person"
	}
	groups := strconv.Itoa(g) + " unnamed groups"
	if g == 1 {
		groups = "1 unnamed group"
	}
	return people + " · " + groups
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
// helper status with a Recheck button, the running face task (progress,
// Pause/Resume, Stop) or the idle scan/regroup controls with the last scan's
// summary, the strictness presets and the two-step "Forget all face data"
// confirmation. Hidden when no controller is
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
		if v.facesDetectBtn.Clicked(gtx) {
			v.ctrl.DetectFaces()
		}
		if v.facesRegroupBtn.Clicked(gtx) {
			v.ctrl.RegroupFaces()
		}
		task, busy := v.ctrl.FaceTask()
		if v.facesPauseBtn.Clicked(gtx) && busy {
			if p := v.ctrl.processes.Get(task.ID); task.Paused {
				p.Resume()
			} else {
				p.Pause()
			}
		}
		if v.facesStopBtn.Clicked(gtx) && busy {
			v.ctrl.processes.Get(task.ID).Cancel()
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
		checking, checkedAt := v.ctrl.FaceChecking()
		running := v.ctrl.facesRunning()
		// Poll while a probe runs so a lost invalidate can't freeze the row
		// on "Checking…"; task progress repaints via the process registry.
		pollWhileLoading(gtx, checking)
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
						lbl := material.Label(th.Theme, unit.Sp(12), faceCheckText(st, probed, checking, checkedAt))
						lbl.Color = th.Foreground
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Recheck"
						if checking {
							label = "Checking…"
							gtx = gtx.Disabled()
						}
						return material.Button(th.Theme, &v.facesRecheck, label).Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if busy {
					return v.faceTaskRow(gtx, th, task)
				}
				return v.faceIdleRow(gtx, th, running)
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

// faceTaskRow shows the running detection pass or regroup with a progress
// bar and Pause/Resume (detection only) and Stop.
func (v *SettingsView) faceTaskRow(gtx layout.Context, th *Theme, task ProcessSnapshot) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(12), faceTaskText(task))
					lbl.Color = th.Foreground
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !task.Pausable {
						return layout.Dimensions{}
					}
					label := "Pause"
					if task.Paused {
						label = "Resume"
					}
					return layout.Inset{Right: unit.Dp(6)}.Layout(gtx,
						material.Button(th.Theme, &v.facesPauseBtn, label).Layout)
				}),
				layout.Rigid(material.Button(th.Theme, &v.facesStopBtn, "Stop").Layout),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layoutProgressBar(gtx, th, task.Done, task.Total, unit.Dp(6))
		}),
	)
}

// faceIdleRow shows the last scan summary, the people/group counts and the
// "Scan for new faces" / "Regroup all" buttons (disabled until the pipeline
// runs).
func (v *SettingsView) faceIdleRow(gtx layout.Context, th *Theme, running bool) layout.Dimensions {
	summary := v.ctrl.LastFacePass()
	if summary == "" {
		summary = "No face scan has run in this session yet."
	}
	if snap, ok := v.ctrl.People(); ok {
		summary += "\n" + peopleCountText(snap)
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(11), summary)
			lbl.Color = th.Muted
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !running {
				gtx = gtx.Disabled()
			}
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(material.Button(th.Theme, &v.facesDetectBtn, "Scan for new faces").Layout),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(material.Button(th.Theme, &v.facesRegroupBtn, "Regroup all").Layout),
			)
		}),
	)
}
