package ui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/export"
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
	emptyTrashBtn      widget.Clickable
	exportFavBtn       widget.Clickable
	exportFavFlat      widget.Bool
	exportFavMove      widget.Bool
	exportFavRecompres widget.Bool
	exportFavMaxEdge   widget.Editor
	sdAutoDetect       widget.Bool
	showShortcuts      widget.Bool

	// ctrl is wired by window.go so the Empty Trash button can read trash
	// stats and trigger the wipe. Optional — when nil, the trash row is
	// hidden.
	ctrl *Controller

	mu          sync.Mutex
	statusMsg   string
	trashMsg    string
	exportFavMsg string

	// pendingInbox / pendingOutbox hold a directory the background zenity
	// picker (pickDir) chose. The picker can't call editor.SetText itself —
	// that mutates a widget.Editor the frame goroutine is laying out — so it
	// stashes the path here (guarded by mu) and Layout applies it on the UI
	// thread. nil means "nothing pending".
	pendingInbox  *string
	pendingOutbox *string

	invalidate func()
}

// pickDir targets — which editor a background directory pick applies to.
const (
	pickInbox = iota
	pickOutbox
)

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
	// statusMsg/trashMsg/exportFavMsg are also written from background
	// goroutines (pickDir, EmptyTrash, setExportMsg), so every access goes
	// through mu.
	v.mu.Lock()
	v.statusMsg = "Config file: " + configPath()
	v.trashMsg = v.trashStatus()
	v.exportFavMsg = v.favoritesStatus()
	v.mu.Unlock()
	v.Open = true
}

// favoritesStatus reports the number of favorites currently in the index,
// shown next to the Export button so the user knows what would be exported.
func (v *SettingsView) favoritesStatus() string {
	if v.ctrl == nil || v.ctrl.Index() == nil {
		return ""
	}
	n := v.ctrl.Index().CountFavorites("", true)
	if n == 0 {
		return "No favorites to export"
	}
	if n == 1 {
		return "1 favorite ready to export"
	}
	return fmt.Sprintf("%d favorites ready to export", n)
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
	// Apply any directory the background picker stashed. SetText must run on
	// the UI goroutine — doing it inside pickDir would mutate the editor while
	// this Layout reads it.
	v.mu.Lock()
	pInbox, pOutbox := v.pendingInbox, v.pendingOutbox
	v.pendingInbox, v.pendingOutbox = nil, nil
	v.mu.Unlock()
	if pInbox != nil {
		v.inboxEditor.SetText(*pInbox)
	}
	if pOutbox != nil {
		v.outboxEditor.SetText(*pOutbox)
	}
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.inboxBrowseBtn.Clicked(gtx) {
		go v.pickDir(pickInbox, "Select inbox directory")
	}
	if v.outboxBrowseBtn.Clicked(gtx) {
		go v.pickDir(pickOutbox, "Select outbox directory")
	}
	if v.saveBtn.Clicked(gtx) {
		c := GetConfig()
		c.InboxDir = v.inboxEditor.Text()
		c.OutboxDir = v.outboxEditor.Text()
		c.SDCardAutoDetect = v.sdAutoDetect.Value
		c.ShowShortcutHints = v.showShortcuts.Value
		msg := "Saved to " + configPath()
		if err := SaveConfig(c); err != nil {
			msg = "Save failed: " + err.Error()
		}
		v.mu.Lock()
		v.statusMsg = msg
		v.mu.Unlock()
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
	if v.exportFavBtn.Clicked(gtx) && v.ctrl != nil {
		opts := ExportFavoritesOptions{
			Flatten: v.exportFavFlat.Value,
			Move:    v.exportFavMove.Value,
		}
		if v.exportFavRecompres.Value {
			opts.MaxLongEdge = parseMaxEdge(v.exportFavMaxEdge.Text())
		}
		go v.pickExportDestAndRun(opts)
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
				v.mu.Lock()
				msg := v.statusMsg
				v.mu.Unlock()
				lbl := material.Label(th.Theme, unit.Sp(11), msg)
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
			layout.Rigid(v.exportFavoritesRow(th)),
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

// exportFavoritesRow renders the "Export favorites" section: copy/move and
// flatten toggles plus a button that prompts for a destination directory
// and starts the export. The status line below reflects either the current
// favorite count or the last run's result.
func (v *SettingsView) exportFavoritesRow(th *Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if v.ctrl == nil {
			return layout.Dimensions{}
		}
		v.mu.Lock()
		msg := v.exportFavMsg
		v.mu.Unlock()
		if msg == "" {
			msg = v.favoritesStatus()
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), "Export favorites")
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.exportFavFlat,
					"Flatten into target directory (default: preserve subfolders)")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.exportFavMove,
					"Move instead of copy (removes the favorite from the library)")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						cb := material.CheckBox(th.Theme, &v.exportFavRecompres,
							"Recompress for smaller files — max long edge:")
						cb.Color = th.Foreground
						return cb.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						v.exportFavMaxEdge.SingleLine = true
						ed := material.Editor(th.Theme, &v.exportFavMaxEdge, "1920")
						ed.Color = th.Foreground
						ed.HintColor = th.Muted
						gtx.Constraints.Min.X = gtx.Dp(unit.Dp(72))
						gtx.Constraints.Max.X = gtx.Dp(unit.Dp(96))
						return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(6)), ed.Layout)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), " px (RAW always copied as-is)")
						lbl.Color = th.Muted
						return lbl.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), msg)
						lbl.Color = th.Foreground
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &v.exportFavBtn, "Choose target & export").Layout),
				)
			}),
		)
	}
}

// pickExportDestAndRun pops a zenity directory picker and, if the user
// confirms, asks the controller to run the export. Status updates are
// surfaced both inline (exportFavMsg) and in the main process bar.
func (v *SettingsView) pickExportDestAndRun(opts ExportFavoritesOptions) {
	dst, err := runZenity("--file-selection", "--directory", "--title=Choose export destination")
	if err != nil {
		v.setExportMsg("Picker failed: " + err.Error())
		return
	}
	if dst == "" {
		return
	}
	opts.Dst = dst
	verb := "Copying"
	switch {
	case opts.Move:
		verb = "Moving"
	case opts.MaxLongEdge > 0:
		verb = fmt.Sprintf("Recompressing (≤%dpx)", opts.MaxLongEdge)
	}
	v.setExportMsg(verb + " favorites to " + dst + "…")
	v.ctrl.ExportFavorites(opts, func(res export.Result, err error) {
		switch {
		case err != nil:
			v.setExportMsg(fmt.Sprintf("Export failed: %v (done %d, skipped %d, failed %d)",
				err, res.Done, res.Skipped, res.Failed))
		case res.Failed > 0:
			v.setExportMsg(fmt.Sprintf("Finished: exported %d, skipped %d, failed %d → %s",
				res.Done, res.Skipped, res.Failed, dst))
		default:
			v.setExportMsg(fmt.Sprintf("Exported %d favorite(s) to %s (skipped %d)",
				res.Done, dst, res.Skipped))
		}
	})
}

// parseMaxEdge interprets the user's "max long edge" text. Empty / invalid
// / non-positive inputs default to 1920 so the recompress checkbox is safe
// even when the editor is blank.
func parseMaxEdge(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1920
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 1920
	}
	return n
}

func (v *SettingsView) setExportMsg(s string) {
	v.mu.Lock()
	v.exportFavMsg = s
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
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

// pickDir shells out to zenity for a directory picker and stashes the result
// for the target editor (pickInbox / pickOutbox). Runs on its own goroutine
// since zenity blocks, so it must not touch the widget.Editor directly — it
// records the path in a pending field that Layout applies on the UI thread.
// Falls back silently if zenity is missing — the user can still type a path.
func (v *SettingsView) pickDir(target int, title string) {
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
	v.mu.Lock()
	switch target {
	case pickInbox:
		v.pendingInbox = &path
	case pickOutbox:
		v.pendingOutbox = &path
	}
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}
