package ui

import (
	"image"
	"path/filepath"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
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
	dups.SetDeleter(ctrl.DeletePath)
	dups.SetBatchDeleter(ctrl.DeletePaths)
	imports := NewImportView(w.Invalidate)
	organize := NewOrganizeView(w.Invalidate)
	// Wire the organize move pass to the controller's index/thumbnail
	// bookkeeping + grid refresh, the same way the duplicates view is wired to
	// the controller's delete path above. Without this the move pass renames
	// files but leaves the grid showing broken/stale entries until a rebuild.
	organize.SetMover(ctrl.ApplyMove)
	organize.SetRefresh(ctrl.RefreshActive)

	// Process registry surfaces background work (scan, warm-up, import,
	// duplicate hash, organize) in the main-screen process bar so the user
	// can pause / resume / cancel without keeping the modal open.
	processes := NewProcessRegistry(w.Invalidate)
	ctrl.SetProcessRegistry(processes)
	dups.SetProcessRegistry(processes)
	imports.SetProcessRegistry(processes)
	organize.SetProcessRegistry(processes)
	processBar := NewProcessBar(processes)
	processBar.OnOpen = func(kind ProcKind) {
		switch kind {
		case ProcImport:
			imports.Show()
		case ProcDuplicates:
			dups.Show()
		case ProcOrganize:
			organize.Show(ctrl.Index(), ctrl.LibraryRoot())
		}
		w.Invalidate()
	}
	settings := NewSettingsView()
	settings.SetInvalidate(w.Invalidate)
	settings.SetController(ctrl)
	sdPrompt := &SDPromptView{}
	sdWatcher := NewSDCardWatcher(w.Invalidate)
	sdWatcher.Start()
	defer sdWatcher.Stop()
	sdPrompt.OnImport = func(dev removableDevice, deleteSource bool) {
		imports.ShowForDevice(dev, deleteSource)
		w.Invalidate()
	}
	indexInfo := NewIndexInfoView(ctrl.IndexStatus)
	webView := NewWebServerView(ctrl.WebServer())
	search := NewFuzzySearchView(ctrl.Index())
	search.SetInvalidate(func() { w.Invalidate() })
	search.OnPick = func(path string, isDir bool) {
		_, prev, _, _ := ctrl.Snapshot()
		grid.RememberFor(prev)
		target := path
		if !isDir {
			target = filepath.Dir(path)
		}
		grid.RestoreFor(target)
		ctrl.SelectDir(target)
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
		_, prev, _, _ := ctrl.Snapshot()
		grid.RememberFor(prev)
		grid.RestoreFor(p)
		ctrl.SelectDir(p)
	}
	sidebar.OnPreview = func(p string) {
		_, prev, _, _ := ctrl.Snapshot()
		grid.RememberFor(prev)
		grid.RestoreFor(p)
		ctrl.PreviewDir(p)
	}
	sidebar.OnPreviewYear = func(year string, dirs []string) {
		_, prev, _, _ := ctrl.Snapshot()
		grid.RememberFor(prev)
		grid.RestoreFor(YearViewPrefix + year)
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
	toolbar.OnRebuild = func() {
		go func() {
			if err := ctrl.Rebuild(); err != nil {
				ctrl.surfaceError("rebuild index", err)
			}
		}()
	}
	toolbar.OnWarmUp = func() {
		ctrl.WarmUp()
	}
	toolbar.OnWebServer = func() {
		webView.Show()
		w.Invalidate()
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
	toolbar.OnSortByLength = func(v bool) {
		c := GetConfig()
		if v {
			c.SortMode = SortByDuration
		} else {
			c.SortMode = SortByName
		}
		_ = SaveConfig(c)
		ctrl.SetSort(c.SortMode)
		w.Invalidate()
	}
	toolbar.Filter = ctrl.Filter()
	toolbar.ShowRAW = ctrl.ShowRAW()
	toolbar.GroupByYear = GetConfig().GroupByYear
	toolbar.SortByLength = ctrl.Sort() == SortByDuration

	splitter := &sidebarSplitter{}

	ctrl.SetInvalidate(w.Invalidate)
	// Kick off the initial indexing only after both the process registry and
	// the invalidate callback are wired. Starting it earlier let SelectDir's
	// background goroutines read c.invalidate while SetInvalidate was still
	// racing to publish it (and it still surfaces in the process bar).
	go ctrl.SelectDir(ctrl.LibraryRoot())

	var ops op.Ops
	for {
		ev := w.Event()
		switch e := ev.(type) {
		case app.DestroyEvent:
			ctrl.FlushFavorites(3 * time.Second)
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			for _, r := range ctrl.TakeFavoriteReverts() {
				viewer.SetFavorite(r.Path, r.Favorite)
			}
			// If the watcher has a fresh removable device and we aren't
			// already showing a modal, surface the prompt. The prompt only
			// pops up when no other modal is active so it can't get buried.
			if !sdPrompt.Open && !imports.Open && !dups.Open && !organize.Open && !settings.Open && !indexInfo.Open && !search.Open && !webView.Open {
				if dev, ok := sdWatcher.Consume(); ok {
					sdPrompt.Show(dev)
				}
			}
			handleKeys(gtx, ctrl, grid, sidebar, viewer, dups, imports, organize, settings, indexInfo, search, sdPrompt, webView, &sidebarFocus, &focusSearch, w)
			if focusSearch {
				search.FocusEditor(gtx)
				focusSearch = false
			}
			sidebar.KeyboardFocus = sidebarFocus
			toolbar.Filter = ctrl.Filter()
			toolbar.ShowRAW = ctrl.ShowRAW()
			toolbar.GroupByYear = GetConfig().GroupByYear
			toolbar.SortByLength = ctrl.Sort() == SortByDuration
			toolbar.AnyProcess = processes.Count() > 0
			toolbar.WebServerRunning = webView.Running()
			drawRoot(gtx, th, ctrl, toolbar, sidebar, grid, viewer, dups, imports, organize, settings, indexInfo, search, sdPrompt, webView, processBar, splitter, sidebarFocus, w)
			e.Frame(gtx.Ops)
		}
	}
}

func drawRoot(gtx layout.Context, th *Theme, ctrl *Controller, tb *Toolbar, sb *Sidebar, g *Grid, v *Viewer, dups *DuplicatesView, imports *ImportView, organize *OrganizeView, settings *SettingsView, indexInfo *IndexInfoView, search *FuzzySearchView, sdPrompt *SDPromptView, webView *WebServerView, processBar *ProcessBar, splitter *sidebarSplitter, sidebarFocus bool, w *app.Window) {
	rect := image.Rectangle{Max: gtx.Constraints.Max}
	defer clip.Rect(rect).Push(gtx.Ops).Pop()
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	treeDir, currentDir, entries, subdirs := ctrl.Snapshot()

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	showShortcuts := GetConfig().ShowShortcutHints
	sbH := 0
	if showShortcuts {
		sbH = gtx.Dp(unit.Dp(shortcutBarHeightDp))
	}

	// Process bar — sits just above the shortcut bar and only consumes
	// space when at least one background process is running.
	procSnap := ctrl.processes.Snapshot()
	pbH := gtx.Dp(unit.Dp(processBar.HeightDp(procSnap)))

	// Bottom shortcut bar — drawn over the viewer and all modals when the
	// user has opted in via Settings. Hidden by default.
	drawShortcut := func() {
		if !showShortcuts {
			return
		}
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, sbH)
		gtx2.Constraints.Min = image.Pt(totalW, sbH)
		stack := op.Offset(image.Pt(0, totalH-sbH)).Push(gtx.Ops)
		drawShortcutBar(gtx2, th, v.Open, sidebarFocus, ctrl.SelectionMode())
		stack.Pop()
	}
	drawProcBar := func() {
		if pbH == 0 {
			return
		}
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, pbH)
		gtx2.Constraints.Min = image.Pt(totalW, pbH)
		stack := op.Offset(image.Pt(0, totalH-sbH-pbH)).Push(gtx.Ops)
		processBar.Layout(gtx2, th, procSnap)
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
	if sdPrompt.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		sdPrompt.Layout(gtx2, th)
		drawShortcut()
		return
	}
	if webView.Open {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-sbH)
		gtx2.Constraints.Min = image.Pt(totalW, totalH-sbH)
		webView.Layout(gtx2, th)
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
		v.InTrash = currentDir == TrashView
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
		case dispDir == TrashView:
			dispDir = "Trash"
		case strings.HasPrefix(dispDir, YearViewPrefix):
			dispDir = "Year " + strings.TrimPrefix(dispDir, YearViewPrefix)
		}
		tb.Layout(gtx2, th, dispDir, len(entries))
	}

	// Body region below the toolbar and above the (optional) process
	// bar + shortcut bar.
	bodyY := tbH
	bodyH := totalH - tbH - sbH - pbH
	minW := gtx.Dp(unit.Dp(sidebarMinDp))
	maxW := gtx.Dp(unit.Dp(sidebarMaxDp))
	maxW = min(maxW, totalW-minW)
	leftW := gtx.Dp(unit.Dp(GetConfig().SidebarWidthDp))
	if splitter.dragging {
		// During an active drag prefer the in-flight width so the divider
		// tracks the pointer on every invalidated frame; the config write is
		// still deferred to drag-release (see layoutSplitter).
		leftW = splitter.pendingW
	}
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
		counts, countsVer := ctrl.DirCountsWithVersion()
		sb.Layout(gtx2, th, ctrl.LibraryRoot(), treeDir, currentDir, subdirs, counts, countsVer, GetConfig().GroupByYear)
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
		drawDeleteConfirm(gtx2, th, filepath.Base(g.ConfirmPath), image.Rectangle{Max: gtx2.Constraints.Max}, currentDir == TrashView)
	}

	drawProcBar()
	drawShortcut()

	// Tooltip overlay for the toolbar — drawn last so it floats above the
	// sidebar/grid/process-bar content. Anchored to the right edge, just
	// below the toolbar.
	if tb.HoveredLabel() != "" {
		gtx2 := gtx
		gtx2.Constraints.Max = image.Pt(totalW, totalH-tbH)
		gtx2.Constraints.Min = image.Point{}
		stack := op.Offset(image.Pt(0, tbH)).Push(gtx.Ops)
		tb.DrawTooltipOverlay(gtx2, th, totalW-gtx.Dp(unit.Dp(12)))
		stack.Pop()
	}
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
			newW := max(sp.startW+delta, minW)
			cap := max(totalW-minW-w, minW)
			if newW > maxW {
				newW = maxW
			}
			if newW > cap {
				newW = cap
			}
			if newW != sp.pendingW {
				sp.pendingW = newW
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

// exportFavoritesViaPicker pops a zenity directory picker and, on confirm,
// asks the controller to copy every favorite into the chosen target while
// preserving subfolders. Always call from a goroutine — zenity blocks.
// Status surfaces in the bottom process bar; no callback is needed here.
func exportFavoritesViaPicker(ctrl *Controller, invalidate func()) {
	dst, err := runZenity("--file-selection", "--directory", "--title=Export favorites to…")
	if err != nil || dst == "" {
		return
	}
	ctrl.ExportFavorites(ExportFavoritesOptions{Dst: dst}, nil)
	if invalidate != nil {
		invalidate()
	}
}
