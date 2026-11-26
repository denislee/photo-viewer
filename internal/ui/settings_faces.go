package ui

import (
	"strconv"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
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

// facesSection: face recognition, in four sub-blocks — Detection (toggle,
// helper status + Recheck), Library scan (the running face task with
// Pause/Resume/Stop, or the idle summary with Scan / Regroup), Grouping
// (strictness presets) and Danger zone (the two-step "Forget all face
// data"). Everything saves on change. Hidden when no controller is wired.
type facesSection struct {
	host *SettingsView

	facesOn         widget.Bool
	recheckBtn      widget.Clickable
	detectBtn       widget.Clickable
	regroupBtn      widget.Clickable
	pauseBtn        widget.Clickable
	stopBtn         widget.Clickable
	strictness      widget.Enum
	forgetBtn       widget.Clickable
	forgetKeepBtn   widget.Clickable
	forgetAllBtn    widget.Clickable
	forgetCancelBtn widget.Clickable
	// forgetStage is the two-step "Forget all face data" confirmation:
	// 0 idle, 1 confirming. Reset by show. UI goroutine only.
	forgetStage int
}

func (s *facesSection) id() string { return "faces" }

func (s *facesSection) show(c Config) {
	s.facesOn.Value = FacesEnabled()
	s.strictness.Value = normaliseStrictness(c.FaceStrictness)
	s.forgetStage = 0
}

// update handles the section's clicks and toggles. It returns the running
// face task, if any.
func (s *facesSection) update(gtx layout.Context, ctrl *Controller) (ProcessSnapshot, bool) {
	if s.facesOn.Update(gtx) {
		ctrl.SetFacesEnabled(s.facesOn.Value)
	}
	// A strictness change persists the preset and regroups every face.
	if s.strictness.Update(gtx) {
		ctrl.SetFaceStrictness(s.strictness.Value)
	}
	if s.recheckBtn.Clicked(gtx) {
		ctrl.RecheckFaces()
	}
	if s.detectBtn.Clicked(gtx) {
		ctrl.DetectFaces()
	}
	if s.regroupBtn.Clicked(gtx) {
		ctrl.RegroupFaces()
	}
	task, busy := ctrl.FaceTask()
	if s.pauseBtn.Clicked(gtx) && busy {
		if p := ctrl.processes.Get(task.ID); task.Paused {
			p.Resume()
		} else {
			p.Pause()
		}
	}
	if s.stopBtn.Clicked(gtx) && busy {
		ctrl.processes.Get(task.ID).Cancel()
	}
	if s.forgetBtn.Clicked(gtx) {
		s.forgetStage = 1
	}
	if s.forgetKeepBtn.Clicked(gtx) {
		s.forgetStage = 0
		ctrl.ForgetFaces(false)
	}
	if s.forgetAllBtn.Clicked(gtx) {
		s.forgetStage = 0
		ctrl.ForgetFaces(true)
	}
	if s.forgetCancelBtn.Clicked(gtx) {
		s.forgetStage = 0
	}
	return task, busy
}

func (s *facesSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	ctrl := s.host.ctrl
	if ctrl == nil {
		return layout.Dimensions{}
	}
	task, busy := s.update(gtx, ctrl)
	checking, _ := ctrl.FaceChecking()
	// Poll while a probe runs so a lost invalidate can't freeze the row on
	// "Checking…"; task progress repaints via the process registry.
	pollWhileLoading(gtx, checking)
	return column(gtx,
		layout.Rigid(sectionHeader(th, "Face recognition",
			"Finds faces in thumbnails and groups them into people. Runs locally; nothing leaves this machine.")),
		vspace(10),
		layout.Rigid(subHeader(th, "Detection")),
		vspace(4),
		layout.Rigid(s.detectionBlock(th, ctrl)),
		vspace(10),
		layout.Rigid(subHeader(th, "Library scan")),
		vspace(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if busy {
				return s.taskRow(gtx, th, task)
			}
			return s.idleRow(gtx, th, ctrl)
		}),
		vspace(10),
		layout.Rigid(subHeader(th, "Grouping")),
		vspace(4),
		layout.Rigid(s.groupingBlock(th)),
		vspace(10),
		layout.Rigid(subHeader(th, "Danger zone")),
		vspace(4),
		layout.Rigid(s.dangerBlock(th)),
	)
}

// detectionBlock: the on/off toggle and the helper status with Recheck.
func (s *facesSection) detectionBlock(th *Theme, ctrl *Controller) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		st, probed := ctrl.FaceStatus()
		checking, checkedAt := ctrl.FaceChecking()
		return column(gtx,
			layout.Rigid(checkbox(th, &s.facesOn, "Detect and group faces (runs locally with pv-face-detect)")),
			vspace(4),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, bodyText(th, faceCheckText(st, probed, checking, checkedAt))),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Recheck"
						if checking {
							label = "Checking…"
							gtx = gtx.Disabled()
						}
						return material.Button(th.Theme, &s.recheckBtn, label).Layout(gtx)
					}),
				)
			}),
		)
	}
}

// groupingBlock: the strictness presets and their hint.
func (s *facesSection) groupingBlock(th *Theme) layout.Widget {
	radio := func(key, label string) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			rb := material.RadioButton(th.Theme, &s.strictness, key, label)
			rb.Color = th.Foreground
			rb.IconColor = th.Accent
			return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, rb.Layout)
		})
	}
	return func(gtx layout.Context) layout.Dimensions {
		return column(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					radio(StrictnessStrict, "Strict (0.50)"),
					radio(StrictnessNormal, "Normal (0.60)"),
					radio(StrictnessLoose, "Loose (0.65)"),
				)
			}),
			vspace(2),
			layout.Rigid(mutedText(th, "Changing it regroups every face; names and fixes are kept.")),
		)
	}
}

// dangerBlock: "Forget all face data…" and its two-step confirmation.
func (s *facesSection) dangerBlock(th *Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if s.forgetStage == 0 {
			return layout.Flex{}.Layout(gtx, layout.Rigid(material.Button(th.Theme, &s.forgetBtn, "Forget all face data…").Layout))
		}
		return column(gtx,
			layout.Rigid(mutedText(th, "Delete detected faces and groups? Names and your fixes can be kept or deleted too.")),
			vspace(4),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(material.Button(th.Theme, &s.forgetKeepBtn, "Forget detections (keep names)").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &s.forgetAllBtn, "Forget everything incl. names").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &s.forgetCancelBtn, "Cancel").Layout),
				)
			}),
		)
	}
}

// taskRow shows the running detection pass or regroup with a progress bar
// and Pause/Resume (detection only) and Stop.
func (s *facesSection) taskRow(gtx layout.Context, th *Theme, task ProcessSnapshot) layout.Dimensions {
	return column(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, bodyText(th, faceTaskText(task))),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !task.Pausable {
						return layout.Dimensions{}
					}
					label := "Pause"
					if task.Paused {
						label = "Resume"
					}
					return layout.Inset{Right: unit.Dp(6)}.Layout(gtx,
						material.Button(th.Theme, &s.pauseBtn, label).Layout)
				}),
				layout.Rigid(material.Button(th.Theme, &s.stopBtn, "Stop").Layout),
			)
		}),
		vspace(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layoutProgressBar(gtx, th, task.Done, task.Total, unit.Dp(6))
		}),
	)
}

// idleRow shows the last scan summary, the people/group counts and the
// "Scan for new faces" / "Regroup all" buttons (disabled until the pipeline
// runs).
func (s *facesSection) idleRow(gtx layout.Context, th *Theme, ctrl *Controller) layout.Dimensions {
	summary := ctrl.LastFacePass()
	if summary == "" {
		summary = "No face scan has run in this session yet."
	}
	if snap, ok := ctrl.People(); ok {
		summary += "\n" + peopleCountText(snap)
	}
	running := ctrl.facesRunning()
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, mutedText(th, summary)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !running {
				gtx = gtx.Disabled()
			}
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(material.Button(th.Theme, &s.detectBtn, "Scan for new faces").Layout),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(material.Button(th.Theme, &s.regroupBtn, "Regroup all").Layout),
			)
		}),
	)
}
