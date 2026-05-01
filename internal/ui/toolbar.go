package ui

import (
	"image/color"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Toolbar groups the path label, count, busy indicator, and action buttons.
// Visually it is one thin row with subtle separators below.
type Toolbar struct {
	root *fyne.Container

	pathLabel  *canvas.Text
	countLabel *canvas.Text
	filterBtn  *widget.Select
	progress   *widget.Activity
	rebuild    *widget.Button
	settings   *widget.Button
	importBtn  *widget.Button
}

func NewToolbar(onFilter func(string), onRebuild func(), onSettings func(), onImport func()) *Toolbar {
	pathLabel := canvas.NewText("", color.NRGBA{R: 0xc8, G: 0xcc, B: 0xd2, A: 0xff})
	pathLabel.TextSize = 11

	countLabel := canvas.NewText("", color.NRGBA{R: 0x80, G: 0x84, B: 0x8c, A: 0xff})
	countLabel.TextSize = 10
	countLabel.Alignment = fyne.TextAlignTrailing

	filterBtn := widget.NewSelect([]string{"All", "Photos", "Videos"}, onFilter)
	filterBtn.SetSelected("All")

	t := &Toolbar{
		pathLabel:  pathLabel,
		countLabel: countLabel,
		filterBtn:  filterBtn,
		progress:   widget.NewActivity(),
		rebuild:    iconButton(theme.ViewRefreshIcon(), onRebuild),
		settings:   iconButton(theme.SettingsIcon(), onSettings),
		importBtn:  iconButton(theme.DownloadIcon(), onImport),
	}
	t.progress.Hide()

	actions := container.NewHBox(t.countLabel, t.filterBtn, t.progress, t.importBtn, t.settings, t.rebuild)
	row := container.New(layout.NewBorderLayout(nil, nil, nil, actions),
		actions,
		container.NewPadded(t.pathLabel),
	)

	sep := canvas.NewRectangle(color.NRGBA{R: 0x26, G: 0x2a, B: 0x31, A: 0xff})
	sep.SetMinSize(fyne.NewSize(0, 1))

	t.root = container.NewVBox(container.NewPadded(row), sep)
	return t
}

func iconButton(icon fyne.Resource, fn func()) *widget.Button {
	b := widget.NewButtonWithIcon("", icon, fn)
	b.Importance = widget.LowImportance
	return b
}

func (t *Toolbar) Widget() fyne.CanvasObject { return t.root }

// SetPath / SetCount / ShowBusy mutate widget state directly. Callers running
// off the main goroutine must wrap the call in fyne.Do.
func (t *Toolbar) SetPath(p string) {
	t.pathLabel.Text = prettyPath(p)
	t.pathLabel.Refresh()
}

func (t *Toolbar) SetCount(n int) {
	t.countLabel.Text = formatCount(n)
	t.countLabel.Refresh()
}

func (t *Toolbar) ShowBusy(busy bool) {
	if busy {
		t.progress.Show()
		t.progress.Start()
		t.rebuild.Disable()
	} else {
		t.progress.Stop()
		t.progress.Hide()
		t.rebuild.Enable()
	}
}

// prettyPath shortens a path for display: it keeps at most the trailing two
// segments, prefixed by "…/" if the original was deeper.
func prettyPath(p string) string {
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	parent, leaf := filepath.Split(cleaned)
	parent = filepath.Clean(parent)
	if parent == "." || parent == string(filepath.Separator) || parent == cleaned {
		return cleaned
	}
	parentParent, parentLeaf := filepath.Split(parent)
	parentParent = filepath.Clean(parentParent)
	if parentParent == "." || parentParent == string(filepath.Separator) || parentParent == parent {
		return filepath.Join(parentLeaf, leaf)
	}
	return "…/" + filepath.Join(parentLeaf, leaf)
}

func formatCount(n int) string {
	switch n {
	case 0:
		return "empty"
	case 1:
		return "1 item"
	}
	return itoa(n) + " items"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
