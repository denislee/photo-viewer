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
	indexInfo  *IndexInfoView
	search     *FuzzySearchView
	sdPrompt   *SDPromptView
	webView    *WebServerView
	processBar *ProcessBar
	splitter   *sidebarSplitter

	// modals is the full-window overlays in priority order; see newModals.
	modals []modal
}

// modal is one full-window overlay: whether it is open, how to draw it, and
// how it handles a key press while it is the top modal. keys reports whether
// the frame needs redrawing.
type modal struct {
	name   string
	open   func() bool
	layout func(gtx layout.Context, th *Theme)
	keys   func(ke key.Event) bool
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
	closeOrCtrlBracket := func(close func()) func(key.Event) bool {
		esc := closeOn(close, key.NameEscape, "Q")
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
			keys:   closeOrCtrlBracket(v.imports.Close),
		},
		{
			name:   "organize",
			open:   func() bool { return v.organize.Open },
			layout: func(gtx layout.Context, th *Theme) { v.organize.Layout(gtx, th, libraryRoot()) },
			keys:   closeOrCtrlBracket(v.organize.Close),
		},
		{
			name:   "settings",
			open:   func() bool { return v.settings.Open },
			layout: func(gtx layout.Context, th *Theme) { v.settings.Layout(gtx, th) },
			keys:   closeOn(v.settings.Close, key.NameEscape, "Q", ","),
		},
		{
			name:   "indexInfo",
			open:   func() bool { return v.indexInfo.Open },
			layout: func(gtx layout.Context, th *Theme) { v.indexInfo.Layout(gtx, th) },
			keys:   closeOn(v.indexInfo.Close, key.NameEscape, "Q"),
		},
		{
			name:   "sdPrompt",
			open:   func() bool { return v.sdPrompt.Open },
			layout: func(gtx layout.Context, th *Theme) { v.sdPrompt.Layout(gtx, th) },
			keys:   closeOn(v.sdPrompt.Close, key.NameEscape, "Q"),
		},
		{
			// Q would collide with the password editor, so only Esc /
			// Ctrl+[ close the modal; everything else falls through to the
			// focused editor.
			name:   "webServer",
			open:   func() bool { return v.webView.Open },
			layout: func(gtx layout.Context, th *Theme) { v.webView.Layout(gtx, th) },
			keys: func(ke key.Event) bool {
				if ke.Name == key.NameEscape || isCtrlBracket(ke) {
					v.webView.Close()
					return true
				}
				return false
			},
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
