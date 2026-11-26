package ui

import (
	"image"
	"log"
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

	"github.com/dns/photo-viewer/internal/cache"
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

// Run drives the window's event loop. It blocks until the window is closed.
// The caller must have called LoadConfig before building ctrl.
func Run(w *app.Window, ctrl *Controller) error {
	th := NewTheme()
	toolbar := NewToolbar()
	sidebar := NewSidebar()
	grid := NewGrid()
	viewer := &Viewer{ShowInfo: true}
	viewer.SetInvalidate(w.Invalidate)
	viewer.SetDisplayStore(ctrl.DisplayStore())
	dups := NewDuplicatesView(ctrl.Index(), ctrl.Store(), ctrl.Thumbs(), w.Invalidate)
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
	imports.OnFinished = ctrl.IndexImported
	organize.SetProcessRegistry(processes)
	processBar := NewProcessBar(processes)
	processBar.OnOpen = func(kind ProcKind) {
		switch kind {
		case ProcImport:
			imports.Show()
		case ProcDuplicates:
			dups.Show()
		case ProcOrganize:
			organize.Show(ctrl.Index())
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
	webView := NewWebServerView(ctrl.NewWebServer(), w.Invalidate)
	search := NewFuzzySearchView(ctrl.Index())
	search.SetInvalidate(func() { w.Invalidate() })

	wireSidebar(sidebar, search, grid, viewer, ctrl, w)

	// focusSearch is set by the Ctrl+K handler so the next layout pass can
	// route keyboard focus to the editor (must run inside a frame).
	focusSearch := false

	// sidebarFocus tracks which pane owns keyboard navigation. The grid is
	// the default; pressing 'h' from the leftmost grid column hands focus to
	// the sidebar, and Enter/'l' on a sidebar row hands it back to the grid.
	sidebarFocus := false

	wireToolbar(toolbar, ctrl, imports, dups, organize, settings, webView, w)

	// People: the review overlay, the overview pane and the person/group
	// header strip. "Open photo" from the review opens the viewer on the
	// grid's entries when the photo is among them, else on just that photo.
	review := NewReviewView(ctrl, func(path string) {
		_, _, entries, _ := ctrl.Snapshot()
		for i, e := range entries {
			if e.Path == path {
				viewer.Show(entries, i)
				return
			}
		}
		if e, ok := ctrl.Index().GetEntry(path); ok {
			viewer.Show([]cache.Entry{e}, 0)
		}
	})
	review.SetInvalidate(w.Invalidate)
	viewer.SetFaces(newViewerFaces(ctrl, func(id int64) {
		viewer.Close()
		ctrl.SelectDir(PersonDir(id))
		w.Invalidate()
	}))
	openReview := func(t ReviewTarget) {
		review.Show(t)
		w.Invalidate()
	}

	views := &appViews{
		toolbar: toolbar, sidebar: sidebar, grid: grid, viewer: viewer,
		dups: dups, imports: imports, organize: organize, settings: settings,
		search: search, sdPrompt: sdPrompt, webView: webView,
		processBar: processBar, splitter: &sidebarSplitter{},
		people: NewPeoplePane(ctrl, openReview), header: NewPersonHeader(ctrl, openReview),
		review: review,
	}
	views.modals = newModals(views, ctrl.LibraryRoot)

	ctrl.SetInvalidate(w.Invalidate)
	// Kick off the initial indexing only after both the process registry and
	// the invalidate callback are wired. Starting it earlier let SelectDir's
	// background goroutines read c.invalidate while SetInvalidate was still
	// racing to publish it (and it still surfaces in the process bar).
	go ctrl.SelectDir(ctrl.LibraryRoot())
	// Face recognition: probe the helper off the UI goroutine (1–15 s) and,
	// when it works and the setting is on, start detecting.
	ctrl.StartFaces()
	debugStartView(ctrl, views, w)

	var ops op.Ops
	for {
		ev := w.Event()
		switch e := ev.(type) {
		case app.DestroyEvent:
			ctrl.StopFaces()
			if !ctrl.DrainDeletions(3 * time.Second) {
				log.Printf("delete: exiting with deletions still in flight")
			}
			ctrl.FlushFavorites(3 * time.Second)
			if err := FlushConfig(3 * time.Second); err != nil {
				log.Printf("config: flush on exit: %v", err)
			}
			return e.Err
		case app.PinchEvent:
			if viewer.Pinch(e) {
				w.Invalidate()
			}
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			for _, r := range ctrl.TakeFavoriteReverts() {
				viewer.SetFavorite(r.Path, r.Favorite)
			}
			// If the watcher has a fresh removable device and we aren't
			// already showing a modal, surface the prompt. The prompt only
			// pops up when no other modal is active so it can't get buried.
			if views.topModal() == nil {
				if dev, ok := sdWatcher.Consume(); ok {
					sdPrompt.Show(dev)
				}
			}
			handleKeys(gtx, ctrl, views, &sidebarFocus, &focusSearch, w)
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
			drawRoot(gtx, th, ctrl, views, sidebarFocus, w)
			e.Frame(gtx.Ops)
		}
	}
}

// wireSidebar wires the sidebar, search palette and grid callbacks. The three
// pane switches all snapshot the current dir first so each pane can remember
// its own scroll position before the switch, which is why they need both the
// grid and the controller. The closures capture the views built by Run.
func wireSidebar(sidebar *Sidebar, search *FuzzySearchView, grid *Grid, viewer *Viewer, ctrl *Controller, w *app.Window) {
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
		ctrl.PreviewYear(year)
	}
	grid.OnOpen = func(idx int) {
		_, _, entries, _ := ctrl.Snapshot()
		viewer.Show(entries, idx)
		w.Invalidate()
	}
}

// wireToolbar wires every toolbar button to the view or controller action it
// opens. The background actions (rebuild, favorites export) run on their own
// goroutine so a slow disk never blocks the event loop; the config-backed
// toggles persist through UpdateConfig.
// linkSettingsToWebServer wires Settings' "Web server › Open" row: close
// Settings, then open the panel through the same path as the toolbar button,
// so only one modal is open afterwards.
func linkSettingsToWebServer(settings *SettingsView, openWebServer func()) {
	settings.OnOpenWebServer = func() {
		settings.Close()
		openWebServer()
	}
}

func wireToolbar(toolbar *Toolbar, ctrl *Controller, imports *ImportView, dups *DuplicatesView, organize *OrganizeView, settings *SettingsView, webView *WebServerView, w *app.Window) {
	toolbar.OnImport = func() {
		imports.Show()
		w.Invalidate()
	}
	toolbar.OnDuplicates = func() {
		dups.Show()
		w.Invalidate()
	}
	toolbar.OnOrganize = func() {
		organize.Show(ctrl.Index())
		w.Invalidate()
	}
	toolbar.OnExportFavorites = func() {
		go exportFavoritesViaPicker(ctrl, w.Invalidate)
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
	linkSettingsToWebServer(settings, toolbar.OnWebServer)
	toolbar.OnFilter = func(f string) {
		ctrl.SetFilter(f)
	}
	toolbar.OnShowRAW = func(v bool) {
		ctrl.SetShowRAW(v)
	}
	toolbar.OnGroupByYear = func(v bool) {
		UpdateConfig(func(c *Config) { c.GroupByYear = v })
		w.Invalidate()
	}
	toolbar.OnSortByLength = func(v bool) {
		mode := SortByName
		if v {
			mode = SortByDuration
		}
		UpdateConfig(func(c *Config) { c.SortMode = mode })
		ctrl.SetSort(mode)
		w.Invalidate()
	}
}

// rootFrame carries what drawRoot measures once per frame — the controller
// snapshot and the window/bar geometry — into the body and overlay passes.
// Bundling it keeps both helpers down to a handful of parameters instead of a
// dozen scalars (same intent as appViews, G4-09). The snapshot is taken once
// in drawRoot so both passes see the same library state.
type rootFrame struct {
	treeDir    string
	currentDir string
	entries    []cache.Entry
	subdirs    []string

	totalW   int
	totalH   int
	sbH      int // shortcut bar height, 0 when hints are disabled
	pbH      int // process bar height, 0 when no process is running
	procSnap []ProcessSnapshot
}

func drawRoot(gtx layout.Context, th *Theme, ctrl *Controller, views *appViews, sidebarFocus bool, w *app.Window) {
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
	pbH := gtx.Dp(unit.Dp(views.processBar.HeightDp(procSnap)))

	// Bottom shortcut bar — drawn over the viewer and all modals when the
	// user has opted in via Settings. Hidden by default.
	drawShortcut := func() {
		if !showShortcuts {
			return
		}
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(totalW, sbH))
		stack := op.Offset(image.Pt(0, totalH-sbH)).Push(gtx.Ops)
		drawShortcutBar(gtx2, th, views.viewer.Open, sidebarFocus, ctrl.SelectionMode())
		stack.Pop()
	}

	f := rootFrame{
		treeDir: treeDir, currentDir: currentDir,
		entries: entries, subdirs: subdirs,
		totalW: totalW, totalH: totalH,
		sbH: sbH, pbH: pbH,
		procSnap: procSnap,
	}

	// The overlays cover the whole window, so when one of them takes the frame
	// the body must not be drawn at all (see layoutOverlays).
	if layoutOverlays(gtx, th, ctrl, views, f, drawShortcut) {
		return
	}
	layoutBody(gtx, th, ctrl, views, w, f, drawShortcut)
}

// layoutOverlays draws the full-window overlays — the top modal (search
// palette, SD-card prompt, duplicates, import, organize, settings, web server)
// and the viewer — and reports whether one of them took the frame, in which
// case the body must be skipped. The shortcut bar is drawn on top of them so
// its esc/close hints stay visible.
func layoutOverlays(gtx layout.Context, th *Theme, ctrl *Controller, views *appViews, f rootFrame, drawShortcut func()) bool {
	v := views.viewer

	// Modal overlays cover the window. Drawn before the shortcut bar so it
	// stays visible at the bottom for esc/close hints. Only the top modal
	// (see newModals for the order) is drawn.
	if m := views.topModal(); m != nil {
		// A modal over a playing video must not leave it redrawing at the
		// video frame rate underneath (G5-04).
		if v.Open {
			v.SetVideoVisible(false)
		}
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(f.totalW, f.totalH-f.sbH))
		if m.name == "viewerPicker" {
			// The tag picker floats over the photo it is tagging.
			v.Layout(gtx2, th, ctrl.Thumbs())
		}
		m.layout(gtx2, th)
		drawShortcut()
		return true
	}

	// When the viewer is open it covers the window. Skip drawing the
	// background UI so its clickables don't steal keyboard focus from the
	// root key handler.
	if v.Open {
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(f.totalW, f.totalH-f.sbH))
		v.InTrash = f.currentDir == TrashView
		v.Layout(gtx2, th, ctrl.Thumbs())
		drawShortcut()
		return true
	}

	return false
}

// layoutBody draws the main screen: the toolbar, the resizable sidebar with
// its splitter, the grid, and the process and shortcut bars, plus the two
// overlays anchored to that content (the delete confirmation and the toolbar
// tooltip).
func layoutBody(gtx layout.Context, th *Theme, ctrl *Controller, views *appViews, w *app.Window, f rootFrame, drawShortcut func()) {
	tb, sb, g, v := views.toolbar, views.sidebar, views.grid, views.viewer
	processBar, splitter := views.processBar, views.splitter
	treeDir, currentDir, entries, subdirs := f.treeDir, f.currentDir, f.entries, f.subdirs
	totalW, totalH, sbH, pbH := f.totalW, f.totalH, f.sbH, f.pbH
	procSnap := f.procSnap

	drawProcBar := func() {
		if pbH == 0 {
			// Layout never runs for an empty bar, so release the finished
			// processes' row buttons here (G4-13).
			processBar.Prune(procSnap)
			return
		}
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(totalW, pbH))
		stack := op.Offset(image.Pt(0, totalH-sbH-pbH)).Push(gtx.Ops)
		processBar.Layout(gtx2, th, procSnap)
		stack.Pop()
	}

	tbH := gtx.Dp(unit.Dp(toolbarHeightDp))

	// Toolbar at top
	{
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(totalW, tbH))
		dispDir := currentDir
		switch {
		case dispDir == FavoritesView:
			dispDir = "Favorites"
		case dispDir == TrashView:
			dispDir = "Trash"
		case strings.HasPrefix(dispDir, YearViewPrefix):
			dispDir = "Year " + strings.TrimPrefix(dispDir, YearViewPrefix)
		case isPeopleDir(dispDir):
			dispDir = ctrl.PersonSentinelName(dispDir)
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
	leftW = min(max(leftW, minW), maxW)
	splitW := gtx.Dp(unit.Dp(splitterWidthDp))
	rightW := totalW - leftW - splitW

	// Sidebar
	{
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(leftW, bodyH))
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
	// Grid — or, for People, the overview pane; a person/group grid gets
	// its header strip on top.
	switch hh := gtx.Dp(unit.Dp(views.header.HeightDp(currentDir))); {
	case currentDir == PeopleView:
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(rightW, bodyH))
		stack := op.Offset(image.Pt(leftW+splitW, bodyY)).Push(gtx.Ops)
		views.people.Layout(gtx2, th)
		stack.Pop()
	case hh > 0 && hh < bodyH:
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(rightW, hh))
		stack := op.Offset(image.Pt(leftW+splitW, bodyY)).Push(gtx.Ops)
		views.header.Layout(gtx2, th, currentDir, len(entries))
		stack.Pop()
		gtx2.Constraints = layout.Exact(image.Pt(rightW, bodyH-hh))
		stack = op.Offset(image.Pt(leftW+splitW, bodyY+hh)).Push(gtx.Ops)
		g.Layout(gtx2, th, entries, ctrl)
		stack.Pop()
	default:
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(rightW, bodyH))
		stack := op.Offset(image.Pt(leftW+splitW, bodyY)).Push(gtx.Ops)
		g.Layout(gtx2, th, entries, ctrl)
		stack.Pop()
	}
	// Decode the selected photo ahead of Enter while the grid rests on it,
	// sized for the window the viewer would cover (V7).
	v.NoteWindowSize(gtx, image.Pt(totalW, totalH-sbH))
	if g.Selected >= 0 && g.Selected < len(entries) {
		v.Warm(entries[g.Selected])
	}

	// Delete-confirmation overlay covers the entire window above the
	// shortcut bar so it's prominent regardless of grid scroll position.
	if g.Confirming() {
		gtx2 := gtx
		gtx2.Constraints = layout.Exact(image.Pt(totalW, totalH-sbH))
		drawDeleteConfirm(gtx2, th, filepath.Base(g.ConfirmPath), image.Rectangle{Max: gtx2.Constraints.Max}, currentDir == TrashView)
	}

	drawProcBar()
	if msg, ok := ctrl.Toast(); ok {
		drawToast(gtx, th, msg, image.Rect(leftW+splitW, bodyY, totalW, bodyY+bodyH))
	}
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
	paint.FillShape(gtx.Ops, th.Muted, clip.Rect(line).Op())

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
			limit := max(totalW-minW-w, minW)
			newW = min(newW, maxW)
			newW = min(newW, limit)
			if newW != sp.pendingW {
				sp.pendingW = newW
				win.Invalidate()
			}
		case pointer.Release, pointer.Cancel:
			if sp.dragging {
				sp.dragging = false
				if wdp := pxToDp(gtx, sp.pendingW); GetConfig().SidebarWidthDp != wdp {
					UpdateConfig(func(c *Config) { c.SidebarWidthDp = wdp })
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
// Progress surfaces in the bottom process bar; a failure or any per-file
// failure is reported in a warning dialog (U6-B2).
func exportFavoritesViaPicker(ctrl *Controller, invalidate func()) {
	dst, err := runZenity("--file-selection", "--directory", "--title=Export favorites to…")
	if err != nil || dst == "" {
		return
	}
	ctrl.ExportFavorites(ExportFavoritesOptions{Dst: dst}, exportFavoritesDone(dst))
	if invalidate != nil {
		invalidate()
	}
}
