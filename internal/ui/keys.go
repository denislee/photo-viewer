package ui

import (
	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
)

// Key-event filters are static sets, so they are built once at package init
// rather than being reconstructed on every frame (handleKeys runs each frame,
// including idle invalidations from video playback and the process bar).
//
// Filters without a Focus field deliver events globally — independent of which
// widget currently holds keyboard focus. This is the right shape for app-wide
// hotkeys (viewer Esc/Q, grid hjkl) since otherwise any click on a widget that
// grabs focus would silently swallow our keys.
//
// Both slices are read-only: gtx.Event only reads them and they are shared
// across every frame, so they must never be mutated.
var (
	// baseKeyFilters is the narrow set registered while the fuzzy-search
	// palette is open: navigation + the close shortcut. Printable keys
	// (letters/space) fall through to the focused editor.
	baseKeyFilters = []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: key.NameUpArrow},
		key.Filter{Name: key.NameDownArrow},
		key.Filter{Name: key.NameReturn},
		key.Filter{Name: key.NamePageUp},
		key.Filter{Name: key.NamePageDown},
		key.Filter{Name: "K", Required: key.ModCtrl},
		key.Filter{Name: "[", Required: key.ModCtrl},
	}

	// fullKeyFilters is baseKeyFilters plus every app-wide hotkey, registered
	// when no modal editor needs printable keys. Built by copying the base set
	// into a fresh backing array (append to a nil slice) so appending the
	// extras can never mutate baseKeyFilters.
	fullKeyFilters = append(append([]event.Filter(nil), baseKeyFilters...),
		key.Filter{Name: key.NameLeftArrow},
		key.Filter{Name: key.NameRightArrow},
		key.Filter{Name: key.NameSpace},
		key.Filter{Name: "D"},
		key.Filter{Name: "E"},
		key.Filter{Name: "F"},
		key.Filter{Name: "G"},
		key.Filter{Name: "G", Required: key.ModShift},
		key.Filter{Name: "H"},
		key.Filter{Name: "I"},
		key.Filter{Name: "J"},
		key.Filter{Name: "K"},
		key.Filter{Name: "L"},
		key.Filter{Name: "M"},
		key.Filter{Name: "O"},
		key.Filter{Name: "o"},
		key.Filter{Name: "Q"},
		key.Filter{Name: "V"},
		key.Filter{Name: "["},
		key.Filter{Name: "]"},
		key.Filter{Name: "F", Required: key.ModCtrl},
		key.Filter{Name: "B", Required: key.ModCtrl},
		key.Filter{Name: "I", Required: key.ModCtrl},
		key.Filter{Name: "D", Required: key.ModCtrl},
		key.Filter{Name: "E", Required: key.ModCtrl},
		key.Filter{Name: "+", Required: key.ModCtrl},
		key.Filter{Name: "=", Required: key.ModCtrl},
		key.Filter{Name: "-", Required: key.ModCtrl},
		key.Filter{Name: ","},
	)
)

func handleKeys(gtx layout.Context, ctrl *Controller, grid *Grid, sidebar *Sidebar, viewer *Viewer, dups *DuplicatesView, imports *ImportView, organize *OrganizeView, settings *SettingsView, indexInfo *IndexInfoView, search *FuzzySearchView, sdPrompt *SDPromptView, webView *WebServerView, sidebarFocus *bool, focusSearch *bool, w *app.Window) {
	_, _, entries, _ := ctrl.Snapshot()
	total := len(entries)

	// Select the pre-built static filter set (see baseKeyFilters /
	// fullKeyFilters). The palette needs printable keys, so it uses the base
	// set only.
	filters := fullKeyFilters
	if search.Open {
		filters = baseKeyFilters
	}
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}
		// Modal overlays consume keys before the rest of the UI sees them.
		if search.Open {
			switch ke.Name {
			case key.NameEscape:
				search.Close()
				w.Invalidate()
			case "[":
				if ke.Modifiers.Contain(key.ModCtrl) {
					search.Close()
					w.Invalidate()
				}
			case key.NameDownArrow:
				if search.Move(1) {
					w.Invalidate()
				}
			case key.NameUpArrow:
				if search.Move(-1) {
					w.Invalidate()
				}
			case key.NamePageDown:
				if search.Move(8) {
					w.Invalidate()
				}
			case key.NamePageUp:
				if search.Move(-8) {
					w.Invalidate()
				}
			case key.NameReturn:
				search.Activate()
				w.Invalidate()
			case "K":
				if ke.Modifiers.Contain(key.ModCtrl) {
					search.Close()
					w.Invalidate()
				}
			}
			continue
		}
		// Ctrl+K opens the fuzzy palette from anywhere outside a modal.
		if ke.Modifiers.Contain(key.ModCtrl) && ke.Name == "K" {
			search.Show(ctrl.LibraryRoot())
			*focusSearch = true
			w.Invalidate()
			continue
		}
		if dups.Open {
			switch ke.Name {
			case key.NameEscape, "Q":
				if !dups.CancelConfirm() {
					dups.Close()
				}
				w.Invalidate()
			case "J", key.NameDownArrow:
				if dups.Move(1) {
					w.Invalidate()
				}
			case "K", key.NameUpArrow:
				if dups.Move(-1) {
					w.Invalidate()
				}
			case key.NameReturn:
				dups.Activate()
				w.Invalidate()
			}
			continue
		}
		if imports.Open {
			switch ke.Name {
			case key.NameEscape, "Q":
				imports.Close()
				w.Invalidate()
			case "[":
				if ke.Modifiers.Contain(key.ModCtrl) {
					imports.Close()
					w.Invalidate()
				}
			}
			continue
		}
		if organize.Open {
			switch ke.Name {
			case key.NameEscape, "Q":
				organize.Close()
				w.Invalidate()
			case "[":
				if ke.Modifiers.Contain(key.ModCtrl) {
					organize.Close()
					w.Invalidate()
				}
			}
			continue
		}
		if settings.Open {
			if ke.Name == key.NameEscape || ke.Name == "Q" || ke.Name == "," {
				settings.Close()
				w.Invalidate()
			}
			continue
		}
		if indexInfo.Open {
			if ke.Name == key.NameEscape || ke.Name == "Q" {
				indexInfo.Close()
				w.Invalidate()
			}
			continue
		}
		if sdPrompt.Open {
			if ke.Name == key.NameEscape || ke.Name == "Q" {
				sdPrompt.Close()
				w.Invalidate()
			}
			continue
		}
		if webView.Open {
			// Q would collide with the password editor, so only Esc /
			// Ctrl+[ close the modal. Everything else falls through to the
			// focused editor.
			if ke.Name == key.NameEscape {
				webView.Close()
				w.Invalidate()
			} else if ke.Name == "[" && ke.Modifiers.Contain(key.ModCtrl) {
				webView.Close()
				w.Invalidate()
			}
			continue
		}
		// "," toggles the Settings overlay from anywhere outside a modal.
		if ke.Name == "," {
			settings.Show()
			w.Invalidate()
			continue
		}
		// Ctrl+I / Ctrl+D open the modals from anywhere outside themselves.
		if ke.Modifiers.Contain(key.ModCtrl) {
			switch ke.Name {
			case "I":
				imports.Show()
				w.Invalidate()
				continue
			case "D":
				dups.Show()
				w.Invalidate()
				continue
			}
		}
		if viewer.Open {
			handleViewerKey(ke, viewer, grid, ctrl, w)
			continue
		}
		if *sidebarFocus {
			handleSidebarKey(ke, sidebar, sidebarFocus, w)
			continue
		}
		handleGridKey(ke, grid, sidebar, sidebarFocus, viewer, total, ctrl, w)
	}
}

func handleSidebarKey(ke key.Event, sidebar *Sidebar, sidebarFocus *bool, w *app.Window) {
	moved := false
	if ke.Name == "G" {
		if ke.Modifiers.Contain(key.ModShift) {
			if sidebar.JumpBottom() {
				sidebar.Preview()
				w.Invalidate()
			}
			sidebar.GPending = false
			return
		}
		if sidebar.GPending {
			if sidebar.JumpTop() {
				sidebar.Preview()
				w.Invalidate()
			}
			sidebar.GPending = false
			return
		}
		sidebar.GPending = true
		return
	}
	sidebar.GPending = false
	if ke.Modifiers.Contain(key.ModCtrl) {
		switch ke.Name {
		case "F":
			if sidebar.PageMove(+1) {
				sidebar.Preview()
				w.Invalidate()
			}
			return
		case "B":
			if sidebar.PageMove(-1) {
				sidebar.Preview()
				w.Invalidate()
			}
			return
		}
	}
	switch ke.Name {
	case "J", key.NameDownArrow:
		moved = sidebar.Move(1)
	case "K", key.NameUpArrow:
		moved = sidebar.Move(-1)
	case key.NamePageDown:
		moved = sidebar.PageMove(+1)
	case key.NamePageUp:
		moved = sidebar.PageMove(-1)
	case "L", key.NameRightArrow:
		// Hand focus back to the grid without changing the directory.
		*sidebarFocus = false
		w.Invalidate()
		return
	case "H":
		// Already at the leftmost pane — no-op rather than clamping.
	case key.NameReturn, key.NameSpace:
		// Enter on a real directory commits and hands focus to the grid.
		// Enter on a year header just toggles the bucket and stays put so
		// the user can keep browsing.
		if sidebar.Activate() {
			*sidebarFocus = false
		}
		w.Invalidate()
		return
	case key.NameEscape:
		*sidebarFocus = false
		w.Invalidate()
		return
	case "Q":
		w.Perform(system.ActionClose)
	}
	if moved {
		// Auto-load the newly-highlighted directory so the grid on the right
		// previews its contents as the user scans through the tree. Preview
		// (vs Activate) keeps the tree anchored so j/k doesn't descend into
		// the row each time.
		sidebar.Preview()
		w.Invalidate()
	}
}
