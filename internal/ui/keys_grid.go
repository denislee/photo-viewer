package ui

import (
	"log"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/io/system"

	"github.com/dns/photo-viewer/internal/cache"
)

// handleGridKey handles one key event for the grid. entries is the
// controller's current entry snapshot, taken by the caller for this event.
func handleGridKey(ke key.Event, grid *Grid, sidebarFocus *bool, viewer *Viewer, entries []cache.Entry, ctrl *Controller, w *app.Window) {
	total := len(entries)
	// While the delete-confirmation modal is up, only Enter / Esc / Ctrl+[
	// are meaningful — all other grid navigation is suppressed so a stray
	// j/k can't move the selection out from under the prompt.
	if grid.Confirming() {
		handleConfirmKey(ke, func() { grid.ConfirmDelete(ctrl.DeletePath) }, grid.CancelDelete, w)
		return
	}
	moved := false
	// 'gg' / 'G' vim-style jumps and Ctrl+F / Ctrl+B paging share their state
	// machine with the sidebar.
	if handlePagerKey(ke, gridPager{g: grid, total: total, w: w}) {
		return
	}
	// Ctrl-modified shortcuts come through with the same Name as the unmodified
	// key; route them by checking Modifiers first.
	if gridCtrlKey(ke, grid, w) {
		return
	}
	switch ke.Name {
	case "V":
		gridSelectKey(grid, entries, ctrl, total, w)
		return
	case "H", key.NameLeftArrow:
		// At the leftmost column, hand focus over to the sidebar instead
		// of clamping in place. Otherwise this is a normal grid move.
		if grid.Selected%grid.Cols() == 0 {
			*sidebarFocus = true
			w.Invalidate()
			return
		}
		moved = grid.Move(-1, 0, total)
	case "L", key.NameRightArrow:
		moved = grid.Move(1, 0, total)
	case "J", key.NameDownArrow:
		moved = grid.Move(0, 1, total)
	case "K", key.NameUpArrow:
		moved = grid.Move(0, -1, total)
	case key.NamePageDown:
		moved = grid.PageMove(+1, total)
	case key.NamePageUp:
		moved = grid.PageMove(-1, total)
	case key.NameReturn, key.NameSpace:
		if gridOpenKey(grid, viewer, entries, ctrl, total, w) {
			return
		}
	case "E":
		if gridExportKey(ke, ctrl, entries, w) {
			return
		}
	case "F":
		if total > 0 {
			idx := grid.SelectedIndex(total)
			ctrl.ToggleFavorite(entries[idx].Path, entries[idx].Favorite)
			w.Invalidate()
		}
	case "D":
		gridDeleteKey(grid, entries, total, w)
	case "O", "o":
		gridOpenExternalKey(grid, entries, ctrl, total, w)
	case "Q":
		w.Perform(system.ActionClose)
	}
	if moved {
		if ctrl.SelectionMode() {
			ctrl.Select(entries[grid.SelectedIndex(total)].Path)
		}
		w.Invalidate()
	}
}

// gridCtrlKey handles the Ctrl-modified shortcuts that share a Name with the
// unmodified key: Ctrl+= / Ctrl++ / Ctrl+- zoom the grid. It reports whether the
// key was consumed.
func gridCtrlKey(ke key.Event, grid *Grid, w *app.Window) bool {
	if !ke.Modifiers.Contain(key.ModCtrl) {
		return false
	}
	switch ke.Name {
	case "+", "=":
		if grid.Zoom(+1) {
			w.Invalidate()
		}
		return true
	case "-":
		if grid.Zoom(-1) {
			w.Invalidate()
		}
		return true
	}
	return false
}

// gridSelectKey handles 'V': leave selection mode (clearing the selection), or
// enter it with the focused entry selected.
func gridSelectKey(grid *Grid, entries []cache.Entry, ctrl *Controller, total int, w *app.Window) {
	if ctrl.SelectionMode() {
		ctrl.ClearSelection()
	} else {
		ctrl.SetSelectionMode(true)
		if total > 0 {
			ctrl.ToggleSelection(entries[grid.SelectedIndex(total)].Path)
		}
	}
	w.Invalidate()
}

// gridOpenKey handles Enter / Space: in selection mode it opens the selected
// entries externally and leaves selection mode; otherwise it opens the focused
// entry in the viewer. It reports whether the key was fully handled.
func gridOpenKey(grid *Grid, viewer *Viewer, entries []cache.Entry, ctrl *Controller, total int, w *app.Window) bool {
	if ctrl.SelectionMode() {
		openExternally(ctrl.SelectedEntries(entries))
		ctrl.ClearSelection()
		w.Invalidate()
		return true
	}
	if total > 0 {
		viewer.Show(entries, grid.SelectedIndex(total))
		w.Invalidate()
	}
	return false
}

// gridExportKey handles 'E': Ctrl+E exports the favorites, plain 'E' copies the
// selection into a user-picked directory when in selection mode. It reports
// whether the key was fully handled (the Ctrl variant) rather than falling
// through.
func gridExportKey(ke key.Event, ctrl *Controller, entries []cache.Entry, w *app.Window) bool {
	if ke.Modifiers.Contain(key.ModCtrl) {
		go exportFavoritesViaPicker(ctrl, w.Invalidate)
		return true
	}
	if ctrl.SelectionMode() {
		var selected []string
		for _, e := range ctrl.SelectedEntries(entries) {
			selected = append(selected, e.Path)
		}
		if len(selected) > 0 {
			go func() {
				target, err := runZenity("--file-selection", "--directory", "--title=Select Export Directory")
				if err != nil || target == "" {
					return
				}
				ctrl.ExportSelection(selected, target, func(copied int, errs []error) {
					if len(errs) == 0 {
						return
					}
					for _, e := range errs {
						log.Printf("export selection to %s: %v", target, e)
					}
					warnUser("Export selection", exportSelectionSummary(copied, len(selected), target, errs))
				})
			}()
		}
		ctrl.ClearSelection()
		w.Invalidate()
	}
	return false
}

// gridDeleteKey asks for confirmation before deleting the focused entry.
func gridDeleteKey(grid *Grid, entries []cache.Entry, total int, w *app.Window) {
	if total > 0 {
		grid.RequestDelete(entries, grid.SelectedIndex(total))
		w.Invalidate()
	}
}

// gridOpenExternalKey handles 'O' / 'o': open the selection, or the focused
// entry when nothing is selected, in the system's default application.
func gridOpenExternalKey(grid *Grid, entries []cache.Entry, ctrl *Controller, total int, w *app.Window) {
	if total <= 0 {
		return
	}
	var selected []cache.Entry
	if ctrl.SelectionMode() {
		selected = ctrl.SelectedEntries(entries)
	}
	if len(selected) == 0 {
		selected = append(selected, entries[grid.SelectedIndex(total)])
	}

	if len(selected) > 0 {
		openExternally(selected)
		if ctrl.SelectionMode() {
			ctrl.ClearSelection()
			w.Invalidate()
		}
	}
}
