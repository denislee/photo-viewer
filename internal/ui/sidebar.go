package ui

import (
	"image"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// dateFolderRe matches subdir basenames the import flow produces: YYYY-MM-DD.
// Used by year-grouping to decide which subdirs to bucket under a year header.
var dateFolderRe = regexp.MustCompile(`^(\d{4})-\d{2}-\d{2}$`)

// Sidebar is a flat list of the library root + the current directory's parent
// + immediate child directories. With year-grouping on, YYYY-MM-DD child
// folders are bucketed under collapsible YYYY headers.
type Sidebar struct {
	list widget.List
	tags []*sidebarTag

	// OnPick is fired on click or Enter — descends the sidebar tree into the
	// chosen path (re-anchors the sidebar). OnPreview is fired on j/k —
	// updates the grid only; the tree stays put. OnPreviewYear fires when
	// j/k lands on a year header so the grid can show the union of all
	// photos in the year's date subfolders.
	OnPick        func(path string)
	OnPreview     func(path string)
	OnPreviewYear func(year string, dirs []string)

	// Selected is the keyboard-focused row. KeyboardFocus controls whether
	// the row is rendered with the focus highlight (only when the sidebar
	// pane currently owns app keyboard focus).
	Selected      int
	KeyboardFocus bool

	// rows mirrors what the most recent Layout pass actually rendered, in
	// order. Used by Activate / Preview / Move to translate Selected back to
	// a path and a row kind without recomputing the grouping.
	rows []sidebarRow

	// expandedYears tracks which YYYY headers are currently open. Year keys
	// are the literal year string (e.g. "2024") so they survive across
	// directory navigations — the user expanding 2024 once usually means
	// they want it to stay open.
	expandedYears map[string]bool

	// ensureVisible is set by Move so the next Layout pass scrolls the list
	// to keep Selected on screen. Scrolling has to happen during Layout
	// because we need the prior frame's Position.Count (visible-row count)
	// to know whether Selected is already on screen.
	ensureVisible bool

	// GPending mirrors Grid.GPending — set after the first 'g' so a follow-up
	// 'g' completes the vim-style jump-to-top. Cleared by the window key
	// handler on any other key.
	GPending bool

	// buildRows memo fields. buildRows is called every Layout frame, but its
	// inputs rarely change between frames. We cache the last result and return
	// it immediately when all inputs match, avoiding the O(N log N) sort and
	// per-subdir regex scan on idle frames.
	expandedRev     int // bumped by toggleYear; invalidates the memo
	memoRoot        string
	memoTreeDir     string
	memoSubLen      int
	memoSubFirst    string
	memoCountsVer   uint64 // DirCountsWithVersion() tag; bumped on every map swap
	memoGroupByYear bool
	memoExpandedRev int
	memoRows        []sidebarRow
}

// sidebarRowKind tells the click / keyboard handlers what to do with a row.
type sidebarRowKind int

const (
	// rowPath is a regular directory or sentinel (Favorites, library root).
	rowPath sidebarRowKind = iota
	// rowYear is a synthetic YYYY header. Clicking toggles expansion;
	// it does not change the active directory.
	rowYear
)

type sidebarTag struct {
	path string
	kind sidebarRowKind
	year string // populated for rowYear, identifies which bucket to toggle
}

func NewSidebar() *Sidebar {
	s := &Sidebar{expandedYears: map[string]bool{}}
	s.list.Axis = layout.Vertical
	return s
}

// visibleRange returns the [start, end) row window to drain pointer events
// for, given a vertical layout.List's Position.First/Count and the total row
// count n. Rows outside this window weren't laid out last frame and so can't
// hold a pending press; polling only the window turns O(rows) event probes
// into O(visible). The one-row margin on each side keeps a click registering
// even right after a fast scroll nudges the pressed row a row out of view.
func visibleRange(first, count, n int) (start, end int) {
	start = max(first-1, 0)
	end = min(first+count+1, n)
	if start > end {
		start = end
	}
	return start, end
}

type sidebarRow struct {
	label string
	path  string
	kind  sidebarRowKind
	year  string // populated for rowYear
	// indent is the visual indent in 12dp units; used so date subdirs nested
	// under a year header are clearly children.
	indent int
	// favorite is true for the synthetic Favorites row so the renderer can
	// draw a star icon (the unicode glyph rendered as an empty box on some
	// systems).
	favorite bool
	// trash is true for the synthetic Trash row so the renderer can draw
	// a trash-can icon instead of a star.
	trash bool
	// expanded is meaningful for rowYear — used to render the chevron.
	expanded bool
	// hasCount/count is precomputed so the renderer doesn't need the counts
	// map at draw time (year totals are summed across child date folders).
	hasCount bool
	count    int
	// yearDirs lists the date subdirs grouped under a rowYear header,
	// sorted ascending. Used by Preview to load the year's union of photos
	// into the grid without expanding the bucket.
	yearDirs []string
}

// Layout draws the sidebar entries. treeDir is the path the tree is rendered
// around (parent + listed subdirs). highlight is the path to render with the
// active-row background — typically the directory whose contents are
// currently shown in the grid. counts maps row path → recursive file count
// (filter-aware) and may be nil; missing entries render without a count.
// When groupByYear is true, child dirs whose basename matches YYYY-MM-DD are
// bucketed under collapsible YYYY headers.
func (s *Sidebar) Layout(gtx layout.Context, th *Theme, root, treeDir, highlight string, subdirs []string, counts map[string]int, countsVer uint64, groupByYear bool) layout.Dimensions {
	rows := s.buildRows(root, treeDir, subdirs, counts, countsVer, groupByYear)
	s.rows = rows

	if cap(s.tags) < len(rows) {
		s.tags = make([]*sidebarTag, len(rows))
	} else {
		s.tags = s.tags[:len(rows)]
	}
	for i := range rows {
		if s.tags[i] == nil {
			s.tags[i] = &sidebarTag{}
		}
		s.tags[i].path = rows[i].path
		s.tags[i].kind = rows[i].kind
		s.tags[i].year = rows[i].year
	}
	if s.Selected >= len(rows) {
		s.Selected = len(rows) - 1
	}
	if s.Selected < 0 {
		s.Selected = 0
	}
	// Drain click events only for the rows laid out last frame. Rows outside
	// the visible window can't have received new pointer events (they weren't
	// laid out), so polling every tag is wasted work — and with year-grouping
	// expanded the sidebar can reach thousands of rows. Mirrors the grid's
	// I-19 fix; the ±1-row margin keeps clicks registering right after a fast
	// scroll moves the pressed row a row out of the window.
	start, end := visibleRange(s.list.Position.First, s.list.Position.Count, len(s.tags))
	for i := start; i < end; i++ {
		t := s.tags[i]
		for {
			// Press must be in Kinds for Gio to track the gesture so the
			// matching Release fires on the same target.
			ev, ok := gtx.Event(pointer.Filter{Target: t, Kinds: pointer.Press | pointer.Release})
			if !ok {
				break
			}
			if pe, ok := ev.(pointer.Event); ok && pe.Kind == pointer.Release && pe.Buttons == pointer.ButtonPrimary {
				switch t.kind {
				case rowYear:
					s.toggleYear(t.year)
				default:
					if s.OnPick != nil {
						s.OnPick(t.path)
					}
				}
			}
		}
	}

	if s.ensureVisible {
		s.scrollToSelected()
		s.ensureVisible = false
	}

	return s.list.Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
		focused := s.KeyboardFocus && i == s.Selected
		r := rows[i]
		active := r.kind == rowPath && r.path == highlight
		return drawSidebarRow(gtx, th, r.label, r.count, r.hasCount, active, focused, r.indent, r.favorite, r.trash, s.tags[i])
	})
}

// buildRows materializes the visible row list, applying year-grouping when
// requested. The order is:
//   - Favorites
//   - library root
//   - ".. (parent)" if treeDir != root
//   - non-date subdirs (alphabetical)
//   - YYYY year headers in ascending order, each followed by its date
//     children when expanded
//
// When grouping is off the original "every subdir as-is" layout is restored.
func (s *Sidebar) buildRows(root, treeDir string, subdirs []string, counts map[string]int, countsVer uint64, groupByYear bool) (rows []sidebarRow) {
	// Fast path: return the cached rows when nothing has changed. countsVer is
	// the monotonic version from DirCountsWithVersion — it increments on every
	// clone-swap, so a changed map always yields a different version.
	subFirst := ""
	if len(subdirs) > 0 {
		subFirst = subdirs[0]
	}
	if root == s.memoRoot && treeDir == s.memoTreeDir &&
		len(subdirs) == s.memoSubLen && subFirst == s.memoSubFirst &&
		countsVer == s.memoCountsVer && groupByYear == s.memoGroupByYear &&
		s.expandedRev == s.memoExpandedRev {
		return s.memoRows
	}

	// On return from the slow path, update the memo so the next frame is fast.
	defer func() {
		s.memoRoot, s.memoTreeDir = root, treeDir
		s.memoSubLen, s.memoSubFirst = len(subdirs), subFirst
		s.memoCountsVer = countsVer
		s.memoGroupByYear = groupByYear
		s.memoExpandedRev = s.expandedRev
		s.memoRows = rows
	}()

	rows = []sidebarRow{
		{label: "Favorites", path: FavoritesView, kind: rowPath, favorite: true},
		{label: "Trash", path: TrashView, kind: rowPath, trash: true},
		{label: filepath.Base(root), path: root, kind: rowPath},
	}
	// Count for synthetic rows comes from the same counts map the controller
	// fills in. Favorites already participates via rowWithCount-equivalent
	// inlining further down for subdirs; do the same for our two sentinels.
	if counts != nil {
		if n, ok := counts[FavoritesView]; ok {
			rows[0].hasCount, rows[0].count = true, n
		}
		if n, ok := counts[TrashView]; ok {
			rows[1].hasCount, rows[1].count = true, n
		}
	}
	if treeDir != root {
		rows = append(rows, sidebarRow{label: ".. (parent)", path: filepath.Dir(treeDir), kind: rowPath})
	}
	rowWithCount := func(label, path string, indent int) sidebarRow {
		r := sidebarRow{label: label, path: path, kind: rowPath, indent: indent}
		if counts != nil {
			if n, ok := counts[path]; ok {
				r.hasCount, r.count = true, n
			}
		}
		return r
	}
	if !groupByYear {
		for _, d := range subdirs {
			rows = append(rows, rowWithCount(filepath.Base(d), d, 0))
		}
		return rows
	}

	// Year-grouped layout. Bucket subdirs whose basename matches YYYY-MM-DD by
	// their year; everything else falls through and renders normally above the
	// year headers so non-date folders aren't hidden by the grouping.
	type yearBucket struct {
		dirs   []string // sorted ascending so children render chronologically
		total  int      // sum of child counts (only when all children have a count)
		hasAny bool     // true if at least one child had a known count
	}
	buckets := map[string]*yearBucket{}
	var nonDate []string
	for _, d := range subdirs {
		base := filepath.Base(d)
		m := dateFolderRe.FindStringSubmatch(base)
		if m == nil {
			nonDate = append(nonDate, d)
			continue
		}
		year := m[1]
		b, ok := buckets[year]
		if !ok {
			b = &yearBucket{}
			buckets[year] = b
		}
		b.dirs = append(b.dirs, d)
		if counts != nil {
			if n, ok := counts[d]; ok {
				b.total += n
				b.hasAny = true
			}
		}
	}
	for _, d := range nonDate {
		rows = append(rows, rowWithCount(filepath.Base(d), d, 0))
	}
	years := make([]string, 0, len(buckets))
	for y := range buckets {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		b := buckets[y]
		expanded := s.expandedYears[y]
		marker := "▸"
		if expanded {
			marker = "▾"
		}
		sort.Strings(b.dirs)
		header := sidebarRow{
			label:    marker + "  " + y,
			kind:     rowYear,
			year:     y,
			expanded: expanded,
			hasCount: b.hasAny,
			count:    b.total,
			yearDirs: append([]string(nil), b.dirs...),
		}
		rows = append(rows, header)
		if !expanded {
			continue
		}
		for _, d := range b.dirs {
			rows = append(rows, rowWithCount(filepath.Base(d), d, 1))
		}
	}
	return rows
}

// toggleYear flips the expansion state for the given year header.
func (s *Sidebar) toggleYear(year string) {
	if s.expandedYears == nil {
		s.expandedYears = map[string]bool{}
	}
	s.expandedYears[year] = !s.expandedYears[year]
	s.expandedRev++
}

// Preview fires OnPreview (or OnPreviewYear for year headers) for the
// currently keyboard-selected row. Used by j/k navigation: refresh the grid
// for the highlighted row but keep the sidebar tree anchored where it is.
func (s *Sidebar) Preview() {
	if s.Selected < 0 || s.Selected >= len(s.rows) {
		return
	}
	r := s.rows[s.Selected]
	switch r.kind {
	case rowYear:
		if s.OnPreviewYear != nil {
			s.OnPreviewYear(r.year, r.yearDirs)
		}
	default:
		if s.OnPreview != nil {
			s.OnPreview(r.path)
		}
	}
}

// RowCount returns how many rows were laid out in the most recent frame.
func (s *Sidebar) RowCount() int { return len(s.rows) }

// PageMove jumps the selection by one viewport-worth of rows. Falls back
// to 8 rows on the very first frame, before list.Position.Count is known.
func (s *Sidebar) PageMove(dir int) bool {
	rows := s.list.Position.Count
	if rows < 1 {
		rows = 8
	}
	if dir > 0 {
		return s.Move(rows)
	}
	return s.Move(-rows)
}

// JumpTop moves the keyboard selection to the first row.
func (s *Sidebar) JumpTop() bool {
	if s.RowCount() == 0 || s.Selected == 0 {
		return false
	}
	s.Selected = 0
	s.ensureVisible = true
	return true
}

// JumpBottom moves the keyboard selection to the last row.
func (s *Sidebar) JumpBottom() bool {
	n := s.RowCount()
	if n == 0 || s.Selected == n-1 {
		return false
	}
	s.Selected = n - 1
	s.ensureVisible = true
	return true
}

// Move adjusts the keyboard selection by delta and clamps to bounds. Returns
// true if the selection changed.
func (s *Sidebar) Move(delta int) bool {
	n := s.RowCount()
	if n == 0 {
		return false
	}
	old := s.Selected
	s.Selected += delta
	if s.Selected < 0 {
		s.Selected = 0
	}
	if s.Selected >= n {
		s.Selected = n - 1
	}
	if s.Selected != old {
		s.ensureVisible = true
		return true
	}
	return false
}

// scrollToSelected nudges list.Position so Selected is on screen with one
// row of context above and below — start scrolling before the selection
// reaches the visible edge. Uses the previous frame's Count to decide; on
// the very first frame Count is 0 and we just align to the top, which is
// fine because nothing was visible yet.
func (s *Sidebar) scrollToSelected() {
	const margin = 1
	pos := &s.list.Position
	if s.Selected-margin < pos.First {
		pos.First = max(0, s.Selected-margin)
		pos.Offset = 0
		return
	}
	if pos.Count > 0 && s.Selected+margin >= pos.First+pos.Count {
		pos.First = max(0, s.Selected+margin-pos.Count+1)
		pos.Offset = 0
	}
}

// Activate fires OnPick for the currently keyboard-selected row, or toggles
// expansion if the selection is on a year header. Returns true when the
// caller should hand keyboard focus back to the grid (i.e. a real directory
// was picked); year toggles keep focus in the sidebar so the user can keep
// browsing.
func (s *Sidebar) Activate() bool {
	if s.Selected < 0 || s.Selected >= len(s.rows) {
		return false
	}
	r := s.rows[s.Selected]
	switch r.kind {
	case rowYear:
		s.toggleYear(r.year)
		return false
	default:
		if s.OnPick != nil {
			s.OnPick(r.path)
		}
		return true
	}
}

func drawSidebarRow(gtx layout.Context, th *Theme, label string, count int, hasCount, active, focused bool, indent int, favorite, trash bool, tag *sidebarTag) layout.Dimensions {
	leftDp := 10 + indent*14
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(float32(leftDp)), Right: unit.Dp(10)}

	totalW := gtx.Constraints.Max.X

	// Force the row to span the full pane width and lay out label + count
	// in a horizontal flex so the count is right-aligned and the label is
	// truncated (with ellipsis) instead of bleeding under the count.
	contentGtx := gtx
	contentGtx.Constraints.Min.X = totalW
	contentGtx.Constraints.Max.X = totalW

	macro := op.Record(gtx.Ops)
	dims := pad.Layout(contentGtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if th.Icons == nil {
					return layout.Dimensions{}
				}
				var ic *widget.Icon
				color := th.Muted
				switch {
				case favorite && th.Icons.Star != nil:
					ic = th.Icons.Star
					color = favoriteGold
				case trash && th.Icons.Trash != nil:
					ic = th.Icons.Trash
				}
				if ic == nil {
					return layout.Dimensions{}
				}
				side := gtx.Dp(unit.Dp(16))
				gtx.Constraints.Min = image.Pt(side, side)
				gtx.Constraints.Max = image.Pt(side, side)
				return layout.Inset{Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return ic.Layout(gtx, color)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), label)
				lbl.Color = th.Foreground
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !hasCount {
					return layout.Dimensions{}
				}
				return layout.Inset{Left: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(11), strconv.Itoa(count))
					lbl.Color = th.Muted
					return lbl.Layout(gtx)
				})
			}),
		)
	})
	contentCall := macro.Stop()

	totalH := dims.Size.Y

	rect := image.Rectangle{Max: image.Pt(totalW, totalH)}
	clipArea := clip.Rect(rect).Push(gtx.Ops)
	bg := th.Background
	if active {
		bg = th.SelectionBG
	}
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	contentCall.Add(gtx.Ops)

	if focused {
		drawBorder(gtx, rect, 2, th.Accent)
	}

	event.Op(gtx.Ops, tag)
	pointer.CursorPointer.Add(gtx.Ops)
	clipArea.Pop()
	return layout.Dimensions{Size: image.Pt(totalW, totalH)}
}
