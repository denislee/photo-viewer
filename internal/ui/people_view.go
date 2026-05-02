package ui

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// PeopleViewActions bundles the controller hooks the People view needs.
// Grouping them keeps the constructor readable as it grows.
type PeopleViewActions struct {
	LoadAvatar func(cache.Cluster) fyne.Resource
	OnPick     func(clusterID int64, label string)
	OnClose    func()
	// OnRecheck re-probes the helper and returns the fresh status. Caller
	// is also expected to (re-)start the worker pool when newly working.
	OnRecheck func() face.Status
	// OnRunNow asks the controller to enqueue every photo in the index for
	// detection. May be a long-running background operation; the callback
	// itself should return promptly.
	OnRunNow func()
}

// ShowPeople opens the dedicated People view. Always renders a Face
// detection management panel at the top so the user can install/recheck the
// helper without leaving the app.
func ShowPeople(win fyne.Window, idx *cache.Index, actions PeopleViewActions) {
	closeBtn := widget.NewButtonWithIcon("Back to Gallery", theme.NavigateBackIcon(), func() {
		if actions.OnClose != nil {
			actions.OnClose()
		}
	})

	header := canvas.NewText("People", theme.Color(theme.ColorNameForeground))
	header.TextSize = 18
	header.TextStyle = fyne.TextStyle{Bold: true}

	// Status panel: rebuilt in-place when the user clicks Recheck, so the
	// surrounding container is captured by reference.
	statusBox := container.NewVBox()
	gridBox := container.NewStack()

	render := func() {
		clusters := idx.AllClusters()
		status := face.Probe()

		statusBox.Objects = nil
		statusBox.Add(buildFaceStatusPanel(win, status, actions))
		statusBox.Refresh()

		gridBox.Objects = nil
		if len(clusters) == 0 {
			msg := "No people detected yet."
			if status.Working {
				msg = "No people detected yet. Click \"Run on library\" or wait for the next scan."
			}
			gridBox.Add(container.NewCenter(widget.NewLabel(msg)))
		} else {
			gridBox.Add(buildPeopleGrid(clusters, actions))
		}
		gridBox.Refresh()
	}

	// Wrap OnRecheck / OnRunNow so the view re-renders after the action.
	wrapped := actions
	wrapped.OnRecheck = func() face.Status {
		var s face.Status
		if actions.OnRecheck != nil {
			s = actions.OnRecheck()
		} else {
			s = face.Probe()
		}
		render()
		return s
	}
	wrapped.OnRunNow = func() {
		if actions.OnRunNow != nil {
			actions.OnRunNow()
		}
		render()
	}
	actions = wrapped

	render()

	body := container.NewBorder(
		container.NewVBox(container.NewHBox(closeBtn), header, statusBox),
		nil, nil, nil,
		gridBox,
	)
	win.SetContent(body)
}

// buildPeopleGrid is the cluster grid extracted so render() can swap it in
// or out without rebuilding the surrounding chrome.
func buildPeopleGrid(clusters []cache.Cluster, actions PeopleViewActions) fyne.CanvasObject {
	return widget.NewGridWrap(
		func() int { return len(clusters) },
		func() fyne.CanvasObject { return newPeopleTile() },
		func(id widget.GridWrapItemID, obj fyne.CanvasObject) {
			if id < 0 || int(id) >= len(clusters) {
				return
			}
			cl := clusters[id]
			tile := obj.(*peopleTile)
			tile.bind(cl, actions.OnPick)
			expected := cl.ID
			go func() {
				var res fyne.Resource
				if actions.LoadAvatar != nil {
					res = actions.LoadAvatar(cl)
				}
				if res == nil {
					return
				}
				fyne.Do(func() { tile.setAvatar(expected, res) })
			}()
		},
	)
}

// buildFaceStatusPanel renders the management section: a coloured status
// pill, install hint, and Recheck / Run-now buttons.
func buildFaceStatusPanel(win fyne.Window, s face.Status, actions PeopleViewActions) fyne.CanvasObject {
	var pillText string
	var pillColor color.Color
	switch {
	case s.Working:
		pillText = "Installed"
		pillColor = color.NRGBA{R: 0x4c, G: 0xaf, B: 0x50, A: 0xff}
	case s.BinaryPath != "":
		pillText = "Broken"
		pillColor = color.NRGBA{R: 0xff, G: 0xb3, B: 0x00, A: 0xff}
	default:
		pillText = "Not installed"
		pillColor = color.NRGBA{R: 0xe5, G: 0x39, B: 0x35, A: 0xff}
	}
	pill := canvas.NewText(pillText, pillColor)
	pill.TextStyle = fyne.TextStyle{Bold: true}
	pill.TextSize = 12

	title := canvas.NewText("Face detection", theme.Color(theme.ColorNameForeground))
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.TextSize = 14

	installCmd := widget.NewEntry()
	installCmd.SetText("pip install --user face_recognition  # or: pipx install face_recognition")
	installCmd.OnChanged = func(string) {
		installCmd.SetText("pip install --user face_recognition  # or: pipx install face_recognition")
	}

	recheck := widget.NewButtonWithIcon("Recheck", theme.ViewRefreshIcon(), func() {
		if actions.OnRecheck != nil {
			actions.OnRecheck()
		}
	})
	runNow := widget.NewButtonWithIcon("Run on library", theme.MediaPlayIcon(), func() {
		if actions.OnRunNow != nil {
			actions.OnRunNow()
		}
	})
	if !s.Working {
		runNow.Disable()
	}

	rows := []fyne.CanvasObject{
		container.NewHBox(title, layout.NewSpacer(), pill),
	}
	if s.BinaryPath != "" {
		path := canvas.NewText("Helper: "+s.BinaryPath, theme.Color(theme.ColorNameDisabled))
		path.TextSize = 11
		rows = append(rows, path)
	}
	if s.Error != "" {
		errLbl := widget.NewLabel(s.Error)
		errLbl.Wrapping = fyne.TextWrapWord
		rows = append(rows, errLbl)
	}
	if !s.Working {
		hint := canvas.NewText("Install with the command below, then click Recheck.", theme.Color(theme.ColorNameForeground))
		hint.TextSize = 11
		rows = append(rows, hint, installCmd)
	}
	rows = append(rows, container.NewHBox(recheck, runNow))

	card := container.NewVBox(rows...)
	sep := canvas.NewRectangle(theme.Color(theme.ColorNameDisabled))
	sep.SetMinSize(fyne.NewSize(0, 1))
	return container.NewVBox(container.NewPadded(card), sep)
}

// peopleTile is one tile in the People grid: avatar over name + count.
type peopleTile struct {
	widget.BaseWidget

	avatar   *canvas.Image
	fallback *widget.Icon
	stack    *fyne.Container
	name     *widget.Label
	cluster  cache.Cluster
	onTap    func(int64, string)
}

func newPeopleTile() *peopleTile {
	t := &peopleTile{
		fallback: widget.NewIcon(theme.AccountIcon()),
		name:     widget.NewLabel(""),
	}
	t.avatar = &canvas.Image{FillMode: canvas.ImageFillContain}
	t.avatar.SetMinSize(fyne.NewSize(96, 96))
	t.avatar.Hide()
	t.stack = container.NewStack(t.fallback, t.avatar)
	t.name.Alignment = fyne.TextAlignCenter
	t.ExtendBaseWidget(t)
	return t
}

func (t *peopleTile) bind(cl cache.Cluster, onTap func(int64, string)) {
	t.cluster = cl
	t.onTap = onTap
	label := cl.Label.String
	if !cl.Label.Valid || label == "" {
		label = "Unnamed"
	}
	t.name.SetText(fmt.Sprintf("%s\n%d photos", label, cl.Count))
	t.avatar.Resource = nil
	t.avatar.Hide()
	t.fallback.Show()
	t.Refresh()
}

func (t *peopleTile) setAvatar(expected int64, res fyne.Resource) {
	if t.cluster.ID != expected {
		return
	}
	t.avatar.Resource = res
	t.avatar.Show()
	t.fallback.Hide()
	t.avatar.Refresh()
}

func (t *peopleTile) CreateRenderer() fyne.WidgetRenderer {
	box := container.NewVBox(t.stack, t.name)
	return widget.NewSimpleRenderer(container.NewPadded(box))
}

func (t *peopleTile) Tapped(*fyne.PointEvent) {
	if t.onTap == nil {
		return
	}
	label := t.cluster.Label.String
	if !t.cluster.Label.Valid || label == "" {
		label = fmt.Sprintf("Person #%d", t.cluster.ID)
	}
	t.onTap(t.cluster.ID, label)
}
