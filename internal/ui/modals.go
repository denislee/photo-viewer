package ui

import (
	"gioui.org/io/key"
	"gioui.org/layout"
)

// appViews bundles the window's views so drawRoot and handleKeys take one
// pointer instead of 16–19 parameters (G4-09). Built once in Run.
type appViews struct {
	toolbar    *Toolbar
	sidebar    *Sidebar
	grid       *Grid
	viewer     *Viewer
	dups       *DuplicatesView
	imports    *ImportView
	organize   *OrganizeView
	settings   *SettingsView
	search     *FuzzySearchView
	sdPrompt   *SDPromptView
	webView    *WebServerView
	processBar *ProcessBar
	splitter   *sidebarSplitter
	people     *PeoplePane
	header     *PersonHeader
	review     *ReviewView

	// modals is the full-window overlays in priority order; see newModals.
	modals []modal
}

// modal is one full-window overlay: whether it is open, how to draw it, and
// how it handles a key press while it is the top modal. keys reports whether
// the frame needs redrawing.
//
// textInput marks a modal with text editors. While it is on top, handleKeys
// registers only textInputKeyFilters, so printable keys, arrows and Return
// reach the focused editor instead of being taken as hotkeys (G6-B1): Gio
// delivers a key.Event as well as the EditEvent for a printable key, and
// handleKeys runs before the modal lays out.
type modal struct {
	name      string
	open      func() bool
	layout    func(gtx layout.Context, th *Theme)
	keys      func(ke key.Event) bool
	textInput bool
	// textInputFn, when set, decides textInput per frame (a modal whose
	// editor only shows some of the time, e.g. the review's person picker).
	textInputFn func() bool
}

// wantsText reports whether printable keys must reach the modal's editor.
func (m *modal) wantsText() bool {
	if m.textInputFn != nil {
		return m.textInputFn()
	}
	return m.textInput
}

// isCtrlBracket reports Ctrl+[, the vim-style Esc.
func isCtrlBracket(ke key.Event) bool {
	return ke.Name == "[" && ke.Modifiers.Contain(key.ModCtrl)
}

// newModals lists the overlays in priority order: when several are open, the
// first one is drawn and gets the keys. The order is the one the hand-written
// chains in drawRoot and handleKeys had. handleKeys gives the palette its keys
// before checking Ctrl+K, and every other modal after.
func newModals(v *appViews, libraryRoot func() string) []modal {
	closeOn := func(close func(), names ...key.Name) func(key.Event) bool {
		return func(ke key.Event) bool {
			for _, n := range names {
				if ke.Name == n {
					close()
					return true
				}
			}
			return false
		}
	}
	closeOrCtrlBracket := func(close func(), names ...key.Name) func(key.Event) bool {
		esc := closeOn(close, names...)
		return func(ke key.Event) bool {
			if isCtrlBracket(ke) {
				close()
				return true
			}
			return esc(ke)
		}
	}
	return []modal{
		{
			name:   "search",
			open:   func() bool { return v.search.Open },
			layout: func(gtx layout.Context, th *Theme) { v.search.Layout(gtx, th) },
			keys: func(ke key.Event) bool {
				switch ke.Name {
				case key.NameEscape:
					v.search.Close()
					return true
				case "[", "K":
					if ke.Modifiers.Contain(key.ModCtrl) {
						v.search.Close()
						return true
					}
				case key.NameDownArrow:
					return v.search.Move(1)
				case key.NameUpArrow:
					return v.search.Move(-1)
				case key.NamePageDown:
					return v.search.Move(8)
				case key.NamePageUp:
					return v.search.Move(-8)
				case key.NameReturn:
					v.search.Activate()
					return true
				}
				return false
			},
		},
		{
			// The header's "Merge into…" person picker (text input).
			name:      "personPicker",
			open:      func() bool { return v.header != nil && v.header.PickerOpen() },
			layout:    func(gtx layout.Context, th *Theme) { v.header.LayoutPicker(gtx, th) },
			keys:      v.header.Key,
			textInput: true,
		},
		{
			// The viewer's face-tag picker; layoutOverlays draws the viewer
			// under it.
			name:      "viewerPicker",
			open:      func() bool { return v.viewer != nil && v.viewer.Open && v.viewer.FacePickerOpen() },
			layout:    func(gtx layout.Context, th *Theme) { v.viewer.LayoutFacePicker(gtx, th) },
			keys:      func(ke key.Event) bool { return v.viewer.FacePickerKey(ke) },
			textInput: true,
		},
		{
			name:        "review",
			open:        func() bool { return v.review != nil && v.review.Open },
			layout:      func(gtx layout.Context, th *Theme) { v.review.Layout(gtx, th) },
			keys:        v.review.Key,
			textInputFn: func() bool { return v.review.TextInput() },
		},
		{
			name:   "duplicates",
			open:   func() bool { return v.dups.Open },
			layout: func(gtx layout.Context, th *Theme) { v.dups.Layout(gtx, th) },
			keys: func(ke key.Event) bool {
				switch ke.Name {
				case key.NameEscape, "Q":
					if !v.dups.CancelConfirm() {
						v.dups.Close()
					}
					return true
				case "J", key.NameDownArrow:
					return v.dups.Move(1)
				case "K", key.NameUpArrow:
					return v.dups.Move(-1)
				case key.NameReturn:
					v.dups.Activate()
					return true
				}
				return false
			},
		},
		{
			name:   "imports",
			open:   func() bool { return v.imports.Open },
			layout: func(gtx layout.Context, th *Theme) { v.imports.Layout(gtx, th) },
			// Esc / Ctrl+[ only: Q would be typed into the add-path editor.
			keys:      closeOrCtrlBracket(v.imports.Close, key.NameEscape),
			textInput: true,
		},
		{
			name:   "organize",
			open:   func() bool { return v.organize.Open },
			layout: func(gtx layout.Context, th *Theme) { v.organize.Layout(gtx, th, libraryRoot()) },
			keys:   closeOrCtrlBracket(v.organize.Close, key.NameEscape, "Q"),
		},
		{
			name:   "settings",
			open:   func() bool { return v.settings.Open },
			layout: func(gtx layout.Context, th *Theme) { v.settings.Layout(gtx, th) },
			// Esc / Ctrl+[ only: Q and "," would be typed into a path editor.
			keys:      closeOrCtrlBracket(v.settings.Close, key.NameEscape),
			textInput: true,
		},
		{
			name:   "sdPrompt",
			open:   func() bool { return v.sdPrompt.Open },
			layout: func(gtx layout.Context, th *Theme) { v.sdPrompt.Layout(gtx, th) },
			keys:   closeOn(v.sdPrompt.Close, key.NameEscape, "Q"),
		},
		{
			// Q would collide with the password editor, so only Esc /
			// Ctrl+[ close the modal.
			name:      "webServer",
			open:      func() bool { return v.webView.Open },
			layout:    func(gtx layout.Context, th *Theme) { v.webView.Layout(gtx, th) },
			keys:      closeOrCtrlBracket(v.webView.Close, key.NameEscape),
			textInput: true,
		},
	}
}

// topModal returns the highest-priority open overlay, or nil.
func (v *appViews) topModal() *modal {
	for i := range v.modals {
		if v.modals[i].open() {
			return &v.modals[i]
		}
	}
	return nil
}
