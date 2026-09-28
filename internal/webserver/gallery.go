package webserver

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// pageSize is the number of cells rendered per request. Galleries with
// 100k+ entries can't be dumped in a single page — both the response body
// and the resulting DOM choke the browser. Picked empirically: 200 cells
// is ~50 KB of HTML, scrolls smoothly, and one round-trip per scroll-page
// stays well under the IntersectionObserver-triggered prefetch budget.
const pageSize = 200

// largeFolderThreshold is the directory size above which the gallery
// surfaces subdirectory drill-down chips above the grid. Matches the
// "100k photos in one folder" target from the optimization brief but
// applies whenever a folder is large enough that drilling beats scrolling.
const largeFolderThreshold = 1000

// viewInfo couples a cache.View with the user-facing metadata the
// gallery and viewer pages need: a page title, the canonical URL for
// the "back to gallery" link, and the query-string fragment that
// forwards the view (plus filter/raw) through /view/<id> for prev/next.
//
// kind is held separately because "trash" doesn't map onto cache.View —
// trashed items aren't in the index. Renderers branch on kind for those
// extra paths.
type viewInfo struct {
	v          cache.View
	kind       string // "all" | "favorites" | "year" | "dir" | "trash"
	title      string
	backHref   string
	backLabel  string
	ctxQuery   string // forwarded to viewer + api endpoints
	contextURL string // canonical gallery URL without ?p=
}

// parseFilter normalizes the filter query param, defaulting to "All".
func parseFilter(s string) string {
	switch s {
	case "Photos", "Videos":
		return s
	default:
		return "All"
	}
}

// parseShowRAW pulls the raw=0|1 toggle. Defaults to true so the gallery
// matches the native year view's "RAW visible" default — and keeps the
// existing webserver tests (which don't pass `raw=`) returning every entry.
func parseShowRAW(q url.Values) bool {
	switch q.Get("raw") {
	case "0", "false", "no":
		return false
	default:
		return true
	}
}

// extraQuery encodes the filter/raw toggles into the URL fragment we tack
// onto sidebar / cell / viewer links so navigation preserves the user's
// toolbar state. Returns "" when both toggles are at their defaults.
func extraQuery(filter string, showRAW bool) string {
	// Treat an empty filter as the "All" default so a zero-value View (one
	// that skipped parseFilter) doesn't emit a spurious `filter=` that would
	// override the receiving handler's own default. The raw default is
	// "visible" (parseShowRAW), so only an explicit hide emits `raw=0`.
	if filter == "" {
		filter = "All"
	}
	parts := []string{}
	if filter != "All" {
		parts = append(parts, "filter="+url.QueryEscape(filter))
	}
	if !showRAW {
		parts = append(parts, "raw=0")
	}
	return strings.Join(parts, "&")
}

// appendQuery joins extra params onto a URL, picking ? or & based on whether
// the URL already has a query.
func appendQuery(href, extra string) string {
	if extra == "" {
		return href
	}
	if strings.Contains(href, "?") {
		return href + "&" + extra
	}
	return href + "?" + extra
}

// viewFromQuery parses a request's `from=...` (plus `y=` / `path=` / `filter=` /
// `raw=`) query parameters and returns the corresponding view metadata.
// Used by the viewer + /api/page to recover the surrounding list context.
func (s *Server) viewFromQuery(q url.Values) (viewInfo, bool) {
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	switch q.Get("from") {
	case "all":
		return viewInfo{
			v:          cache.View{Kind: "all", Filter: filter, ShowRAW: showRAW},
			kind:       "all",
			title:      "All media",
			backHref:   withExtra("/"),
			backLabel:  "All media",
			ctxQuery:   buildCtxQuery("all", "", "", filter, showRAW),
			contextURL: withExtra("/"),
		}, true
	case "favorites":
		return viewInfo{
			v:          cache.View{Kind: "favorites", Filter: filter, ShowRAW: showRAW},
			kind:       "favorites",
			title:      "Favorites",
			backHref:   withExtra("/favorites"),
			backLabel:  "Favorites",
			ctxQuery:   buildCtxQuery("favorites", "", "", filter, showRAW),
			contextURL: withExtra("/favorites"),
		}, true
	case "trash":
		// Trash isn't in the index, so vi.v drives no listing here — but the
		// sidebar rendered on the trash page reads vi.v.Filter/ShowRAW to build
		// its links, so carry the toolbar state through exactly like the other
		// views instead of leaving a zero-value (Filter:"", ShowRAW:false) that
		// would flip the user's RAW/filter state (W-04).
		return viewInfo{
			v:          cache.View{Filter: filter, ShowRAW: showRAW},
			kind:       "trash",
			title:      "Trash",
			backHref:   "/trash",
			backLabel:  "Trash",
			ctxQuery:   buildCtxQuery("trash", "", "", filter, showRAW),
			contextURL: "/trash",
		}, true
	case "year":
		y, err := strconv.Atoi(q.Get("y"))
		if err != nil {
			return viewInfo{}, false
		}
		ys := q.Get("y")
		return viewInfo{
			v:          cache.View{Kind: "year", Year: y, Filter: filter, ShowRAW: showRAW},
			kind:       "year",
			title:      "Year " + ys,
			backHref:   withExtra("/year/" + url.PathEscape(ys)),
			backLabel:  "Year " + ys,
			ctxQuery:   buildCtxQuery("year", ys, "", filter, showRAW),
			contextURL: withExtra("/year/" + url.PathEscape(ys)),
		}, true
	case "dir":
		abs, ok := s.resolveDirParam(q.Get("path"))
		if !ok {
			return viewInfo{}, false
		}
		return viewInfo{
			v:          cache.View{Kind: "dir", Dir: abs, Filter: filter, ShowRAW: showRAW},
			kind:       "dir",
			title:      filepath.Base(abs),
			backHref:   withExtra(s.dirHref(abs)),
			backLabel:  filepath.Base(abs),
			ctxQuery:   buildCtxQuery("dir", "", s.relPath(abs), filter, showRAW),
			contextURL: withExtra(s.dirHref(abs)),
		}, true
	}
	return viewInfo{}, false
}

// buildCtxQuery assembles the `from=...&...` fragment used to thread context
// through viewer + api links. dir is library-relative (see relPath).
func buildCtxQuery(from, year, dir, filter string, showRAW bool) string {
	parts := []string{"from=" + url.QueryEscape(from)}
	if year != "" {
		parts = append(parts, "y="+url.QueryEscape(year))
	}
	if dir != "" {
		parts = append(parts, "path="+url.QueryEscape(dir))
	}
	if extra := extraQuery(filter, showRAW); extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, "&")
}

// handleIndex serves the default gallery — every indexed media file.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	vi := viewInfo{
		v:          cache.View{Kind: "all", Filter: filter, ShowRAW: showRAW},
		kind:       "all",
		title:      "All media",
		backHref:   appendQuery("/", extra),
		backLabel:  "All media",
		ctxQuery:   buildCtxQuery("all", "", "", filter, showRAW),
		contextURL: appendQuery("/", extra),
	}
	s.renderGalleryPage(w, r, vi)
}

// handleFavorites serves only the entries flagged as favorites.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	vi := viewInfo{
		v:          cache.View{Kind: "favorites", Filter: filter, ShowRAW: showRAW},
		kind:       "favorites",
		title:      "Favorites",
		backHref:   appendQuery("/favorites", extra),
		backLabel:  "Favorites",
		ctxQuery:   buildCtxQuery("favorites", "", "", filter, showRAW),
		contextURL: appendQuery("/favorites", extra),
	}
	s.renderGalleryPage(w, r, vi)
}

// handleTrash serves a listing of soft-deleted files. Trashed items are
// not in the index — they're read from the trash directory directly.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	// vi.v doesn't drive the trash listing (trashEntries does), but renderSidebar
	// reads vi.v.Filter/ShowRAW to build its links. Parse them from the query
	// like every other handler so the sidebar carries the user's toolbar state
	// instead of a zero-value view that hides RAW and forces filter= (W-04).
	vi := viewInfo{
		v:          cache.View{Filter: filter, ShowRAW: showRAW},
		kind:       "trash",
		title:      "Trash",
		backHref:   "/trash",
		backLabel:  "Trash",
		ctxQuery:   buildCtxQuery("trash", "", "", filter, showRAW),
		contextURL: "/trash",
	}
	s.renderGalleryPage(w, r, vi)
}

// handleYear serves the entries whose capture year matches /year/<YYYY>.
func (s *Server) handleYear(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/year/")
	rest = strings.Trim(rest, "/")
	year, err := strconv.Atoi(rest)
	if err != nil || year < 1900 || year > 9999 {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	vi := viewInfo{
		v:          cache.View{Kind: "year", Year: year, Filter: filter, ShowRAW: showRAW},
		kind:       "year",
		title:      "Year " + rest,
		backHref:   appendQuery("/year/"+rest, extra),
		backLabel:  "Year " + rest,
		ctxQuery:   buildCtxQuery("year", rest, "", filter, showRAW),
		contextURL: appendQuery("/year/"+rest, extra),
	}
	s.renderGalleryPage(w, r, vi)
}

// handleDir serves the entries under /dir?path=<library-relative path>. The
// requested path must resolve to a directory inside the library root —
// anything outside is rejected so a URL can't escape the configured tree.
func (s *Server) handleDir(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	abs, ok := s.resolveDirParam(raw)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	vi := viewInfo{
		v:          cache.View{Kind: "dir", Dir: abs, Filter: filter, ShowRAW: showRAW},
		kind:       "dir",
		title:      filepath.Base(abs),
		backHref:   appendQuery(s.dirHref(abs), extra),
		backLabel:  filepath.Base(abs),
		ctxQuery:   buildCtxQuery("dir", "", s.relPath(abs), filter, showRAW),
		contextURL: appendQuery(s.dirHref(abs), extra),
	}
	s.renderGalleryPage(w, r, vi)
}

// withinRoot reports whether abs is the library root itself or a path
// nested under it. Used to validate /dir requests.
func (s *Server) withinRoot(abs string) bool {
	if s.libraryRoot == "" {
		return false
	}
	if abs == s.libraryRoot {
		return true
	}
	return strings.HasPrefix(abs, s.libraryRoot+string(filepath.Separator))
}

// relPath returns abs relative to the library root, which is what the server
// puts in URLs and API responses so LAN clients never see the absolute
// location (home directory, username) of the library (W-19). A path outside
// the root — which callers never pass — falls back to its base name.
func (s *Server) relPath(abs string) string {
	if !s.withinRoot(abs) {
		return filepath.Base(abs)
	}
	rel, err := filepath.Rel(s.libraryRoot, abs)
	if err != nil {
		return filepath.Base(abs)
	}
	return rel
}

// dirHref is the /dir link for the directory abs.
func (s *Server) dirHref(abs string) string {
	return "/dir?path=" + url.QueryEscape(s.relPath(abs))
}

// resolveDirParam turns a /dir or from=dir `path` parameter into an absolute
// directory inside the library root. The server emits library-relative paths;
// absolute ones are still accepted so links bookmarked before W-19 keep
// working. Empty is rejected explicitly: joined onto the root it would
// silently serve the whole library as a "dir" view.
func (s *Server) resolveDirParam(raw string) (string, bool) {
	if raw == "" || s.libraryRoot == "" {
		return "", false
	}
	abs := filepath.Clean(raw)
	if !filepath.IsAbs(raw) {
		abs = filepath.Join(s.libraryRoot, raw)
	}
	if !s.withinRoot(abs) {
		return "", false
	}
	return abs, true
}

// renderGalleryPage writes the first page of the gallery (sidebar, header,
// initial cells) and includes the infinite-scroll bootstrap. Additional
// pages are fetched as JSON from /api/page.
func (s *Server) renderGalleryPage(w http.ResponseWriter, r *http.Request, vi viewInfo) {
	page := pageNumber(r.URL.Query().Get("p"))

	var entries []cache.Entry
	var total int
	var hasNext bool

	if vi.kind == "trash" {
		all := s.trashEntries()
		total = len(all)
		offset := (page - 1) * pageSize
		if offset > total {
			offset = 0
			page = 1
		}
		end := min(offset+pageSize, total)
		entries = all[offset:end]
		hasNext = end < total
	} else {
		total = s.cachedCountView(vi.v)
		offset := (page - 1) * pageSize
		if offset > total {
			offset = 0
			page = 1
		}
		entries = s.index.ListPage(vi.v, offset, pageSize)
		hasNext = offset+len(entries) < total
	}

	// For a directory view both the sidebar's "In <dir>" section and the
	// large-folder chip strip need the same child directories and their
	// filtered counts. Compute them once here and hand them to both renderers
	// so the os.ReadDir and the O(subtree) grouped-count scan
	// (CountChildDirsFiltered) each run a single time per page instead of once
	// per renderer (W-08). Non-dir views leave both nil and neither renderer
	// touches them.
	var dirChildren []string
	var dirChildCounts map[string]int
	if vi.v.Kind == "dir" {
		dirChildren = listSubdirs(vi.v.Dir)
		if len(dirChildren) > 0 {
			dirChildCounts = s.index.CountChildDirsFiltered(vi.v.Dir, vi.v.Filter, vi.v.ShowRAW)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)
	fmt.Fprint(w, `<input type="checkbox" id="navtoggle" class="navtoggle" hidden>`)
	fmt.Fprint(w, `<div class="layout">`)
	s.renderSidebar(w, vi, dirChildren, dirChildCounts)
	fmt.Fprint(w, `<main class="content">`)
	fmt.Fprint(w, mobileTopBar)
	s.renderToolbar(w, vi)
	fmt.Fprintf(w, `<h1>%s <span class="count">%d items</span></h1>`,
		html.EscapeString(vi.title), total)

	if vi.v.Kind == "dir" && total > largeFolderThreshold {
		s.renderSubdirChips(w, vi, dirChildren, dirChildCounts)
	}

	fmt.Fprint(w, `<div class="grid" id="grid"`)
	fmt.Fprintf(w, ` data-from="%s" data-next-page="%d" data-has-next="%t"`,
		html.EscapeString(vi.ctxQuery), page+1, hasNext)
	fmt.Fprint(w, `>`)
	writeCells(w, entries, vi.ctxQuery)
	fmt.Fprint(w, `</div>`)
	if hasNext {
		fmt.Fprint(w, `<div class="loader" id="loader">Loading more…</div>`)
		// Fallback link for users with JS disabled or assistive tech.
		nextURL := vi.contextURL
		sep := "?"
		if strings.Contains(nextURL, "?") {
			sep = "&"
		}
		fmt.Fprintf(w, `<noscript><a class="pager" href="%s%sp=%d">Next page →</a></noscript>`,
			html.EscapeString(nextURL), sep, page+1)
	}
	fmt.Fprint(w, `</main></div>`)
	fmt.Fprint(w, gridScript)
	fmt.Fprint(w, `</body></html>`)
}

// writeCells emits one <a class="cell"> per entry. Used by the initial
// page render; subsequent pages are appended client-side from /api/page
// JSON so any change to the cell shape needs to mirror to the JS builder
// in gridScript.
func writeCells(w http.ResponseWriter, entries []cache.Entry, ctxQ string) {
	for _, e := range entries {
		name := html.EscapeString(filepath.Base(e.Path))
		thumbURL := "/thumb/" + e.ThumbID
		viewURL := "/view/" + e.ThumbID
		if ctxQ != "" {
			viewURL += "?" + ctxQ
		}
		badge := ""
		if e.Type == scan.TypeVideo {
			badge = `<span class="badge">video</span>`
		}
		star := ""
		if e.Favorite {
			star = `<span class="star" title="Favorite">★</span>`
		}
		fmt.Fprintf(w,
			`<a class="cell" href="%s" title="%s" data-id="%s"><img loading="lazy" decoding="async" src="%s" alt="">%s%s<span class="name">%s</span></a>`,
			viewURL, name, e.ThumbID, thumbURL, badge, star, name,
		)
	}
}

// pageNumber parses a 1-based "?p=" parameter, defaulting to 1 for empty
// or invalid input. Negative or zero pages collapse to 1.
func pageNumber(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// renderToolbar emits the segmented filter buttons + RAW toggle above the
// gallery header. Each control is a plain link that re-renders the current
// view with the toggle flipped, so JS isn't required for the toolbar to
// work — keeps the page resilient on locked-down browsers.
func (s *Server) renderToolbar(w http.ResponseWriter, vi viewInfo) {
	// Trash has no filter semantics; skip the bar to avoid suggesting a
	// non-functional toggle.
	if vi.kind == "trash" {
		return
	}
	currentFilter := vi.v.Filter
	if currentFilter == "" {
		currentFilter = "All"
	}
	rawOn := vi.v.ShowRAW

	makeURL := func(filter string, showRAW bool) string {
		return appendQuery(s.basePathFor(vi), extraQuery(filter, showRAW))
	}

	fmt.Fprint(w, `<div class="toolbar">`)
	fmt.Fprint(w, `<div class="seg-group" role="group" aria-label="Filter">`)
	for _, f := range []string{"All", "Photos", "Videos"} {
		cls := "seg"
		if f == currentFilter {
			cls += " active"
		}
		fmt.Fprintf(w, `<a class="%s" href="%s">%s</a>`,
			cls, html.EscapeString(makeURL(f, rawOn)), html.EscapeString(f))
	}
	fmt.Fprint(w, `</div>`)

	rawCls := "seg toggle"
	rawLabel := "RAW hidden"
	if rawOn {
		rawCls += " active"
		rawLabel = "RAW visible"
	}
	fmt.Fprintf(w, `<a class="%s" href="%s" title="Toggle RAW visibility">%s</a>`,
		rawCls, html.EscapeString(makeURL(currentFilter, !rawOn)), html.EscapeString(rawLabel))
	fmt.Fprint(w, `</div>`)
}

// basePathFor returns the canonical URL path (no query) for the gallery
// view described by vi. Used by the toolbar to build flip-state links.
func (s *Server) basePathFor(vi viewInfo) string {
	switch vi.kind {
	case "favorites":
		return "/favorites"
	case "trash":
		return "/trash"
	case "year":
		return "/year/" + strconv.Itoa(vi.v.Year)
	case "dir":
		return s.dirHref(vi.v.Dir)
	default:
		return "/"
	}
}

// listSubdirsHook, when non-nil, is invoked with every listSubdirs argument.
// Test-only seam (see server_test.go) used to assert that a large-directory
// gallery page scans each directory's children exactly once (W-08).
var listSubdirsHook func(dir string)

// listSubdirs returns the immediate child directories of dir, sorted by
// name, with dotfile directories filtered out (matching the native
// sidebar's behavior). Errors are silently swallowed — a missing or
// unreadable directory just yields an empty sidebar section.
func listSubdirs(dir string) []string {
	if listSubdirsHook != nil {
		listSubdirsHook(dir)
	}
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	// os.ReadDir already returns entries sorted by filename; joining each with
	// the common `dir` prefix preserves that order, so no explicit sort needed.
	return out
}

// trashEntriesTTL is how long the cached trash listing is reused before
// we re-scan the directory. Trash is tiny and changes rarely; a few
// seconds is plenty to coalesce a burst of /thumb, /media, and /api/info
// requests for the same item.
const trashEntriesTTL = 5 * time.Second

// trashEntries returns the current trash listing, caching the result for
// trashEntriesTTL. Also rebuilds the id→entry map used by lookupEntry.
func (s *Server) trashEntries() []cache.Entry {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	if s.trashDir == "" {
		return nil
	}
	if !s.trashAt.IsZero() && time.Since(s.trashAt) < trashEntriesTTL {
		return s.trashCache
	}
	entries := cache.ListTrash(s.trashDir)
	s.trashCache = entries
	s.trashIndex = make(map[string]cache.Entry, len(entries))
	for _, e := range entries {
		s.trashIndex[e.ThumbID] = e
	}
	s.trashAt = time.Now()
	return entries
}

// countTrash returns the number of items currently in the trash, using the
// cached listing when fresh.
func (s *Server) countTrash() int {
	return len(s.trashEntries())
}
