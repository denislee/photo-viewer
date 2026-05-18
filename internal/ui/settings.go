package ui

import (
	"fmt"
	"sync"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// SettingsView is a minimal settings overlay for the Gio build. It exposes
// the inbox/outbox paths used by the import flow and persists them via the
// shared Config.
type SettingsView struct {
	Open    bool
	OnClose func()

	inboxEditor     widget.Editor
	outboxEditor    widget.Editor
	inboxBrowseBtn  widget.Clickable
	outboxBrowseBtn widget.Clickable
	saveBtn         widget.Clickable
	closeBtn        widget.Clickable
	emptyTrashBtn   widget.Clickable
	sdAutoDetect    widget.Bool
	showShortcuts   widget.Bool

	// ctrl is wired by window.go so the Empty Trash button can read trash
	// stats and trigger the wipe. Optional — when nil, the trash row is
	// hidden.
	ctrl *Controller

	mu        sync.Mutex
	statusMsg string
	trashMsg  string

	invalidate func()
}

// SetController gives the settings view a handle to the controller for
// trash management. Call once at startup.
func (v *SettingsView) SetController(c *Controller) { v.ctrl = c }

// SetInvalidate registers the redraw callback so background folder-picker
// goroutines can refresh the editors after the user picks a path.
func (v *SettingsView) SetInvalidate(f func()) { v.invalidate = f }

// NewSettingsView constructs the overlay.
func NewSettingsView() *SettingsView {
	v := &SettingsView{}
	v.inboxEditor.SingleLine = true
	v.outboxEditor.SingleLine = true
	return v
}

// Show populates the editors from the persisted config and reveals the modal.
func (v *SettingsView) Show() {
	c := GetConfig()
	v.inboxEditor.SetText(c.InboxDir)
	v.outboxEditor.SetText(c.OutboxDir)
	v.sdAutoDetect.Value = c.SDCardAutoDetect
	v.showShortcuts.Value = c.ShowShortcutHints
	v.statusMsg = "Config file: " + configPath()
	v.trashMsg = v.trashStatus()
	v.Open = true
}

// trashStatus formats the current trash usage for display next to the Empty
// Trash button. Returns "" when no controller is wired.
func (v *SettingsView) trashStatus() string {
	if v.ctrl == nil {
		return ""
	}
	count, bytes := v.ctrl.TrashStats()
	if count == 0 {
		return "Trash is empty"
	}
	return fmt.Sprintf("Trash: %d item(s), %s", count, humanBytes(bytes))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Close hides the modal.
func (v *SettingsView) Close() {
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

// Layout draws the overlay over the supplied constraints.
func (v *SettingsView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.inboxBrowseBtn.Clicked(gtx) {
		go v.pickDir(&v.inboxEditor, "Select inbox directory")
	}
	if v.outboxBrowseBtn.Clicked(gtx) {
		go v.pickDir(&v.outboxEditor, "Select outbox directory")
	}
	if v.saveBtn.Clicked(gtx) {
		c := GetConfig()
		c.InboxDir = v.inboxEditor.Text()
		c.OutboxDir = v.outboxEditor.Text()
		c.SDCardAutoDetect = v.sdAutoDetect.Value
		c.ShowShortcutHints = v.showShortcuts.Value
		if err := SaveConfig(c); err != nil {
			v.statusMsg = "Save failed: " + err.Error()
		} else {
			v.statusMsg = "Saved to " + configPath()
		}
	}
	// Persist the toggle immediately so the watcher reacts even if the user
	// flips it and then closes without clicking Save.
	if v.sdAutoDetect.Update(gtx) {
		c := GetConfig()
		c.SDCardAutoDetect = v.sdAutoDetect.Value
		_ = SaveConfig(c)
	}
	if v.showShortcuts.Update(gtx) {
		c := GetConfig()
		c.ShowShortcutHints = v.showShortcuts.Value
		_ = SaveConfig(c)
	}
	if v.emptyTrashBtn.Clicked(gtx) && v.ctrl != nil {
		v.mu.Lock()
		v.trashMsg = "Emptying trash..."
		v.mu.Unlock()
		v.ctrl.EmptyTrash(func(count int, bytes int64, err error) {
			v.mu.Lock()
			if err != nil {
				v.trashMsg = fmt.Sprintf("Empty trash failed: %v", err)
			} else if count == 0 {
				v.trashMsg = "Trash is empty"
			} else {
				v.trashMsg = fmt.Sprintf("Wiped %d item(s), freed %s", count, humanBytes(bytes))
			}
			v.mu.Unlock()
			if v.invalidate != nil {
				v.invalidate()
			}
		})
	}

	drawBackground(gtx, th.Background)

	pad := layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(20), Right: unit.Dp(20)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Settings")
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(11), v.statusMsg)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
			layout.Rigid(v.editorRow(th, "Inbox directory", &v.inboxEditor, "/path/to/inbox", &v.inboxBrowseBtn)),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(v.editorRow(th, "Outbox directory", &v.outboxEditor, "/path/to/outbox", &v.outboxBrowseBtn)),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.sdAutoDetect,
					"Suggest import when a USB drive or SD card is connected")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.showShortcuts,
					"Show keyboard-shortcut hints at the bottom of the window")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(v.trashRow(th)),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(material.Button(th.Theme, &v.saveBtn, "Save").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
				)
			}),
		)
	})
}

// trashRow draws the trash status label next to an Empty Trash button.
// Hidden when no controller is wired.
func (v *SettingsView) trashRow(th *Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if v.ctrl == nil {
			return layout.Dimensions{}
		}
		v.mu.Lock()
		msg := v.trashMsg
		v.mu.Unlock()
		if msg == "" {
			msg = v.trashStatus()
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), "Deleted files (trash)")
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), msg)
						lbl.Color = th.Foreground
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &v.emptyTrashBtn, "Empty Trash").Layout),
				)
			}),
		)
	}
}

func (v *SettingsView) editorRow(th *Theme, label string, ed *widget.Editor, hint string, browse *widget.Clickable) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), label)
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						ed.SingleLine = true
						editor := material.Editor(th.Theme, ed, hint)
						editor.Color = th.Foreground
						editor.HintColor = th.Muted
						return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(8)), editor.Layout)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, browse, "Browse...").Layout),
				)
			}),
		)
	}
}

// pickDir shells out to zenity for a directory picker and writes the result
// into ed on success. Runs on its own goroutine since zenity blocks. Falls
// back silently if zenity is missing — the user can still type a path.
func (v *SettingsView) pickDir(ed *widget.Editor, title string) {
	path, err := runZenity("--file-selection", "--directory", "--title="+title)
	if err != nil {
		v.mu.Lock()
		v.statusMsg = "Picker failed: " + err.Error() + " (type a path instead)"
		v.mu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
		return
	}
	if path == "" {
		return
	}
	ed.SetText(path)
	if v.invalidate != nil {
		v.invalidate()
	}
}
