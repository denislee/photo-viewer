package ui

import (
	"image"
	"path/filepath"
	"slices"
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

	// alphaTop is the empty-query result list for candidates (their
	// alphabetical top searchResultCap), built alongside them so an empty
	// palette never sorts the whole library on the UI goroutine.
	alphaTop *alphaRanking

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
	isDir     bool
	baseLower string // strings.ToLower(base), precomputed
	relLower  string // strings.ToLower(rel), precomputed
	// title is the row title, folder glyph or two-space indent plus base,
	// precomputed.
	title string
}

// newSearchCandidate builds a candidate from a path and its root-relative form,
// precomputing the lowercased match keys and the display title so the row
// renderer never formats them per frame.
func newSearchCandidate(path, rel string, isDir bool) searchCandidate {
	base := filepath.Base(path)
	title := "  " + base
	if isDir {
		title = "📁 " + base
	}
	return searchCandidate{
		path:      path,
		rel:       rel,
		isDir:     isDir,
		baseLower: strings.ToLower(base),
		relLower:  strings.ToLower(rel),
		title:     title,
	}
}

// searchResultCap bounds the palette's result list.
const searchResultCap = 500

// alphaRanking is the empty-query ranking of one candidate slice.
type alphaRanking struct {
	cands []searchCandidate // the slice top indexes into
	top   []searchResult
}

// alphaTopResults returns the n candidates with the smallest rel, in rel
// order: the empty-query listing. Ties on rel fall back to index order so the
// result is deterministic.
func alphaTopResults(cands []searchCandidate, n int) []searchResult {
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool {
		a, b := idx[i], idx[j]
		if cands[a].rel != cands[b].rel {
			return cands[a].rel < cands[b].rel
		}
		return a < b
	})
	idx = idx[:min(n, len(idx))]
	out := make([]searchResult, len(idx))
	for i, c := range idx {
		out[i] = searchResult{idx: c, score: len(cands[c].rel)}
	}
	return out
}

type searchResult struct {
	idx   int // into candidates
	score int
}

// searchRowTag is the pointer target for one result row. It carries no state:
// the row it belongs to is identified by its index in rowTags, and the click
// loop reads the selection straight from that index. Not zero-size, for the
// same pointer-identity reason as sidebarTag.
type searchRowTag struct{ _ byte }

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

	if !ready {
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
	if !v.countAt.IsZero() && time.Since(v.countAt) < searchCountTTL {
		n := v.countCache
		v.mu.Unlock()
		return n
	}
	v.mu.Unlock()

	n := v.idx.Count()

	v.mu.Lock()
	v.countCache = n
	v.countAt = time.Now()
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
			cand = append(cand, newSearchCandidate(dir, rel, true))
		}
		rel := relTo(libraryRoot, e.Path)
		cand = append(cand, newSearchCandidate(e.Path, rel, false))
	}
	alpha := &alphaRanking{cands: cand, top: alphaTopResults(cand, searchResultCap)}
	v.mu.Lock()
	v.candidates = cand
	v.alphaTop = alpha
	v.candidatesReady = true
	v.cacheGen = gen
	v.cacheRows = len(all)
	v.builtAt = time.Now()
	// Invalidate the row-count probe rather than seeding it: the first
	// staleness check after this build then re-counts, so an incremental scan
	// that adds rows without bumping the generation is picked up promptly
	// (within one COUNT(*), not one full searchCountTTL of blindness).
	v.countAt = time.Time{}
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
	// Read the candidate slice before the early return (G4-05): a rebuild
	// that lands after the first frame of a cold open (or mid-session) leaves
	// the query unchanged but replaces the candidates, and the results must be
	// re-derived from the new slice. Skip only when both the query and the
	// slice the current results were scored on are unchanged.
	v.mu.Lock()
	candidates := v.candidates
	alpha := v.alphaTop
	v.mu.Unlock()
	if q == v.lastQuery && sameCandidateSlice(candidates, v.activeCandidates) {
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

	// candidates was snapshotted under the mutex above so the background
	// rebuild goroutine can safely replace v.candidates concurrently.
	// activeCandidates is the UI-goroutine-only copy used by layoutResults
	// and Activate for the rest of this frame.
	v.activeCandidates = candidates

	if q == "" {
		// The alphabetical top searchResultCap of *all* candidates, not the
		// first searchResultCap in index order sorted afterwards (G4-05).
		// rebuildCandidates precomputes it off the UI goroutine; a slice
		// published without one (tests) is ranked here, once.
		if alpha == nil || !sameCandidateSlice(alpha.cands, candidates) {
			alpha = &alphaRanking{cands: candidates, top: alphaTopResults(candidates, searchResultCap)}
			v.mu.Lock()
			if sameCandidateSlice(v.candidates, candidates) {
				v.alphaTop = alpha
			}
			v.mu.Unlock()
		}
		v.results = alpha.top
		v.resultsTruncated = len(candidates) > searchResultCap
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

	// Bounded top-K (G5-01): keep only the searchResultCap best matches in a
	// heap instead of collecting, sorting and truncating every match — on a
	// broad query over a large library the old full sort dominated the frame
	// goroutine.
	out, matched := topSearchResults(ql, candidates, searchResultCap)
	v.results = out
	v.resultsTruncated = matched > searchResultCap
}

// scoreCandidate scores candidate c against the lowercased query ql. A basename
// hit wins outright; a path-only hit is penalized (+50) so basename matches
// float to the top. It is the single source of truth for match scores, shared
// by appendFuzzyResult and topSearchResults so both assign identical values.
func scoreCandidate(ql string, c searchCandidate) (int, bool) {
	if ok, sc := fuzzyScore(ql, c.baseLower); ok {
		return sc, true
	}
	if ok, sc := fuzzyScore(ql, c.relLower); ok {
		return sc + 50, true
	}
	return 0, false
}

// appendFuzzyResult scores candidate c (at index i) against the lowercased
// query ql and appends a searchResult to out when it matches. Since G5-01 only
// the incremental narrowing path uses it: it re-scores the previous pass's
// results, keeping their idx values, where the full scan scores in place and
// keeps a bounded heap (see topSearchResults).
func appendFuzzyResult(out []searchResult, ql string, i int, c searchCandidate) []searchResult {
	if sc, ok := scoreCandidate(ql, c); ok {
		return append(out, searchResult{idx: i, score: sc})
	}
	return out
}

// betterResult reports whether a ranks ahead of b under sortSearchResults'
// ordering: lower score wins, ties broken by relative path. Both idx values
// must index the same candidates slice.
func betterResult(a, b searchResult, candidates []searchCandidate) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return candidates[a.idx].rel < candidates[b.idx].rel
}

// worseResult reports whether a ranks behind b — the "greater" relation of the
// max-heap in topSearchResults, whose root is the worst kept result.
func worseResult(a, b searchResult, candidates []searchCandidate) bool {
	return betterResult(b, a, candidates)
}

// siftUpWorst restores the max-heap invariant after appending at i.
func siftUpWorst(h []searchResult, i int, candidates []searchCandidate) {
	for i > 0 {
		parent := (i - 1) / 2
		if !worseResult(h[i], h[parent], candidates) {
			break
		}
		h[i], h[parent] = h[parent], h[i]
		i = parent
	}
}

// siftDownWorst restores the max-heap invariant after replacing the root at i.
func siftDownWorst(h []searchResult, i int, candidates []searchCandidate) {
	n := len(h)
	for {
		worst := i
		if l := 2*i + 1; l < n && worseResult(h[l], h[worst], candidates) {
			worst = l
		}
		if r := 2*i + 2; r < n && worseResult(h[r], h[worst], candidates) {
			worst = r
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}

// topSearchResults returns the k best matches for the lowercased query ql, in
// sortSearchResults order, plus the total number of matches seen (which may
// exceed k). k must be positive — every caller passes searchResultCap. It
// scores every candidate once and keeps a bounded max-heap whose root is the
// worst kept result, so a broad query costs O(n log k) instead of O(n log n)
// (G5-01).
func topSearchResults(ql string, candidates []searchCandidate, k int) (top []searchResult, matched int) {
	heap := make([]searchResult, 0, min(k, len(candidates)))
	for i, c := range candidates {
		sc, ok := scoreCandidate(ql, c)
		if !ok {
			continue
		}
		matched++
		r := searchResult{idx: i, score: sc}
		if len(heap) < k {
			heap = append(heap, r)
			siftUpWorst(heap, len(heap)-1, candidates)
			continue
		}
		// Full: the new match only earns a slot if it beats the current worst.
		if betterResult(r, heap[0], candidates) {
			heap[0] = r
			siftDownWorst(heap, 0, candidates)
		}
	}
	sortSearchResults(heap, candidates)
	return heap, matched
}

// sortSearchResults orders results best→worst: lower score first, ties broken
// by relative path (which is unique per candidate, making the order total and
// deterministic). Shared so the incremental and full scans produce identical
// ordering.
func sortSearchResults(out []searchResult, candidates []searchCandidate) {
	slices.SortFunc(out, func(a, b searchResult) int {
		switch {
		case betterResult(a, b, candidates):
			return -1
		case betterResult(b, a, candidates):
			return 1
		default:
			return 0
		}
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
	v.selected = min(max(v.selected, 0), n-1)
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
				v.selected = i
				v.Activate()
			}
		}
	}

	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	rect := image.Rectangle{Max: image.Pt(totalW, totalH)}
	paint.FillShape(gtx.Ops, th.Background, clip.Rect(rect).Op())

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
	case v.resultsTruncated:
		return "500+ matches — refine your query"
	default:
		return ""
	}
}

func (v *FuzzySearchView) layoutSearchBox(gtx layout.Context, th *Theme) layout.Dimensions {
	w2 := max(gtx.Constraints.Max.X, 200)
	h := gtx.Dp(unit.Dp(40))
	rect := image.Rectangle{Max: image.Pt(w2, h)}
	paint.FillShape(gtx.Ops, th.CellBG, clip.Rect(rect).Op())
	gtx.Constraints = layout.Exact(image.Pt(w2, h))
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
	v.selected = min(v.selected, len(v.results)-1)
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
				lbl := material.Label(th.Theme, unit.Sp(13), c.title)
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
