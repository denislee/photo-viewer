package ui

import (
	"image"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// FuzzySearchView is the Ctrl+K palette: fuzzy-matches both directory paths
// and file paths from the index, navigates to the picked entry on Enter.
type FuzzySearchView struct {
	Open bool
	// OnPick is fired when a result is activated. isDir is true for
	// directory entries; for files the caller typically navigates to the
	// parent directory. The receiver is responsible for closing the view.
	OnPick func(path string, isDir bool)

	idx *cache.Index

	editor   widget.Editor
	list     widget.List
	closeBtn widget.Clickable

	// mu guards candidates, candidatesReady and the memoization bookkeeping
	// below, which are written from the background goroutine started in Show
	// and read on the UI goroutine.
	mu              sync.Mutex
	candidatesReady bool

	// candidates is built asynchronously on Show from the current index
	// snapshot. rebuilding on every keystroke would be wasteful and the
	// dataset rarely shifts under the user during a search session.
	candidates []searchCandidate

	// Candidate memoization across palette opens (G-04). Building candidates
	// runs Index.All() over the whole table and allocates ~5 strings per
	// entry; on a large library that is a visible pause on *every* Ctrl+K.
	// cacheGen/cacheRows/builtAt record the index state the current
	// candidates were built from, so a Show can reuse them when nothing has
	// changed. See candidatesFresh for the staleness rules.
	cacheGen  uint64    // Index.Generation() captured at build time
	cacheRows int       // Index row count captured at build time
	builtAt   time.Time // when candidates were last built (zero = never)

	// countCache memoizes the SELECT COUNT(*) staleness probe so a burst of
	// opens within countTTL doesn't re-hit SQLite once per open.
	countCache int
	countAt    time.Time
	countValid bool

	// activeCandidates is a UI-goroutine-only snapshot of candidates taken
	// once per frame in recomputeResults. layoutResults and Activate use this
	// to ensure they operate on the same slice that produced v.results, even
	// if the background goroutine replaces v.candidates in the interim.
	activeCandidates []searchCandidate

	// results are indexes into activeCandidates, sorted best→worst. Recomputed
	// whenever the editor text changes between frames.
	results   []searchResult
	lastQuery string
	selected  int

	// resultsTruncated records whether the last recomputeResults hit the 500
	// result cap. The incremental-narrowing fast path (G-05) may only rescan
	// the previous result set when that set was *complete* — if the prior pass
	// was capped, candidates that would now rank into the top 500 could have
	// been dropped, so an extension must fall back to a full rescan.
	resultsTruncated bool

	rowTags []*searchRowTag

	// invalidate wakes the Gio frame loop; set via SetInvalidate.
	invalidate func()
}

type searchCandidate struct {
	path      string // absolute
	rel       string // relative to library root, used for display+matching
	base      string
	isDir     bool
	baseLower string // strings.ToLower(base), precomputed
	relLower  string // strings.ToLower(rel), precomputed
}

type searchResult struct {
	idx   int // into candidates
	score int
}

type searchRowTag struct{ idx int }

// NewFuzzySearchView wires the palette to the index. The index is read
// lazily on Show, so the view stays cheap to construct.
func NewFuzzySearchView(idx *cache.Index) *FuzzySearchView {
	v := &FuzzySearchView{idx: idx}
	v.editor.SingleLine = true
	v.editor.Submit = true
	v.list.Axis = layout.Vertical
	return v
}

// SetInvalidate registers a callback used to wake the Gio frame loop after
// the background candidate-build completes.
func (v *FuzzySearchView) SetInvalidate(f func()) { v.invalidate = f }

const (
	// searchCacheTTL bounds how long a memoized candidate list is trusted
	// with no cheaper staleness signal firing. Even when the generation and
	// row count are unchanged, a list older than this is rebuilt so an
	// in-place edit that leaves the row count identical (e.g. a file replaced
	// at the same path) can't leave the palette permanently stale.
	searchCacheTTL = 60 * time.Second
	// searchCountTTL bounds how often the row-count staleness probe hits
	// SQLite. Back-to-back opens within this window reuse the last count
	// rather than re-running SELECT COUNT(*).
	searchCountTTL = 5 * time.Second
)

// Show reveals the palette and starts building the candidate list in the
// background. The text editor is reset and refocused on each open so Ctrl+K
// always behaves like a fresh palette.
//
// The candidate list itself is memoized across opens (G-04): when the index
// hasn't changed since the last build and the cache hasn't aged out, Show
// reuses the existing candidates instead of re-running Index.All() and
// reallocating ~5 strings per entry. The editor/selection are still reset so
// the palette always *behaves* fresh; only the expensive rebuild is skipped.
func (v *FuzzySearchView) Show(libraryRoot string) {
	v.Open = true
	v.editor.SetText("")
	v.lastQuery = "\x00" // force recompute on next Layout
	v.selected = 0

	if v.candidatesFresh() {
		// Reuse the memoized list. Leave v.candidates/v.candidatesReady intact
		// so recomputeResults re-derives results from the reset query on the
		// next frame — no query, no allocation, no "Loading…" pause.
		return
	}

	v.mu.Lock()
	v.candidates = nil
	v.candidatesReady = false
	v.mu.Unlock()
	go func() {
		v.rebuildCandidates(libraryRoot)
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
}

// candidatesFresh reports whether the memoized candidate list can be reused
// without a rebuild. It is fresh when a build has completed and: the index
// generation is unchanged (no Rebuild/Clear since), the total row count is
// unchanged (no incremental add/remove), and the list is younger than
// searchCacheTTL. The row-count probe is itself TTL-cached (see rowCount) so
// rapid re-opens stay cheap.
func (v *FuzzySearchView) candidatesFresh() bool {
	if v.idx == nil {
		return false
	}
	v.mu.Lock()
	ready := v.candidatesReady
	builtAt := v.builtAt
	cacheGen := v.cacheGen
	cacheRows := v.cacheRows
	v.mu.Unlock()

	if !ready || builtAt.IsZero() {
		return false
	}
	if time.Since(builtAt) > searchCacheTTL {
		return false
	}
	if v.idx.Generation() != cacheGen {
		return false
	}
	return v.rowCount() == cacheRows
}

// rowCount returns the index row count, memoized for searchCountTTL so a burst
// of palette opens runs at most one SELECT COUNT(*). The DB is queried outside
// the mutex so a slow count never serializes UI reads of the candidate slice.
func (v *FuzzySearchView) rowCount() int {
	v.mu.Lock()
	if v.countValid && time.Since(v.countAt) < searchCountTTL {
		n := v.countCache
		v.mu.Unlock()
		return n
	}
	v.mu.Unlock()

	n := v.idx.Count()

	v.mu.Lock()
	v.countCache = n
	v.countAt = time.Now()
	v.countValid = true
	v.mu.Unlock()
	return n
}

// Close hides the palette.
func (v *FuzzySearchView) Close() {
	v.Open = false
}

func (v *FuzzySearchView) rebuildCandidates(libraryRoot string) {
	if v.idx == nil {
		v.mu.Lock()
		v.candidates = nil
		v.candidatesReady = true
		v.builtAt = time.Now()
		v.mu.Unlock()
		return
	}
	// Capture the generation *before* reading the rows so any Clear that races
	// the All() below leaves cacheGen behind the data it produced — the next
	// Show then sees the generation move and rebuilds rather than trusting a
	// snapshot that may straddle the wipe.
	gen := v.idx.Generation()
	all := v.idx.All()
	cand := make([]searchCandidate, 0, len(all)+64)
	seenDirs := make(map[string]struct{})
	for _, e := range all {
		dir := filepath.Dir(e.Path)
		if _, ok := seenDirs[dir]; !ok {
			seenDirs[dir] = struct{}{}
			rel := relTo(libraryRoot, dir)
			base := filepath.Base(dir)
			cand = append(cand, searchCandidate{
				path:      dir,
				rel:       rel,
				base:      base,
				isDir:     true,
				baseLower: strings.ToLower(base),
				relLower:  strings.ToLower(rel),
			})
		}
		rel := relTo(libraryRoot, e.Path)
		base := filepath.Base(e.Path)
		cand = append(cand, searchCandidate{
			path:      e.Path,
			rel:       rel,
			base:      base,
			isDir:     false,
			baseLower: strings.ToLower(base),
			relLower:  strings.ToLower(rel),
		})
	}
	v.mu.Lock()
	v.candidates = cand
	v.candidatesReady = true
	v.cacheGen = gen
	v.cacheRows = len(all)
	v.builtAt = time.Now()
	// Invalidate the row-count probe rather than seeding it: the first
	// staleness check after this build then re-counts, so an incremental scan
	// that adds rows without bumping the generation is picked up promptly
	// (within one COUNT(*), not one full searchCountTTL of blindness).
	v.countValid = false
	v.mu.Unlock()
}

// relTo returns path relative to root, or path itself if it's outside.
func relTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		if rel == "." {
			return filepath.Base(path)
		}
		return rel
	}
	return path
}

// recomputeResults rescans the candidate list with the current query.
// Empty query keeps everything (sorted alphabetically). Match scoring is
// a basic subsequence walk: position of first hit + summed gap between
// consecutive hits, with a bonus for matches that start in the basename.
func (v *FuzzySearchView) recomputeResults() {
	q := strings.TrimSpace(v.editor.Text())
	if q == v.lastQuery {
		return
	}
	// Capture the previous pass's state before we overwrite it — the
	// incremental fast path below interprets v.results against the candidate
	// slice they were scored on (v.activeCandidates), and needs to know both
	// the old query and whether that pass was capped.
	prevQuery := v.lastQuery
	prevResults := v.results
	prevCandidates := v.activeCandidates
	prevTruncated := v.resultsTruncated

	v.lastQuery = q
	v.selected = 0

	// Snapshot the candidate slice under the mutex so the background
	// rebuild goroutine can safely replace v.candidates concurrently.
	// activeCandidates is the UI-goroutine-only copy used by layoutResults
	// and Activate for the rest of this frame.
	v.mu.Lock()
	candidates := v.candidates
	v.mu.Unlock()
	v.activeCandidates = candidates

	if q == "" {
		out := make([]searchResult, 0, min(500, len(candidates)))
		for i, c := range candidates {
			out = append(out, searchResult{idx: i, score: len(c.rel)})
			if len(out) >= 500 {
				break
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			return candidates[out[i].idx].rel < candidates[out[j].idx].rel
		})
		v.results = out
		v.resultsTruncated = len(out) >= 500
		return
	}

	ql := strings.ToLower(q)

	// Incremental narrowing (G-05): fuzzyScore is a subsequence match, so if
	// the new query merely extends the previous one (typing forward, the
	// common case), every candidate that matches the longer query already
	// matched the shorter one. We can therefore rescan only the previous
	// result set instead of every candidate. Two guards keep this exact:
	//   - the previous pass must not have been truncated at the cap, or a
	//     candidate that would now rank into the top 500 could be missing;
	//   - the candidate slice must be the very one those results were scored
	//     against (the background rebuild may have swapped v.candidates since,
	//     in which case the old indexes no longer refer to the same entries).
	if prevQuery != "" && !prevTruncated &&
		strings.HasPrefix(q, prevQuery) &&
		sameCandidateSlice(candidates, prevCandidates) {
		out := make([]searchResult, 0, len(prevResults))
		for _, r := range prevResults {
			i := r.idx
			if i < 0 || i >= len(candidates) {
				continue
			}
			out = appendFuzzyResult(out, ql, i, candidates[i])
		}
		sortSearchResults(out, candidates)
		// A subset of a complete (untruncated) set cannot exceed the cap, so
		// the narrowed result set is likewise complete.
		v.results = out
		v.resultsTruncated = false
		return
	}

	out := make([]searchResult, 0, 256)
	for i, c := range candidates {
		out = appendFuzzyResult(out, ql, i, c)
	}
	sortSearchResults(out, candidates)
	truncated := false
	if len(out) > 500 {
		out = out[:500]
		truncated = true
	}
	v.results = out
	v.resultsTruncated = truncated
}

// appendFuzzyResult scores candidate c (at index i) against the lowercased
// query ql and appends a searchResult to out when it matches. A basename hit
// wins outright; a path-only hit is penalized (+50) so basename matches float
// to the top. Shared by the full and incremental scan paths so both assign
// identical scores.
func appendFuzzyResult(out []searchResult, ql string, i int, c searchCandidate) []searchResult {
	if ok, sc := fuzzyScore(ql, c.baseLower); ok {
		return append(out, searchResult{idx: i, score: sc})
	}
	if ok, sc := fuzzyScore(ql, c.relLower); ok {
		return append(out, searchResult{idx: i, score: sc + 50})
	}
	return out
}

// sortSearchResults orders results best→worst: lower score first, ties broken
// by relative path (which is unique per candidate, making the order total and
// deterministic). Shared so the incremental and full scans produce identical
// ordering.
func sortSearchResults(out []searchResult, candidates []searchCandidate) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return candidates[out[i].idx].rel < candidates[out[j].idx].rel
	})
}

// sameCandidateSlice reports whether a and b share the same backing array (and
// length) — i.e. the candidate snapshot has not been replaced by the
// background rebuild goroutine since the previous frame. rebuildCandidates
// always publishes a freshly allocated slice, so pointer identity of the first
// element is an exact "unchanged" signal.
func sameCandidateSlice(a, b []searchCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// fuzzyScore returns (matched, score) for query as a subsequence of s.
// Both arguments are expected to be lowercase already. Lower score is
// better; non-matches return (false, 0).
func fuzzyScore(query, s string) (bool, int) {
	if query == "" {
		return true, len(s)
	}
	qi := 0
	last := -1
	score := 0
	for i := 0; i < len(s) && qi < len(query); i++ {
		if s[i] == query[qi] {
			switch {
			case last < 0:
				score += i
			default:
				score += (i - last - 1)
			}
			last = i
			qi++
		}
	}
	if qi < len(query) {
		return false, 0
	}
	return true, score
}

// Move adjusts the selected result index by delta and clamps. Returns
// true if the selection changed.
func (v *FuzzySearchView) Move(delta int) bool {
	n := len(v.results)
	if n == 0 {
		return false
	}
	old := v.selected
	v.selected += delta
	if v.selected < 0 {
		v.selected = 0
	}
	if v.selected >= n {
		v.selected = n - 1
	}
	return v.selected != old
}

// Activate fires OnPick for the highlighted result.
func (v *FuzzySearchView) Activate() {
	if v.OnPick == nil || v.selected < 0 || v.selected >= len(v.results) {
		return
	}
	idx := v.results[v.selected].idx
	if idx < 0 || idx >= len(v.activeCandidates) {
		return
	}
	c := v.activeCandidates[idx]
	v.OnPick(c.path, c.isDir)
}

// FocusEditor tells the next layout pass to put keyboard focus on the
// text editor. Called once per Show by window.go.
func (v *FuzzySearchView) FocusEditor(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &v.editor})
}

// Layout renders the palette into the supplied constraints.
func (v *FuzzySearchView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	for {
		evt, ok := v.editor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := evt.(widget.SubmitEvent); ok {
			v.Activate()
		}
	}
	v.recomputeResults()

	// Drain row click events.
	if cap(v.rowTags) < len(v.results) {
		v.rowTags = make([]*searchRowTag, len(v.results))
	} else {
		v.rowTags = v.rowTags[:len(v.results)]
	}
	for i := range v.results {
		if v.rowTags[i] == nil {
			v.rowTags[i] = &searchRowTag{}
		}
		v.rowTags[i].idx = i
	}
	// Drain click events only for the rows laid out last frame. Results
	// outside the visible window can't have received new pointer events (they
	// weren't laid out), so polling all 500 result tags every frame is wasted
	// work — and it's paid continuously while the palette is open, exactly
	// when frames are busiest. Mirrors the grid's I-19 fix; the ±1-row margin
	// keeps clicks registering right after a fast scroll.
	start, end := visibleRange(v.list.Position.First, v.list.Position.Count, len(v.rowTags))
	for i := start; i < end; i++ {
		t := v.rowTags[i]
		for {
			ev, ok := gtx.Event(pointer.Filter{Target: t, Kinds: pointer.Press | pointer.Release})
			if !ok {
				break
			}
			if pe, ok := ev.(pointer.Event); ok && pe.Kind == pointer.Release && pe.Buttons == pointer.ButtonPrimary {
				v.selected = t.idx
				v.Activate()
			}
		}
	}

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	rect := image.Rectangle{Max: image.Pt(totalW, totalH)}
	bg := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()

	pad := layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(20), Right: unit.Dp(20)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Search")
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutSearchBox(gtx, th)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(11), v.statusLine())
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return v.layoutResults(gtx, th)
			}),
		)
	})
}

func (v *FuzzySearchView) statusLine() string {
	v.mu.Lock()
	ready := v.candidatesReady
	nCands := len(v.candidates)
	v.mu.Unlock()
	switch {
	case !ready:
		return "Loading…"
	case nCands == 0:
		return "Index is empty — scan the library first."
	case len(v.results) == 0:
		return "No matches."
	case len(v.results) == 500:
		return "500+ matches — refine your query"
	default:
		return ""
	}
}

func (v *FuzzySearchView) layoutSearchBox(gtx layout.Context, th *Theme) layout.Dimensions {
	v.editor.SingleLine = true
	w2 := max(gtx.Constraints.Max.X, 200)
	h := gtx.Dp(unit.Dp(40))
	rect := image.Rectangle{Max: image.Pt(w2, h)}
	ca := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()
	gtx.Constraints.Min = image.Pt(w2, h)
	gtx.Constraints.Max = image.Pt(w2, h)
	return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = image.Pt(gtx.Constraints.Max.X, 0)
		ed := material.Editor(th.Theme, &v.editor, "Type to fuzzy-search files and folders…")
		ed.Color = th.Foreground
		ed.HintColor = th.Muted
		ed.TextSize = unit.Sp(14)
		return ed.Layout(gtx)
	})
}

func (v *FuzzySearchView) layoutResults(gtx layout.Context, th *Theme) layout.Dimensions {
	v.mu.Lock()
	ready := v.candidatesReady
	v.mu.Unlock()
	if !ready {
		lbl := material.Label(th.Theme, unit.Sp(13), "Loading…")
		lbl.Color = th.Muted
		return lbl.Layout(gtx)
	}
	if len(v.results) == 0 {
		return layout.Dimensions{Size: gtx.Constraints.Min}
	}
	if v.selected >= len(v.results) {
		v.selected = len(v.results) - 1
	}
	cands := v.activeCandidates
	return v.list.Layout(gtx, len(v.results), func(gtx layout.Context, i int) layout.Dimensions {
		idx := v.results[i].idx
		if idx < 0 || idx >= len(cands) {
			return layout.Dimensions{}
		}
		c := cands[idx]
		return v.drawResultRow(gtx, th, c, i == v.selected, v.rowTags[i])
	})
}

func (v *FuzzySearchView) drawResultRow(gtx layout.Context, th *Theme, c searchCandidate, focused bool, tag *searchRowTag) layout.Dimensions {
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(10), Right: unit.Dp(10)}
	macro := op.Record(gtx.Ops)
	dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				prefix := "  "
				if c.isDir {
					prefix = "📁 "
				}
				lbl := material.Label(th.Theme, unit.Sp(13), prefix+c.base)
				lbl.Color = th.Foreground
				lbl.Font.Weight = 600
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(10), c.rel)
				lbl.Color = th.Muted
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
		)
	})
	textCall := macro.Stop()

	w := gtx.Constraints.Max.X
	h := dims.Size.Y
	rect := image.Rectangle{Max: image.Pt(w, h)}
	ca := clip.Rect(rect).Push(gtx.Ops)
	bg := th.Background
	if focused {
		bg = th.SelectionBG
	}
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	textCall.Add(gtx.Ops)

	event.Op(gtx.Ops, tag)
	pointer.CursorPointer.Add(gtx.Ops)
	ca.Pop()
	return layout.Dimensions{Size: image.Pt(w, h)}
}
