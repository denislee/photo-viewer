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
	if ke.Modifiers.Contain(key.ModCtrl) {
		switch ke.Name {
		case "+", "=":
			if grid.Zoom(+1) {
				w.Invalidate()
			}
			return
		case "-":
			if grid.Zoom(-1) {
				w.Invalidate()
			}
			return
		}
	}
	switch ke.Name {
	case "V":
		if ctrl.SelectionMode() {
			ctrl.ClearSelection()
		} else {
			ctrl.SetSelectionMode(true)
			if total > 0 {
				ctrl.ToggleSelection(entries[grid.SelectedIndex(total)].Path)
			}
		}
		w.Invalidate()
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
		if ctrl.SelectionMode() {
			openExternally(ctrl.SelectedEntries(entries))
			ctrl.ClearSelection()
			w.Invalidate()
			return
		}
		if total > 0 {
			viewer.Show(entries, grid.SelectedIndex(total))
			w.Invalidate()
		}
	case "E":
		if ke.Modifiers.Contain(key.ModCtrl) {
			go exportFavoritesViaPicker(ctrl, w.Invalidate)
			return
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
	case "F":
		if total > 0 {
			idx := grid.SelectedIndex(total)
			ctrl.ToggleFavorite(entries[idx].Path, entries[idx].Favorite)
			w.Invalidate()
		}
	case "D":
		if total > 0 {
			grid.RequestDelete(entries, grid.SelectedIndex(total))
			w.Invalidate()
		}
	case "O", "o":
		if total > 0 {
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
