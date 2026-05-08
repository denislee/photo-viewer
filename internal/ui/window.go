package ui

import (
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// sidebarSplitter owns the drag state for the resizable divider between the
// sidebar and the grid. It tracks the in-progress drag origin and the width at
// the start of the drag so we can compute the new width as deltaX from that
// snapshot instead of frame-to-frame deltas (which would jitter under
// sub-pixel pointer events).
type sidebarSplitter struct {
	dragging bool
	startX   float32
	startW   int
	pendingW int // committed width awaiting persistence on drag-release
}

const (
	sidebarMinDp    = 140
	sidebarMaxDp    = 600
	splitterWidthDp = 6
)

// Run is the Gio replacement for fyne.Window.ShowAndRun. It blocks until the
// window is closed.
func Run(w *app.Window, ctrl *Controller) error {
	LoadConfig()

	th := NewTheme()
	toolbar := NewToolbar()
	sidebar := NewSidebar()
	grid := NewGrid()
	viewer := &Viewer{ShowInfo: true}
	viewer.SetInvalidate(w.Invalidate)
	dups := NewDuplicatesView(ctrl.Index(), ctrl.Store(), ctrl.Thumbs(), w.Invalidate)
	imports := NewImportView(w.Invalidate)
	organize := NewOrganizeView(w.Invalidate)
	settings := NewSettingsView()
	settings.SetInvalidate(w.Invalidate)
	indexInfo := NewIndexInfoView(ctrl.IndexStatus)
	search := NewFuzzySearchView(ctrl.Index())
	search.OnPick = func(path string, isDir bool) {
		grid.Selected = 0
		if isDir {
			ctrl.SelectDir(path)
		} else {
			ctrl.SelectDir(filepath.Dir(path))
		}
		search.Close()
		w.Invalidate()
	}
	// focusSearch is set by the Ctrl+K handler so the next layout pass can
	// route keyboard focus to the editor (must run inside a frame).
	focusSearch := false

	// sidebarFocus tracks which pane owns keyboard navigation. The grid is
	// the default; pressing 'h' from the leftmost grid column hands focus to
	// the sidebar, and Enter/'l' on a sidebar row hands it back to the grid.
	sidebarFocus := false

	sidebar.OnPick = func(p string) {
		grid.Selected = 0
		ctrl.SelectDir(p)
	}
	sidebar.OnPreview = func(p string) {
		grid.Selected = 0
		ctrl.PreviewDir(p)
	}
	sidebar.OnPreviewYear = func(year string, dirs []string) {
		grid.Selected = 0
		ctrl.PreviewYear(year, dirs)
	}
	grid.OnOpen = func(idx int) {
		_, _, entries, _ := ctrl.Snapshot()
		viewer.Show(entries, idx)
		w.Invalidate()
	}
	toolbar.OnImport = func() {
		imports.Show()
		w.Invalidate()
	}
	toolbar.OnDuplicates = func() {
		dups.Show()
		w.Invalidate()
	}
	toolbar.OnOrganize = func() {
		organize.Show(ctrl.Index(), ctrl.LibraryRoot())
		w.Invalidate()
	}
	toolbar.OnSettings = func() {
		settings.Show()
		w.Invalidate()
	}
	toolbar.OnIndexInfo = func() {
		indexInfo.Show()
		w.Invalidate()
	}
	toolbar.OnRebuild = func() {
		go func() {
			if err := ctrl.Rebuild(); err != nil {
				return
			}
		}()
	}
	toolbar.OnWarmUp = func() {
		ctrl.WarmUp()
	}
	toolbar.OnFilter = func(f string) {
		ctrl.SetFilter(f)
	}
	toolbar.OnShowRAW = func(v bool) {
		ctrl.SetShowRAW(v)
	}
	toolbar.OnGroupByYear = func(v bool) {
		c := GetConfig()
		c.GroupByYear = v
		_ = SaveConfig(c)
		w.Invalidate()
	}
	toolbar.Filter = ctrl.Filter()
	toolbar.ShowRAW = ctrl.ShowRAW()
	toolbar.GroupByYear = GetConfig().GroupByYear

	splitter := &sidebarSplitter{}

	ctrl.SetInvalidate(w.Invalidate)

	var ops op.Ops
	for {
		ev := w.Event()
		switch e := ev.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			handleKeys(gtx, ctrl, grid, sidebar, viewer, dups, imports, organize, settings, indexInfo, search, &sidebarFocus, &focusSearch, w)
			if focusSearch {
				search.FocusEditor(gtx)
				focusSearch = false
			}
			sidebar.KeyboardFocus = sidebarFocus
			toolbar.Filter = ctrl.Filter()
			toolbar.ShowRAW = ctrl.ShowRAW()
			toolbar.GroupByYear = GetConfig().GroupByYear
			toolbar.Busy = ctrl.Scanning()
			drawRoot(gtx, th, ctrl, toolbar, sidebar, grid, viewer, dups, imports, organize, settings, indexInfo, search, splitter, sidebarFocus, w)
			e.Frame(gtx.Ops)
		}
	}
}

func handleKeys(gtx layout.Context, ctrl *Controller, grid *Grid, sidebar *Sidebar, viewer *Viewer, dups *DuplicatesView, imports *ImportView, organize *OrganizeView, settings *SettingsView, indexInfo *IndexInfoView, search *FuzzySearchView, sidebarFocus *bool, focusSearch *bool, w *app.Window) {
	_, _, entries, _ := ctrl.Snapshot()
	total := len(entries)

	// Filters without a Focus field deliver events globally — independent of
	// which widget currently holds keyboard focus. This is the right shape
	// for app-wide hotkeys (viewer Esc/Q, grid hjkl) since otherwise any
	// click on a widget that grabs focus would silently swallow our keys.
	//
	// When the fuzzy-search palette is open the editor needs printable keys,
	// so we register a narrow filter set: navigation + the close shortcut.
	// Letters/space fall through to the focused editor.
	filters := []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: key.NameUpArrow},
		key.Filter{Name: key.NameDownArrow},
		key.Filter{Name: key.NameReturn},
		key.Filter{Name: key.NamePageUp},
		key.Filter{Name: key.NamePageDown},
		key.Filter{Name: "K", Required: key.ModCtrl},
		key.Filter{Name: "[", Required: key.ModCtrl},
	}
	if !search.Open {
		filters = append(filters,
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
			key.Filter{Name: "O"},
			key.Filter{Name: "o"},
			key.Filter{Name: "Q"},
			key.Filter{Name: "V"},
			key.Filter{Name: "F", Required: key.ModCtrl},
			key.Filter{Name: "B", Required: key.ModCtrl},
			key.Filter{Name: "I", Required: key.ModCtrl},
			key.Filter{Name: "D", Required: key.ModCtrl},
			key.Filter{Name: "+", Required: key.ModCtrl},
			key.Filter{Name: "=", Required: key.ModCtrl},
			key.Filter{Name: "-", Required: key.ModCtrl},
		)
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
			if ke.Name == key.NameEscape || ke.Name == "Q" {
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
			handleViewerKey(ke, viewer, ctrl, w)
			continue
		}
		if *sidebarFocus {
			handleSidebarKey(ke, sidebar, sidebarFocus, w)
			continue
		}
		handleGridKey(ke, grid, sidebar, sidebarFocus, viewer, total, ctrl, w)
	}
}

func handleViewerKey(ke key.Event, viewer *Viewer, ctrl *Controller, w *app.Window) {
	// While the delete-confirmation modal is up, only Enter / Esc / Ctrl+[
	// are meaningful — every other key is a no-op so a stray hjkl can't
	// silently advance past a confirmation the user hasn't dismissed.
	if viewer.Confirming {
		switch ke.Name {
		case key.NameReturn:
			viewer.ConfirmDelete(ctrl.DeletePath)
			w.Invalidate()
		case key.NameEscape:
			viewer.CancelDelete()
			w.Invalidate()
		case "[":
			if ke.Modifiers.Contain(key.ModCtrl) {
				viewer.CancelDelete()
				w.Invalidate()
			}
		}
		return
	}
	switch ke.Name {
	case key.NameEscape, "Q":
		viewer.Close()
		w.Invalidate()
	case "[":
		if ke.Modifiers.Contain(key.ModCtrl) {
			viewer.Close()
			w.Invalidate()
		}
	case key.NameLeftArrow, "H":
		viewer.Prev()
		w.Invalidate()
	case key.NameRightArrow, "L":
		viewer.Next()
		w.Invalidate()
	case key.NameDownArrow, "J":
		viewer.Next()
		w.Invalidate()
	case key.NameUpArrow, "K":
		viewer.Prev()
		w.Invalidate()
	case "D":
		if !ke.Modifiers.Contain(key.ModCtrl) {
			viewer.RequestDelete()
			w.Invalidate()
		}
	case "I":
		viewer.ShowInfo = !viewer.ShowInfo
		w.Invalidate()
	case "O", "o":
		if viewer.Index >= 0 && viewer.Index < len(viewer.entries) {
			e := viewer.entries[viewer.Index]
			if e.Type == scan.TypeVideo {
				go func() {
					_ = exec.Command("mpv", "--loop", e.Path).Start()
				}()
			}
		}
	case "F":
		if viewer.Index >= 0 && viewer.Index < len(viewer.entries) {
			e := &viewer.entries[viewer.Index]
			e.Favorite = ctrl.ToggleFavorite(e.Path)
			w.Invalidate()
		}
	}
}

func handleGridKey(ke key.Event, grid *Grid, sidebar *Sidebar, sidebarFocus *bool, viewer *Viewer, total int, ctrl *Controller, w *app.Window) {
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
		if ctrl.SelectionMode {
			ctrl.ClearSelection()
		} else {
			ctrl.SelectionMode = true
			if total > 0 {
				_, _, entries, _ := ctrl.Snapshot()
				idx := grid.SelectedIndex(total)
				ctrl.ToggleSelection(entries[idx].Path)
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
		if ctrl.SelectionMode {
			_, _, entries, _ := ctrl.Snapshot()
			var selected []cache.Entry
			for _, e := range entries {
				if ctrl.IsSelected(e.Path) {
					selected = append(selected, e)
				}
			}
			if len(selected) > 0 {
				allVideos := true
				var paths []string
				for _, e := range selected {
					paths = append(paths, e.Path)
					if e.Type != scan.TypeVideo {
						allVideos = false
					}
				}

				if allVideos {
					go func() {
						var args []string
						if len(paths) > 1 {
							args = append([]string{"--loop-playlist=inf"}, paths...)
						} else {
							args = []string{"--loop", paths[0]}
						}
						_ = exec.Command("mpv", args...).Start()
					}()
				} else {
					for _, p := range paths {
						path := p
						go func() {
							_ = exec.Command("xdg-open", path).Start()
						}()
					}
				}
			}
			ctrl.ClearSelection()
			w.Invalidate()
			return
		}
		if total > 0 {
			_, _, entries, _ := ctrl.Snapshot()
			viewer.Show(entries, grid.SelectedIndex(total))
			w.Invalidate()
		}
	case "E":
		if ctrl.SelectionMode {
			_, _, entries, _ := ctrl.Snapshot()
			var selected []string
			for _, e := range entries {
				if ctrl.IsSelected(e.Path) {
					selected = append(selected, e.Path)
				}
			}
			if len(selected) > 0 {
				// For now, let's use a simple way to get a target dir.
				// We don't have a folder picker in this Gio UI yet.
				// I'll implement a simple one or just use zenity if available.
				go func() {
					// Use zenity to pick a directory if available
					cmd := exec.Command("zenity", "--file-selection", "--directory", "--title=Select Export Directory")
					out, err := cmd.Output()
					if err != nil {
						return
					}
					target := strings.TrimSpace(string(out))
					if target == "" {
						return
					}
					for _, src := range selected {
						dst := filepath.Join(target, filepath.Base(src))
						_ = copyFile(src, dst)
					}
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
				ctrl.ToggleFavorite(entries[idx].Path)
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
			if ctrl.SelectionMode {
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
				allVideos := true
				var paths []string
				for _, e := range selected {
					paths = append(paths, e.Path)
					if e.Type != scan.TypeVideo {
						allVideos = false
					}
				}

				if allVideos {
					go func() {
						var args []string
						if len(paths) > 1 {
							args = append([]string{"--loop-playlist=inf"}, paths...)
						} else {
							args = []string{"--loop", paths[0]}
						}
						_ = exec.Command("mpv", args...).Start()
					}()
					if ctrl.SelectionMode {
						ctrl.ClearSelection()
						w.Invalidate()
					}
				}
			}
		}
	case "Q":
		os.Exit(0)
	}
	if moved {
		if ctrl.SelectionMode {
			_, _, entries, _ := ctrl.Snapshot()
			idx := grid.SelectedIndex(total)
			if idx >= 0 && idx < len(entries) {
				ctrl.Select(entries[idx].Path)
			}
		}
		w.Invalidate()
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
		os.Exit(0)
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

func drawRoot(gtx layout.Context, th *Theme, ctrl *Controller, tb *Toolbar, sb *Sidebar, g *Grid, v *Viewer, dups *DuplicatesView, imports *ImportView, organize *OrganizeView, settings *SettingsView, indexInfo *IndexInfoView, search *FuzzySearchView, splitter *sidebarSplitter, sidebarFocus bool, w *app.Window) {
	rect := image.Rectangle{Max: gtx.Constraints.Max}
	defer clip.Rect(rect).Push(gtx.Ops).Pop()
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	treeDir, currentDir, entries, subdirs := ctrl.Snapshot()

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	sbH := gtx.Dp(unit.Dp(shortcutBarHeightDp))

	// Bottom shortcut bar — always drawn, including over the viewer.
	drawShortcut := func() {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, sbH)
		gtx2.Constraints.Min = image.Pt(totalW, sbH)
		stack := op.Offset(image.Pt(0, totalH-sbH)).Push(gtx.Ops)
		drawShortcutBar(gtx2, th, v.Open, sidebarFocus, ctrl.SelectionMode)
		stack.Pop()
	}

	// Modal overlays cover the window. Drawn before the shortcut bar so it
	// stays visible at the bottom for esc/close hints.
	if search.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		search.Layout(gtx2, th)
		drawShortcut()
		return
	}
	if dups.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		dups.Layout(gtx2, th)
		drawShortcut()
		return
	}
	if imports.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		imports.Layout(gtx2, th)
		drawShortcut()
		return
	}
	if organize.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		organize.Layout(gtx2, th, ctrl.LibraryRoot())
		drawShortcut()
		return
	}
	if settings.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		settings.Layout(gtx2, th)
		drawShortcut()
		return
	}
	if indexInfo.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		indexInfo.Layout(gtx2, th)
		drawShortcut()
		return
	}

	// When the viewer is open it covers the window. Skip drawing the
	// background UI so its clickables don't steal keyboard focus from the
	// root key handler.
	if v.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		v.Layout(gtx2, th, ctrl.Thumbs())
		drawShortcut()
		return
	}

	tbH := gtx.Dp(unit.Dp(toolbarHeightDp))

	// Toolbar at top
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, tbH)
		gtx2.Constraints.Min = image.Pt(totalW, tbH)
		dispDir := currentDir
		switch {
		case dispDir == FavoritesView:
			dispDir = "Favorites"
		case strings.HasPrefix(dispDir, YearViewPrefix):
			dispDir = "Year " + strings.TrimPrefix(dispDir, YearViewPrefix)
		}
		tb.Layout(gtx2, th, dispDir, len(entries))
	}

	// Body region below the toolbar and above the shortcut bar.
	bodyY := tbH
	bodyH := totalH - tbH - sbH
	minW := gtx.Dp(unit.Dp(sidebarMinDp))
	maxW := gtx.Dp(unit.Dp(sidebarMaxDp))
	if maxW > totalW-minW {
		maxW = totalW - minW
	}
	leftW := gtx.Dp(unit.Dp(GetConfig().SidebarWidthDp))
	if leftW <= 0 {
		leftW = totalW * 22 / 100
	}
	if leftW < minW {
		leftW = minW
	}
	if leftW > maxW {
		leftW = maxW
	}
	splitW := gtx.Dp(unit.Dp(splitterWidthDp))
	rightW := totalW - leftW - splitW

	// Sidebar
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(leftW, bodyH)
		gtx2.Constraints.Min = image.Pt(leftW, bodyH)
		stack := op.Offset(image.Pt(0, bodyY)).Push(gtx.Ops)
		sb.Layout(gtx2, th, ctrl.LibraryRoot(), treeDir, currentDir, subdirs, ctrl.DirCounts(), GetConfig().GroupByYear)
		stack.Pop()
	}
	// Splitter (drag handle) — visually a thin separator with a wider hit area
	// for the pointer. Drag updates the saved sidebar width on release so the
	// in-memory width tracks the drag immediately while disk I/O is deferred.
	{
		stack := op.Offset(image.Pt(leftW, bodyY)).Push(gtx.Ops)
		layoutSplitter(gtx, th, splitter, splitW, bodyH, leftW, totalW, minW, maxW, w)
		stack.Pop()
	}
	// Grid
	{
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(rightW, bodyH)
		gtx2.Constraints.Min = image.Pt(rightW, bodyH)
		stack := op.Offset(image.Pt(leftW+splitW, bodyY)).Push(gtx.Ops)
		g.Layout(gtx2, th, entries, ctrl)
		stack.Pop()
	}

	// Delete-confirmation overlay covers the entire window above the
	// shortcut bar so it's prominent regardless of grid scroll position.
	if g.Confirming {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		drawDeleteConfirm(gtx2, th, filepath.Base(g.ConfirmPath), image.Rectangle{Max: gtx2.Constraints.Max})
	}

	drawShortcut()
}

// layoutSplitter draws the divider strip and processes pointer drag events
// against it. The hit area is the full strip width; on drag the new sidebar
// width is computed from the absolute pointer position so the drag stays
// anchored to where the user grabbed the divider. Persistence happens once on
// release to avoid one disk write per pointer event.
func layoutSplitter(gtx layout.Context, th *Theme, sp *sidebarSplitter, w, h, leftW, totalW, minW, maxW int, win *app.Window) {
	rect := image.Rectangle{Max: image.Pt(w, h)}
	area := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	// Thin visible line in the middle of the hit strip.
	lineX := w / 2
	line := image.Rect(lineX, 0, lineX+1, h)
	cl := clip.Rect(line).Push(gtx.Ops)
	paint.ColorOp{Color: th.Muted}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	cl.Pop()

	pointer.CursorColResize.Add(gtx.Ops)
	event.Op(gtx.Ops, sp)
	area.Pop()

	for {
		ev, ok := gtx.Event(pointer.Filter{
			Target: sp,
			Kinds:  pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel,
		})
		if !ok {
			break
		}
		pe, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch pe.Kind {
		case pointer.Press:
			sp.dragging = true
			sp.startX = pe.Position.X
			sp.startW = leftW
			sp.pendingW = leftW
		case pointer.Drag:
			if !sp.dragging {
				continue
			}
			delta := int(pe.Position.X - sp.startX)
			newW := sp.startW + delta
			if newW < minW {
				newW = minW
			}
			cap := totalW - minW - w
			if cap < minW {
				cap = minW
			}
			if newW > maxW {
				newW = maxW
			}
			if newW > cap {
				newW = cap
			}
			if newW != sp.pendingW {
				sp.pendingW = newW
				c := GetConfig()
				c.SidebarWidthDp = pxToDp(gtx, newW)
				_ = SaveConfig(c)
				win.Invalidate()
			}
		case pointer.Release, pointer.Cancel:
			if sp.dragging {
				sp.dragging = false
				c := GetConfig()
				if c.SidebarWidthDp != pxToDp(gtx, sp.pendingW) {
					c.SidebarWidthDp = pxToDp(gtx, sp.pendingW)
					_ = SaveConfig(c)
				}
			}
		}
	}
}

// pxToDp converts a px length back to dp using the current gtx metric. Gio
// only exposes Dp→px so we invert manually; the rounding asymmetry is
// fine for a persisted UI width.
func pxToDp(gtx layout.Context, px int) int {
	if gtx.Metric.PxPerDp == 0 {
		return px
	}
	return int(float32(px) / gtx.Metric.PxPerDp)
}
