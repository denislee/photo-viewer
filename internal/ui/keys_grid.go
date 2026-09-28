package ui

import (
	"log"

	"gioui.org/app"
	"gioui.org/io/key"
	"gioui.org/io/system"

	"github.com/dns/photo-viewer/internal/cache"
)

func handleGridKey(ke key.Event, grid *Grid, _ *Sidebar, sidebarFocus *bool, viewer *Viewer, total int, ctrl *Controller, w *app.Window) {
	// While the delete-confirmation modal is up, only Enter / Esc / Ctrl+[
	// are meaningful — all other grid navigation is suppressed so a stray
	// j/k can't move the selection out from under the prompt.
	if grid.Confirming {
		switch ke.Name {
		case key.NameReturn:
			grid.ConfirmDelete(ctrl.DeletePath)
			w.Invalidate()
		case key.NameEscape:
			grid.CancelDelete()
			w.Invalidate()
		case "[":
			if ke.Modifiers.Contain(key.ModCtrl) {
				grid.CancelDelete()
				w.Invalidate()
			}
		}
		return
	}
	moved := false
	// 'gg' / 'G' vim-style jumps. Track the first 'g' on the grid; any other
	// keystroke clears the pending state so a stale 'g' doesn't fire later.
	if ke.Name == "G" {
		if ke.Modifiers.Contain(key.ModShift) {
			if grid.JumpBottom(total) {
				w.Invalidate()
			}
			grid.GPending = false
			return
		}
		if grid.GPending {
			if grid.JumpTop(total) {
				w.Invalidate()
			}
			grid.GPending = false
			return
		}
		grid.GPending = true
		return
	}
	grid.GPending = false
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
		case "F":
			if grid.PageMove(+1, total) {
				w.Invalidate()
			}
			return
		case "B":
			if grid.PageMove(-1, total) {
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
				_, _, entries, _ := ctrl.Snapshot()
				idx := grid.SelectedIndex(len(entries))
				if idx >= 0 && idx < len(entries) {
					ctrl.ToggleSelection(entries[idx].Path)
				}
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
			_, _, entries, _ := ctrl.Snapshot()
			var selected []cache.Entry
			for _, e := range entries {
				if ctrl.IsSelected(e.Path) {
					selected = append(selected, e)
				}
			}
			openExternally(selected)
			ctrl.ClearSelection()
			w.Invalidate()
			return
		}
		if total > 0 {
			_, _, entries, _ := ctrl.Snapshot()
			viewer.Show(entries, grid.SelectedIndex(len(entries)))
			w.Invalidate()
		}
	case "E":
		if ke.Modifiers.Contain(key.ModCtrl) {
			go exportFavoritesViaPicker(ctrl, w.Invalidate)
			return
		}
		if ctrl.SelectionMode() {
			_, _, entries, _ := ctrl.Snapshot()
			var selected []string
			for _, e := range entries {
				if ctrl.IsSelected(e.Path) {
					selected = append(selected, e.Path)
				}
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
						_, _ = runZenity("--warning", "--no-markup", "--title=Export selection",
							"--text="+exportSelectionSummary(copied, len(selected), target, errs))
					})
				}()
			}
			ctrl.ClearSelection()
			w.Invalidate()
		}
	case "F":
		if total > 0 {
			_, _, entries, _ := ctrl.Snapshot()
			idx := grid.SelectedIndex(len(entries))
			if idx >= 0 && idx < len(entries) {
				ctrl.ToggleFavorite(entries[idx].Path, entries[idx].Favorite)
				w.Invalidate()
			}
		}
	case "D":
		if ke.Modifiers.Contain(key.ModCtrl) {
			return
		}
		if total > 0 {
			_, _, entries, _ := ctrl.Snapshot()
			idx := grid.SelectedIndex(len(entries))
			grid.RequestDelete(entries, idx)
			w.Invalidate()
		}
	case "O", "o":
		if total > 0 {
			_, _, entries, _ := ctrl.Snapshot()
			var selected []cache.Entry
			if ctrl.SelectionMode() {
				for _, e := range entries {
					if ctrl.IsSelected(e.Path) {
						selected = append(selected, e)
					}
				}
			}
			if len(selected) == 0 {
				idx := grid.SelectedIndex(len(entries))
				if idx >= 0 && idx < len(entries) {
					selected = append(selected, entries[idx])
				}
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
			_, _, entries, _ := ctrl.Snapshot()
			idx := grid.SelectedIndex(total)
			if idx >= 0 && idx < len(entries) {
				ctrl.Select(entries[idx].Path)
			}
		}
		w.Invalidate()
	}
}
