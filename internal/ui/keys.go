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

	// textInputKeyFilters is registered while a modal with text editors
	// (Settings, Import, Web server) is on top: only its close keys and the
	// palette shortcut, so everything else reaches the focused editor.
	textInputKeyFilters = []event.Filter{
		key.Filter{Name: key.NameEscape},
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
		// People: review verdicts, rename, undo, viewer face boxes/tags.
		key.Filter{Name: "C"},
		key.Filter{Name: "N"},
		key.Filter{Name: "N", Required: key.ModShift},
		key.Filter{Name: "P"},
		key.Filter{Name: "R"},
		key.Filter{Name: "S"},
		key.Filter{Name: "T"},
		key.Filter{Name: "X"},
		key.Filter{Name: "Y"},
		key.Filter{Name: "Y", Required: key.ModShift},
		key.Filter{Name: "Z", Required: key.ModCtrl},
	)
)

// keyFiltersFor picks the pre-built static filter set for this frame. The
// palette needs printable keys, so it uses the base set; a modal with text
// editors gets only its close keys; everything else gets the full set.
func keyFiltersFor(views *appViews) []event.Filter {
	top := views.topModal()
	switch {
	case top == nil:
		// The person header's name editor needs printable keys too.
		if views.header != nil && views.header.Editing() {
			return textInputKeyFilters
		}
		return fullKeyFilters
	case top.name == "search":
		return baseKeyFilters
	case top.wantsText():
		return textInputKeyFilters
	}
	return fullKeyFilters
}

func handleKeys(gtx layout.Context, ctrl *Controller, views *appViews, sidebarFocus *bool, focusSearch *bool, w *app.Window) {
	grid, sidebar, viewer := views.grid, views.sidebar, views.viewer
	settings, imports, dups, search := views.settings, views.imports, views.dups, views.search
	filters := keyFiltersFor(views)
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
		// The top modal is re-read per event: a key may have just closed one.
		top := views.topModal()
		if top != nil && top.name == "search" {
			if top.keys(ke) {
				w.Invalidate()
			}
			continue
		}
		// Ctrl+K opens the fuzzy palette from anywhere, over any other modal.
		if ke.Modifiers.Contain(key.ModCtrl) && ke.Name == "K" {
			search.Show(ctrl.LibraryRoot())
			*focusSearch = true
			w.Invalidate()
			continue
		}
		if top != nil {
			if top.keys(ke) {
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
		if views.header != nil && views.header.Editing() {
			if views.header.Key(ke) {
				w.Invalidate()
			}
			continue
		}
		if *sidebarFocus {
			handleSidebarKey(ke, sidebar, sidebarFocus, w)
			continue
		}
		if handlePeopleKey(ke, ctrl, views, sidebarFocus) {
			w.Invalidate()
			continue
		}
		// Snapshot per event (it is just a locked read of the slice header):
		// an earlier key in the same batch may have changed the entries, e.g.
		// F patching a favorite.
		_, _, entries, _ := ctrl.Snapshot()
		handleGridKey(ke, grid, sidebarFocus, viewer, entries, ctrl, w)
	}
}

// pager is the shared keyboard-navigation surface of the grid and the sidebar.
// handlePagerKey drives the vim-style gg/G jumps and the Ctrl+F/Ctrl+B page
// moves through it, so both key handlers run the same state machine.
type pager interface {
	// GPending reports whether a lone 'g' is waiting for its second.
	GPending() bool
	// SetGPending records or clears the pending 'g'.
	SetGPending(pending bool)
	// JumpTop and JumpBottom move the selection to the first/last row,
	// reporting whether it moved.
	JumpTop() bool
	JumpBottom() bool
	// PageMove moves the selection by one viewport-worth of rows.
	PageMove(dir int) bool
	// Changed is called after the selection moved, so the pager can preview
	// the new row and wake the frame loop.
	Changed()
}

// gridPager adapts Grid to pager for handleGridKey; total is the entry count
// its jumps and page moves clamp to.
type gridPager struct {
	g     *Grid
	total int
	w     *app.Window
}

func (p gridPager) GPending() bool           { return p.g.GPending }
func (p gridPager) SetGPending(pending bool) { p.g.GPending = pending }
func (p gridPager) JumpTop() bool            { return p.g.JumpTop(p.total) }
func (p gridPager) JumpBottom() bool         { return p.g.JumpBottom(p.total) }
func (p gridPager) PageMove(dir int) bool    { return p.g.PageMove(dir, p.total) }
func (p gridPager) Changed()                 { p.w.Invalidate() }

// sidebarPager adapts Sidebar to pager for handleSidebarKey.
type sidebarPager struct {
	s *Sidebar
	w *app.Window
}

func (p sidebarPager) GPending() bool           { return p.s.GPending }
func (p sidebarPager) SetGPending(pending bool) { p.s.GPending = pending }
func (p sidebarPager) JumpTop() bool            { return p.s.JumpTop() }
func (p sidebarPager) JumpBottom() bool         { return p.s.JumpBottom() }
func (p sidebarPager) PageMove(dir int) bool    { return p.s.PageMove(dir) }
func (p sidebarPager) Changed() {
	p.s.Preview()
	p.w.Invalidate()
}

// handlePagerKey runs the gg/G and Ctrl+F/Ctrl+B state machine shared by the
// grid and sidebar handlers, and reports whether it consumed the keystroke.
// Any other key clears a pending 'g' so a stale one can't fire later.
func handlePagerKey(ke key.Event, p pager) bool {
	if ke.Name == "G" {
		if ke.Modifiers.Contain(key.ModShift) {
			if p.JumpBottom() {
				p.Changed()
			}
			p.SetGPending(false)
			return true
		}
		if p.GPending() {
			if p.JumpTop() {
				p.Changed()
			}
			p.SetGPending(false)
			return true
		}
		p.SetGPending(true)
		return true
	}
	p.SetGPending(false)
	if ke.Modifiers.Contain(key.ModCtrl) {
		switch ke.Name {
		case "F":
			if p.PageMove(+1) {
				p.Changed()
			}
			return true
		case "B":
			if p.PageMove(-1) {
				p.Changed()
			}
			return true
		}
	}
	return false
}

// handleConfirmKey routes one key event while a delete-confirmation modal is
// up over the viewer or the grid: Enter confirms, Esc and Ctrl+[ cancel.
// Every other key is swallowed so a stray hjkl can't advance past the prompt.
func handleConfirmKey(ke key.Event, confirm, cancel func(), w *app.Window) {
	switch ke.Name {
	case key.NameReturn:
		confirm()
		w.Invalidate()
	case key.NameEscape:
		cancel()
		w.Invalidate()
	case "[":
		if ke.Modifiers.Contain(key.ModCtrl) {
			cancel()
			w.Invalidate()
		}
	}
}

func handleSidebarKey(ke key.Event, sidebar *Sidebar, sidebarFocus *bool, w *app.Window) {
	moved := false
	// 'gg' / 'G' vim-style jumps and Ctrl+F / Ctrl+B paging share their state
	// machine with the grid.
	if handlePagerKey(ke, sidebarPager{s: sidebar, w: w}) {
		return
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
