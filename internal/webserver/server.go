// Package webserver exposes the photo-viewer index over HTTP so a
// remote browser can list and view every indexed media file.
//
// Media is addressed by ThumbID (sha1 of the absolute path) rather than
// by filesystem path — that keeps the wire format opaque and prevents
// directory-traversal probes from reaching files outside the index.
package webserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// Server wraps a net/http.Server scoped to one index + thumb store.
// All methods are safe for concurrent use.
type Server struct {
	index       *cache.Index
	store       *cache.ThumbStore
	libraryRoot string

	mu       sync.Mutex
	srv      *http.Server
	listener net.Listener
	addr     string // resolved listen address (host:port) once running
	password string
	running  bool
}

// New constructs an idle server. libraryRoot is used to enumerate the
// top-level directory categories and to confine /dir requests to paths
// rooted at the library. Call Start to bind a port.
func New(index *cache.Index, store *cache.ThumbStore, libraryRoot string) *Server {
	return &Server{index: index, store: store, libraryRoot: libraryRoot}
}

// Running reports whether the server has an active listener.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Addr returns the bound address (host:port) while running, or "" when stopped.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start binds host:port and serves the index. When password is non-empty the
// routes are wrapped with HTTP Basic auth (username "viewer"). Returns an
// error if the listener can't be opened or the server is already running.
func (s *Server) Start(host string, port int, password string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("webserver already running")
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/favorites", s.handleFavorites)
	mux.HandleFunc("/year/", s.handleYear)
	mux.HandleFunc("/dir", s.handleDir)
	mux.HandleFunc("/view/", s.handleView)
	mux.HandleFunc("/thumb/", s.handleThumb)
	mux.HandleFunc("/media/", s.handleMedia)
	mux.HandleFunc("/api/page", s.handleAPIPage)

	var handler http.Handler = mux
	if password != "" {
		handler = basicAuth(handler, password)
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming large videos can exceed any fixed budget
		IdleTimeout:  120 * time.Second,
	}
	s.srv = srv
	s.listener = ln
	s.addr = ln.Addr().String()
	s.password = password
	s.running = true
	s.mu.Unlock()

	go func() {
		_ = srv.Serve(ln)
		s.mu.Lock()
		s.running = false
		s.addr = ""
		s.mu.Unlock()
	}()
	return nil
}

// Stop gracefully shuts down the running server. No-op when stopped.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.listener = nil
	s.running = false
	s.addr = ""
	s.password = ""
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// basicAuth wraps next so that every request must present the configured
// password via HTTP Basic auth. The username is fixed ("viewer") — browsers
// require one for the dialog but it carries no security on its own.
func basicAuth(next http.Handler, password string) http.Handler {
	pw := []byte(password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, given, ok := r.BasicAuth()
		// subtle.ConstantTimeCompare requires equal-length inputs.
		if !ok || len(given) != len(pw) || subtle.ConstantTimeCompare([]byte(given), pw) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="photo-viewer"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// viewInfo couples a cache.View with the user-facing metadata the
// gallery and viewer pages need: a page title, the canonical URL for
// the "back to gallery" link, and the query-string fragment that
// forwards the view through /view/<id> for prev/next.
type viewInfo struct {
	v          cache.View
	title      string
	backHref   string
	backLabel  string
	ctxQuery   string
	contextURL string // canonical gallery URL without ?p=
}

// viewFromQuery parses a request's `from=...` (plus `y=` / `path=`)
// query parameters and returns the corresponding view metadata.
// Used by the viewer to recover the surrounding list context.
func (s *Server) viewFromQuery(q url.Values) (viewInfo, bool) {
	switch q.Get("from") {
	case "all":
		return viewInfo{
			v:          cache.View{Kind: "all"},
			title:      "All media",
			backHref:   "/",
			backLabel:  "All media",
			ctxQuery:   "from=all",
			contextURL: "/",
		}, true
	case "favorites":
		return viewInfo{
			v:          cache.View{Kind: "favorites"},
			title:      "Favorites",
			backHref:   "/favorites",
			backLabel:  "Favorites",
			ctxQuery:   "from=favorites",
			contextURL: "/favorites",
		}, true
	case "year":
		y, err := strconv.Atoi(q.Get("y"))
		if err != nil {
			return viewInfo{}, false
		}
		ys := q.Get("y")
		return viewInfo{
			v:          cache.View{Kind: "year", Year: y, Filter: "All", ShowRAW: true},
			title:      "Year " + ys,
			backHref:   "/year/" + url.PathEscape(ys),
			backLabel:  "Year " + ys,
			ctxQuery:   "from=year&y=" + url.QueryEscape(ys),
			contextURL: "/year/" + url.PathEscape(ys),
		}, true
	case "dir":
		path := q.Get("path")
		abs, err := filepath.Abs(path)
		if err != nil || !s.withinRoot(abs) {
			return viewInfo{}, false
		}
		return viewInfo{
			v:          cache.View{Kind: "dir", Dir: abs},
			title:      filepath.Base(abs),
			backHref:   "/dir?path=" + url.QueryEscape(abs),
			backLabel:  filepath.Base(abs),
			ctxQuery:   "from=dir&path=" + url.QueryEscape(abs),
			contextURL: "/dir?path=" + url.QueryEscape(abs),
		}, true
	}
	return viewInfo{}, false
}

// handleIndex serves the default gallery — every indexed media file.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	vi := viewInfo{
		v:          cache.View{Kind: "all"},
		title:      "All media",
		backHref:   "/",
		backLabel:  "All media",
		ctxQuery:   "from=all",
		contextURL: "/",
	}
	s.renderGalleryPage(w, r, vi)
}

// handleFavorites serves only the entries flagged as favorites.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	vi := viewInfo{
		v:          cache.View{Kind: "favorites"},
		title:      "Favorites",
		backHref:   "/favorites",
		backLabel:  "Favorites",
		ctxQuery:   "from=favorites",
		contextURL: "/favorites",
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
	vi := viewInfo{
		v:          cache.View{Kind: "year", Year: year, Filter: "All", ShowRAW: true},
		title:      "Year " + rest,
		backHref:   "/year/" + rest,
		backLabel:  "Year " + rest,
		ctxQuery:   "from=year&y=" + url.QueryEscape(rest),
		contextURL: "/year/" + rest,
	}
	s.renderGalleryPage(w, r, vi)
}

// handleDir serves the entries under /dir?path=<abs-path>. The requested
// path must resolve to a directory inside the library root — anything
// outside is rejected so a URL can't escape the configured tree.
func (s *Server) handleDir(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.withinRoot(abs) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	vi := viewInfo{
		v:          cache.View{Kind: "dir", Dir: abs},
		title:      filepath.Base(abs),
		backHref:   "/dir?path=" + url.QueryEscape(abs),
		backLabel:  filepath.Base(abs),
		ctxQuery:   "from=dir&path=" + url.QueryEscape(abs),
		contextURL: "/dir?path=" + url.QueryEscape(abs),
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

// renderGalleryPage writes the first page of the gallery (sidebar, header,
// initial cells) and includes the infinite-scroll bootstrap. Additional
// pages are fetched as JSON from /api/page.
func (s *Server) renderGalleryPage(w http.ResponseWriter, r *http.Request, vi viewInfo) {
	page := pageNumber(r.URL.Query().Get("p"))
	total := s.index.CountView(vi.v)
	offset := (page - 1) * pageSize
	if offset > total {
		offset = 0
		page = 1
	}
	entries := s.index.ListPage(vi.v, offset, pageSize)
	hasNext := offset+len(entries) < total

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)
	fmt.Fprint(w, `<input type="checkbox" id="navtoggle" class="navtoggle" hidden>`)
	fmt.Fprint(w, `<div class="layout">`)
	s.renderSidebar(w, vi)
	fmt.Fprint(w, `<main class="content">`)
	fmt.Fprint(w, mobileTopBar)
	fmt.Fprintf(w, `<h1>%s <span class="count">%d items</span></h1>`,
		html.EscapeString(vi.title), total)

	if vi.v.Kind == "dir" && total > largeFolderThreshold {
		s.renderSubdirChips(w, vi.v.Dir)
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
	fmt.Fprint(w, infiniteScrollScript)
	fmt.Fprint(w, `</body></html>`)
}

// cellJSON is the wire shape returned by /api/page. Kept compact since
// 100k-photo libraries will fetch this hundreds of times per session.
type cellJSON struct {
	ID    string `json:"id"`    // ThumbID — drives /thumb/<id> and /view/<id>
	Name  string `json:"name"`  // basename for the caption + img alt
	Video bool   `json:"video"` // adds the "video" badge in the corner
}

// handleAPIPage serves the next page of cells as JSON for the
// infinite-scroll fetcher. Query: ?from=...&y=...&path=...&p=N
func (s *Server) handleAPIPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vi, ok := s.viewFromQuery(q)
	if !ok {
		http.Error(w, "invalid view", http.StatusBadRequest)
		return
	}
	page := pageNumber(q.Get("p"))
	offset := (page - 1) * pageSize
	entries := s.index.ListPage(vi.v, offset, pageSize)
	total := s.index.CountView(vi.v)
	items := make([]cellJSON, 0, len(entries))
	for _, e := range entries {
		items = append(items, cellJSON{
			ID:    e.ThumbID,
			Name:  filepath.Base(e.Path),
			Video: e.Type == scan.TypeVideo,
		})
	}
	resp := struct {
		Items   []cellJSON `json:"items"`
		HasNext bool       `json:"hasNext"`
	}{
		Items:   items,
		HasNext: offset+len(entries) < total,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeCells emits one <a class="cell"> per entry. Used by the initial
// page render; subsequent pages are appended client-side from /api/page
// JSON so any change to the cell shape needs to mirror to the JS builder
// in infiniteScrollScript.
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
		fmt.Fprintf(w,
			`<a class="cell" href="%s" title="%s"><img loading="lazy" decoding="async" src="%s" alt="">%s<span class="name">%s</span></a>`,
			viewURL, name, thumbURL, badge, name,
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

// renderSidebar emits the categories panel: Favorites, library root,
// top-level subdirectories, and a year list. Active row matches the
// currently-rendered view.
func (s *Server) renderSidebar(w http.ResponseWriter, vi viewInfo) {
	v := vi.v
	fmt.Fprint(w, `<nav class="sidebar"><div class="sec">Library</div>`)

	favCount := s.index.CountFavorites("All", true)
	s.sidebarRow(w, "/", "All media", "", s.index.Count(), v.Kind == "all")
	s.sidebarRow(w, "/favorites", "Favorites", "★", favCount, v.Kind == "favorites")

	// Top-level subdirectories. Sourced from the filesystem (mirroring the
	// native sidebar) so newly-created folders show up without a rebuild.
	subdirs := listSubdirs(s.libraryRoot)
	if len(subdirs) > 0 {
		fmt.Fprint(w, `<div class="sec">Folders</div>`)
		for _, d := range subdirs {
			q := url.Values{"path": []string{d}}.Encode()
			href := "/dir?" + q
			active := v.Kind == "dir" && v.Dir == d
			s.sidebarRow(w, href, filepath.Base(d), "", s.index.CountDir(d), active)
		}
	}

	// When viewing a directory, surface its child folders as a nested
	// section so the user can drill down instead of paginating through
	// a 100k-photo flat list.
	if v.Kind == "dir" {
		children := listSubdirs(v.Dir)
		if len(children) > 0 {
			fmt.Fprintf(w, `<div class="sec">In %s</div>`, html.EscapeString(filepath.Base(v.Dir)))
			counts := s.index.CountChildDirsFiltered(v.Dir, "All", true)
			for _, d := range children {
				q := url.Values{"path": []string{d}}.Encode()
				href := "/dir?" + q
				s.sidebarRow(w, href, filepath.Base(d), "", counts[d], false)
			}
		}
	}

	// Year list. ListByYear gives counts directly via Years().
	years := s.index.Years("All", true)
	if len(years) > 0 {
		fmt.Fprint(w, `<div class="sec">Years</div>`)
		for _, y := range years {
			ys := strconv.Itoa(y.Year)
			active := v.Kind == "year" && v.Year == y.Year
			s.sidebarRow(w, "/year/"+ys, ys, "", y.Count, active)
		}
	}
	fmt.Fprint(w, `</nav>`)
}

// renderSubdirChips renders a compact strip of child-folder chips above
// the grid for large directories. Cheap visual cue that there are
// subfolders worth drilling into without scrolling past the grid.
func (s *Server) renderSubdirChips(w http.ResponseWriter, dir string) {
	children := listSubdirs(dir)
	if len(children) == 0 {
		return
	}
	counts := s.index.CountChildDirsFiltered(dir, "All", true)
	type chip struct {
		path  string
		name  string
		count int
	}
	chips := make([]chip, 0, len(children))
	for _, c := range children {
		chips = append(chips, chip{path: c, name: filepath.Base(c), count: counts[c]})
	}
	// Heaviest subfolders first so the user sees the chunkiest drill-downs
	// without scrolling the chip strip.
	sort.Slice(chips, func(i, j int) bool {
		if chips[i].count != chips[j].count {
			return chips[i].count > chips[j].count
		}
		return chips[i].name < chips[j].name
	})
	fmt.Fprint(w, `<div class="chips">`)
	for _, c := range chips {
		q := url.Values{"path": []string{c.path}}.Encode()
		fmt.Fprintf(w,
			`<a class="chip" href="/dir?%s"><span>%s</span><span class="chip-count">%d</span></a>`,
			q, html.EscapeString(c.name), c.count)
	}
	fmt.Fprint(w, `</div>`)
}

// sidebarRow renders one anchor in the sidebar with an optional leading
// glyph (used for the Favorites star) and a right-aligned count badge.
func (s *Server) sidebarRow(w http.ResponseWriter, href, label, glyph string, count int, active bool) {
	cls := "row"
	if active {
		cls += " active"
	}
	g := ""
	if glyph != "" {
		g = `<span class="glyph">` + html.EscapeString(glyph) + `</span>`
	}
	fmt.Fprintf(w,
		`<a class="%s" href="%s">%s<span class="label">%s</span><span class="badge-count">%d</span></a>`,
		cls, href, g, html.EscapeString(label), count,
	)
}

// listSubdirs returns the immediate child directories of dir, sorted by
// name, with dotfile directories filtered out (matching the native
// sidebar's behavior). Errors are silently swallowed — a missing or
// unreadable directory just yields an empty sidebar section.
func listSubdirs(dir string) []string {
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
	sort.Strings(out)
	return out
}

// handleView renders a single-media page with the image or video centered
// and prev/next links into the surrounding list. The `from` query carries
// the list context — without it, the viewer renders without nav arrows.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/view/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	cur, ok := s.index.GetEntryByThumbID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	vi, hasCtx := s.viewFromQuery(r.URL.Query())
	var prev, next *cache.Entry
	var pos, total int
	var ctxQ, backHref, backLabel string
	if hasCtx {
		prev, next, pos, total = s.index.Neighbors(vi.v, cur.Path)
		ctxQ = vi.ctxQuery
		backHref = vi.backHref
		backLabel = vi.backLabel
	} else {
		backHref = "/"
		backLabel = "Gallery"
	}

	s.renderViewer(w, cur, prev, next, ctxQ, backHref, backLabel, pos, total)
}

// renderViewer writes the single-media page. pos is the 1-based position
// in the surrounding list (0 when no context was provided); total is the
// list length. prev/next are nil at the edges.
func (s *Server) renderViewer(w http.ResponseWriter,
	cur cache.Entry, prev, next *cache.Entry,
	ctxQ, backHref, backLabel string, pos, total int,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)

	prevHref := ""
	if prev != nil {
		prevHref = "/view/" + prev.ThumbID
		if ctxQ != "" {
			prevHref += "?" + ctxQ
		}
	}
	nextHref := ""
	if next != nil {
		nextHref = "/view/" + next.ThumbID
		if ctxQ != "" {
			nextHref += "?" + ctxQ
		}
	}

	fmt.Fprint(w, `<div class="viewer">`)
	fmt.Fprint(w, `<header class="vbar">`)
	fmt.Fprintf(w, `<a class="back" href="%s">‹ %s</a>`, html.EscapeString(backHref), html.EscapeString(backLabel))
	fmt.Fprintf(w, `<span class="vname">%s</span>`, html.EscapeString(filepath.Base(cur.Path)))
	if pos > 0 && total > 0 {
		fmt.Fprintf(w, `<span class="vpos">%d / %d</span>`, pos, total)
	} else {
		fmt.Fprint(w, `<span class="vpos"></span>`)
	}
	fmt.Fprint(w, `</header>`)

	fmt.Fprint(w, `<div class="vstage">`)
	mediaURL := "/media/" + cur.ThumbID
	if cur.Type == scan.TypeVideo {
		fmt.Fprintf(w, `<video class="vmedia" controls preload="metadata" playsinline src="%s"></video>`, mediaURL)
	} else {
		fmt.Fprintf(w, `<img class="vmedia" src="%s" alt="%s">`,
			mediaURL, html.EscapeString(filepath.Base(cur.Path)))
	}
	// Tap zones — large invisible areas on the left/right so anywhere
	// outside the controls advances. The visible arrow buttons are
	// duplicated for clarity.
	if prevHref != "" {
		fmt.Fprintf(w, `<a class="varrow vprev" href="%s" aria-label="Previous">‹</a>`, prevHref)
	}
	if nextHref != "" {
		fmt.Fprintf(w, `<a class="varrow vnext" href="%s" aria-label="Next">›</a>`, nextHref)
	}
	fmt.Fprint(w, `</div>`)
	fmt.Fprint(w, `</div>`)

	// Keyboard nav: left/right arrows mirror the on-screen buttons; Esc
	// goes back to the gallery. Tiny script, intentionally inline so the
	// viewer doesn't need an extra request.
	fmt.Fprintf(w, `<script>
(function(){
  var prev=%q, next=%q, back=%q;
  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea|video)$/i.test(e.target.tagName)) return;
    if (e.key === 'ArrowLeft'  && prev) location.href = prev;
    if (e.key === 'ArrowRight' && next) location.href = next;
    if (e.key === 'Escape') location.href = back;
  });
})();
</script>`, prevHref, nextHref, backHref)
	fmt.Fprint(w, `</body></html>`)
}

// handleThumb serves the cached thumbnail JPEG for /thumb/<id>.
// Generates on demand via ThumbStore.Path so unseen thumbs are created
// the same way the GUI would.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	// The thumb_id is sha1(path), and the thumb is regenerated whenever
	// the source mtime advances past the thumb mtime — so the same id
	// always identifies "the thumb for this path right now". Using it as
	// a strong ETag lets browsers skip even revalidation roundtrips, and
	// the conditional-GET shortcut here avoids touching disk on a hit.
	etag := `"` + id + `"`
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	e, ok := s.index.GetEntryByThumbID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	thumbPath, err := s.store.Path(e)
	if err != nil {
		http.Error(w, "thumbnail unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("ETag", etag)
	http.ServeFile(w, r, thumbPath)
}

// handleMedia serves the original media file for /media/<id>. Uses
// http.ServeFile so range requests are honored (important for video).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/media/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	e, ok := s.index.GetEntryByThumbID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ct := mimeFor(e); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	// Make browser save with the original filename instead of the opaque id.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`inline; filename="%s"`, filepath.Base(e.Path)))
	http.ServeFile(w, r, e.Path)
}

// validID returns true if id is a 40-char lowercase hex string — the shape
// ThumbIDFor produces. Anything else is rejected before touching disk.
func validID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// mimeFor returns a content type for common extensions. The Go stdlib's
// http.ServeFile sniffs content from the first 512 bytes if we leave the
// header unset, but that misclassifies HEIC and CR2 — set explicitly when
// we can so browsers don't try to render RAW as plain text.
func mimeFor(e cache.Entry) string {
	ext := strings.ToLower(filepath.Ext(e.Path))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".heic", ".heif":
		return "image/heic"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	}
	return ""
}

// mobileTopBar is emitted at the top of every gallery's .content so the
// hamburger has a place to live. On desktop the bar is hidden via CSS.
const mobileTopBar = `<div class="topbar"><label for="navtoggle" class="hamburger" aria-label="Menu">☰</label><span class="brand">Photo Viewer</span></div>`

// infiniteScrollScript is a tiny IntersectionObserver-based fetcher that
// appends more cells when the bottom-of-grid loader scrolls into view.
// Self-contained so the gallery doesn't need an external JS file.
//
// Cells are built up via createElement + textContent rather than parsed
// from an HTML string: it avoids any path that resembles innerHTML on
// network data, keeps the JSON wire format compact, and means the cell
// shape lives in one JS function instead of also a string template.
const infiniteScrollScript = `<script>
(function(){
  var grid = document.getElementById('grid');
  var loader = document.getElementById('loader');
  if (!grid || !loader) return;
  var nextPage = parseInt(grid.dataset.nextPage || '2', 10);
  var hasNext  = grid.dataset.hasNext === 'true';
  var from     = grid.dataset.from || '';
  var loading  = false;

  function buildCell(item) {
    var a = document.createElement('a');
    a.className = 'cell';
    a.href = '/view/' + item.id + (from ? '?' + from : '');
    a.title = item.name;

    var img = document.createElement('img');
    img.loading = 'lazy';
    img.decoding = 'async';
    img.alt = '';
    img.src = '/thumb/' + item.id;
    a.appendChild(img);

    if (item.video) {
      var badge = document.createElement('span');
      badge.className = 'badge';
      badge.textContent = 'video';
      a.appendChild(badge);
    }

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = item.name;
    a.appendChild(name);
    return a;
  }

  function fetchNext() {
    if (loading || !hasNext) return;
    loading = true;
    var url = '/api/page?' + from + '&p=' + nextPage;
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function(r){ return r.json(); })
      .then(function(data){
        if (data && data.items) {
          var frag = document.createDocumentFragment();
          for (var i = 0; i < data.items.length; i++) {
            frag.appendChild(buildCell(data.items[i]));
          }
          grid.appendChild(frag);
        }
        hasNext = !!(data && data.hasNext);
        nextPage++;
        loading = false;
        if (!hasNext) {
          loader.remove();
          obs.disconnect();
        }
      })
      .catch(function(){
        loading = false;
        loader.textContent = 'Failed to load — scroll to retry.';
      });
  }

  var obs = new IntersectionObserver(function(entries){
    entries.forEach(function(e){ if (e.isIntersecting) fetchNext(); });
  }, { rootMargin: '600px 0px' });
  obs.observe(loader);
})();
</script>`

const pageHeader = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Photo Viewer</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; background: #121212; color: #eee; font: 14px system-ui, sans-serif; }
  .navtoggle { display: none; }
  .topbar { display: none; }
  .layout { display: grid; grid-template-columns: 240px 1fr; min-height: 100vh; }
  .sidebar { background: #181818; border-right: 1px solid #2a2a2a; padding: 8px 0; overflow-y: auto;
             position: sticky; top: 0; max-height: 100vh; }
  .sidebar .sec { padding: 12px 16px 4px; color: #888; font-size: 11px; text-transform: uppercase;
                  letter-spacing: 0.05em; font-weight: 600; }
  .sidebar .row { display: flex; align-items: center; gap: 6px; padding: 10px 16px;
                  color: #eee; text-decoration: none; font-size: 13px; }
  .sidebar .row:hover { background: #232323; }
  .sidebar .row.active { background: #2a3a52; }
  .sidebar .row .label { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sidebar .row .badge-count { color: #888; font-size: 11px; }
  .sidebar .row .glyph { color: #e0b03c; width: 14px; text-align: center; }
  .content { min-width: 0; }
  h1 { padding: 16px 20px; margin: 0; font-size: 16px; font-weight: 600; border-bottom: 1px solid #2a2a2a; }
  h1 .count { color: #888; font-weight: 400; font-size: 13px; margin-left: 8px; }
  .chips { display: flex; flex-wrap: wrap; gap: 6px; padding: 10px 14px; border-bottom: 1px solid #222; }
  .chip { display: inline-flex; gap: 6px; align-items: center; padding: 6px 10px; background: #1c1c1c;
          border-radius: 999px; color: #ddd; text-decoration: none; font-size: 12px; }
  .chip:hover { background: #243046; }
  .chip-count { color: #888; font-size: 11px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 8px; padding: 12px;
          content-visibility: auto; contain-intrinsic-size: 1px 600px; }
  .cell { position: relative; display: flex; flex-direction: column; align-items: center;
          background: #1c1c1c; border-radius: 6px; overflow: hidden; text-decoration: none; color: inherit;
          contain: content; }
  .cell img { width: 100%; aspect-ratio: 1 / 1; object-fit: cover; background: #000; display: block; }
  .cell .name { width: 100%; padding: 6px 8px; font-size: 11px; overflow: hidden;
                text-overflow: ellipsis; white-space: nowrap; }
  .cell:hover { outline: 2px solid #4a90e2; }
  .badge { position: absolute; top: 6px; right: 6px; padding: 2px 6px; border-radius: 4px;
           background: rgba(0,0,0,0.65); color: #fff; font-size: 10px; }
  .loader { padding: 18px; text-align: center; color: #888; font-size: 12px; }
  .pager { display: inline-block; margin: 12px; padding: 8px 14px; background: #2a3a52; color: #eee;
           border-radius: 6px; text-decoration: none; }

  /* Single-media viewer */
  .viewer { display: flex; flex-direction: column; height: 100vh; background: #000; }
  .vbar { display: flex; align-items: center; gap: 12px; padding: 10px 14px;
          background: rgba(0,0,0,0.6); color: #eee; font-size: 13px; }
  .vbar .back { color: #eee; text-decoration: none; padding: 6px 10px; border-radius: 6px; background: #1c1c1c; }
  .vbar .vname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #ccc; }
  .vbar .vpos { color: #888; font-size: 12px; min-width: 60px; text-align: right; }
  .vstage { position: relative; flex: 1; display: flex; align-items: center; justify-content: center;
            min-height: 0; }
  .vmedia { max-width: 100%; max-height: 100%; object-fit: contain; display: block; }
  .varrow { position: absolute; top: 0; bottom: 0; width: 25%; display: flex; align-items: center;
            color: #fff; font-size: 56px; text-decoration: none; user-select: none;
            opacity: 0.35; transition: opacity 0.15s; }
  .varrow:hover, .varrow:focus { opacity: 1; background: linear-gradient(to right, rgba(0,0,0,0.4), transparent); }
  .vprev { left: 0; justify-content: flex-start; padding-left: 16px; }
  .vnext { right: 0; justify-content: flex-end; padding-right: 16px;
           background: linear-gradient(to left, rgba(0,0,0,0.0), transparent); }
  .vnext:hover, .vnext:focus { background: linear-gradient(to left, rgba(0,0,0,0.4), transparent); }

  @media (max-width: 720px) {
    .layout { grid-template-columns: 1fr; }
    .topbar { display: flex; align-items: center; gap: 12px; padding: 10px 14px;
              background: #181818; border-bottom: 1px solid #2a2a2a; position: sticky; top: 0; z-index: 10; }
    .topbar .brand { font-weight: 600; color: #ccc; }
    .hamburger { display: inline-flex; align-items: center; justify-content: center;
                 min-width: 40px; min-height: 40px; padding: 0 12px;
                 background: #2a2a2a; border-radius: 6px; font-size: 20px; line-height: 1;
                 cursor: pointer; user-select: none; color: #eee; }
    .sidebar { display: none; }
    .navtoggle:checked ~ .layout .sidebar { display: block; position: static; max-height: none;
                                            border-right: none; border-bottom: 1px solid #2a2a2a; }
    .grid { grid-template-columns: repeat(auto-fill, minmax(110px, 1fr)); gap: 4px; padding: 6px; }
    .cell .name { font-size: 10px; padding: 4px 6px; }
    h1 { padding: 12px 14px; font-size: 14px; }
    .varrow { width: 40%; opacity: 0.5; font-size: 48px; }
  }
</style></head><body>`
