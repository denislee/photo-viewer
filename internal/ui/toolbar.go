package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Toolbar groups the path label, count, progress, and rebuild button.
type Toolbar struct {
	root *fyne.Container

	pathLabel  *widget.Label
	countLabel *widget.Label
	progress   *widget.ProgressBarInfinite
	rebuild    *widget.Button
	settings   *widget.Button
	importBtn  *widget.Button
}

func NewToolbar(onRebuild func(), onSettings func(), onImport func()) *Toolbar {
	t := &Toolbar{
		pathLabel:  widget.NewLabel(""),
		countLabel: widget.NewLabel(""),
		progress:   widget.NewProgressBarInfinite(),
		rebuild:    widget.NewButtonWithIcon("Rebuild index", theme.ViewRefreshIcon(), onRebuild),
		settings:   widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), onSettings),
		importBtn:  widget.NewButtonWithIcon("Import", theme.DownloadIcon(), onImport),
	}
	t.progress.Hide()
	t.root = container.NewBorder(
		nil, nil,
		container.NewHBox(t.settings, t.importBtn, t.rebuild),
		container.NewHBox(t.countLabel, t.progress),
		t.pathLabel,
	)
	return t
}

func (t *Toolbar) Widget() fyne.CanvasObject { return t.root }

// SetPath / SetCount / ShowBusy mutate widget state directly. Callers running
// off the main goroutine must wrap the call in fyne.Do.
func (t *Toolbar) SetPath(p string) { t.pathLabel.SetText(p) }
func (t *Toolbar) SetCount(n int)   { t.countLabel.SetText(formatCount(n)) }
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

func formatCount(n int) string {
	switch n {
	case 0:
		return "no items"
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
