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
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

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

// dateFolderRe matches subdir basenames the import flow produces: YYYY-MM-DD.
// Mirrors internal/ui.dateFolderRe so the web sidebar's year grouping bins
// directories the same way the native sidebar does.
var dateFolderRe = regexp.MustCompile(`^(\d{4})-\d{2}-\d{2}$`)

// Server wraps a net/http.Server scoped to one index + thumb store.
// All methods are safe for concurrent use.
type Server struct {
	index       *cache.Index
	store       *cache.ThumbStore
	libraryRoot string
	trashDir    string // resolved once at New; "" when no writable location

	// trashMu guards the cached trash entry list. ListTrash re-scans the
	// directory; cache it for a short window so /trash, /thumb/<trash id>,
	// and /media/<trash id> don't all stat the dir on every request.
	trashMu    sync.Mutex
	trashCache []cache.Entry
	trashIndex map[string]cache.Entry
	trashAt    time.Time

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
	trashDir := ""
	if libraryRoot != "" {
		// Match cache.TrashDir's preferred location; if the dir doesn't
		// exist yet there's simply nothing in trash, and the sidebar entry
		// renders with count 0.
		trashDir = filepath.Join(libraryRoot, ".photo-viewer-trash")
	}
	return &Server{index: index, store: store, libraryRoot: libraryRoot, trashDir: trashDir}
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
	mux.HandleFunc("/trash", s.handleTrash)
	mux.HandleFunc("/year/", s.handleYear)
	mux.HandleFunc("/dir", s.handleDir)
	mux.HandleFunc("/view/", s.handleView)
	mux.HandleFunc("/thumb/", s.handleThumb)
	mux.HandleFunc("/media/", s.handleMedia)
	mux.HandleFunc("/api/page", s.handleAPIPage)
	mux.HandleFunc("/api/favorite", s.handleAPIFavorite)
	mux.HandleFunc("/api/info", s.handleAPIInfo)

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
		return viewInfo{
			kind:       "trash",
			title:      "Trash",
			backHref:   "/trash",
			backLabel:  "Trash",
			ctxQuery:   buildCtxQuery("trash", "", "", "All", true),
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
		path := q.Get("path")
		abs, err := filepath.Abs(path)
		if err != nil || !s.withinRoot(abs) {
			return viewInfo{}, false
		}
		return viewInfo{
			v:          cache.View{Kind: "dir", Dir: abs, Filter: filter, ShowRAW: showRAW},
			kind:       "dir",
			title:      filepath.Base(abs),
			backHref:   withExtra("/dir?path=" + url.QueryEscape(abs)),
			backLabel:  filepath.Base(abs),
			ctxQuery:   buildCtxQuery("dir", "", abs, filter, showRAW),
			contextURL: withExtra("/dir?path=" + url.QueryEscape(abs)),
		}, true
	}
	return viewInfo{}, false
}

// buildCtxQuery assembles the `from=...&...` fragment used to thread context
// through viewer + api links.
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
	vi := viewInfo{
		kind:       "trash",
		title:      "Trash",
		backHref:   "/trash",
		backLabel:  "Trash",
		ctxQuery:   buildCtxQuery("trash", "", "", "All", true),
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
	q := r.URL.Query()
	filter := parseFilter(q.Get("filter"))
	showRAW := parseShowRAW(q)
	extra := extraQuery(filter, showRAW)
	vi := viewInfo{
		v:          cache.View{Kind: "dir", Dir: abs, Filter: filter, ShowRAW: showRAW},
		kind:       "dir",
		title:      filepath.Base(abs),
		backHref:   appendQuery("/dir?path="+url.QueryEscape(abs), extra),
		backLabel:  filepath.Base(abs),
		ctxQuery:   buildCtxQuery("dir", "", abs, filter, showRAW),
		contextURL: appendQuery("/dir?path="+url.QueryEscape(abs), extra),
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
		end := offset + pageSize
		if end > total {
			end = total
		}
		entries = all[offset:end]
		hasNext = end < total
	} else {
		total = s.index.CountView(vi.v)
		offset := (page - 1) * pageSize
		if offset > total {
			offset = 0
			page = 1
		}
		entries = s.index.ListPage(vi.v, offset, pageSize)
		hasNext = offset+len(entries) < total
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)
	fmt.Fprint(w, `<input type="checkbox" id="navtoggle" class="navtoggle" hidden>`)
	fmt.Fprint(w, `<div class="layout">`)
	s.renderSidebar(w, vi)
	fmt.Fprint(w, `<main class="content">`)
	fmt.Fprint(w, mobileTopBar)
	s.renderToolbar(w, vi)
	fmt.Fprintf(w, `<h1>%s <span class="count">%d items</span></h1>`,
		html.EscapeString(vi.title), total)

	if vi.v.Kind == "dir" && total > largeFolderThreshold {
		s.renderSubdirChips(w, vi.v.Dir, vi)
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

// cellJSON is the wire shape returned by /api/page. Kept compact since
// 100k-photo libraries will fetch this hundreds of times per session.
type cellJSON struct {
	ID       string `json:"id"`       // ThumbID — drives /thumb/<id> and /view/<id>
	Name     string `json:"name"`     // basename for the caption + img alt
	Video    bool   `json:"video"`    // adds the "video" badge in the corner
	Favorite bool   `json:"favorite"` // toggles the star badge
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

	var entries []cache.Entry
	var total int
	if vi.kind == "trash" {
		all := s.trashEntries()
		total = len(all)
		offset := (page - 1) * pageSize
		if offset > total {
			offset = total
		}
		end := offset + pageSize
		if end > total {
			end = total
		}
		entries = all[offset:end]
	} else {
		offset := (page - 1) * pageSize
		entries = s.index.ListPage(vi.v, offset, pageSize)
		total = s.index.CountView(vi.v)
	}

	items := make([]cellJSON, 0, len(entries))
	for _, e := range entries {
		items = append(items, cellJSON{
			ID:       e.ThumbID,
			Name:     filepath.Base(e.Path),
			Video:    e.Type == scan.TypeVideo,
			Favorite: e.Favorite,
		})
	}
	offset := (page - 1) * pageSize
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

// favoriteRequest is the body shape for POST /api/favorite. Either
// `on` or `toggle` controls the resulting state; with both omitted we
// toggle.
type favoriteRequest struct {
	ID     string `json:"id"`
	On     *bool  `json:"on,omitempty"`
	Toggle bool   `json:"toggle,omitempty"`
}

// handleAPIFavorite toggles or sets the favorite flag for a given thumb id.
// Returns the new state so the client can update the UI deterministically.
func (s *Server) handleAPIFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req favoriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	e, ok := s.index.GetEntryByThumbID(req.ID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	want := !e.Favorite
	if req.On != nil {
		want = *req.On
	}
	if err := s.index.SetFavorite(e.Path, want); err != nil {
		http.Error(w, "set favorite failed", http.StatusInternalServerError)
		return
	}
	resp := struct {
		ID       string `json:"id"`
		Favorite bool   `json:"favorite"`
	}{ID: req.ID, Favorite: want}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// infoJSON is the wire shape returned by /api/info. Lazily fetched per
// /view/<id> request so opening the viewer doesn't pay the exiftool cost
// up front.
type infoJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         string `json:"size"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	Dimensions   string `json:"dimensions"`
	Camera       string `json:"camera"`
	Lens         string `json:"lens"`
	Aperture     string `json:"aperture"`
	ShutterSpeed string `json:"shutter_speed"`
	ISO          string `json:"iso"`
	FocalLength  string `json:"focal_length"`
	Favorite     bool   `json:"favorite"`
}

// handleAPIInfo returns extended metadata for one entry. The viewer's info
// panel calls this on demand so we don't pay the exiftool cost up front.
func (s *Server) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	e, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mi := scan.GetMediaInfo(e.Path)

	info := infoJSON{
		ID:           e.ThumbID,
		Name:         filepath.Base(e.Path),
		Path:         e.Path,
		Type:         titleCase(e.Type.String()),
		Size:         formatBytes(e.Size),
		Modified:     e.ModTime.Local().Format("2006-01-02 15:04:05"),
		Camera:       fallback(mi.Camera, "—"),
		Lens:         fallback(mi.Lens, "—"),
		Aperture:     fallback(mi.Aperture, "—"),
		ShutterSpeed: fallback(mi.ShutterSpeed, "—"),
		ISO:          fallback(mi.ISO, "—"),
		FocalLength:  fallback(mi.FocalLength, "—"),
		Favorite:     e.Favorite,
	}
	if !mi.Created.IsZero() {
		info.Created = mi.Created.Local().Format("2006-01-02 15:04:05")
	} else {
		info.Created = "—"
	}
	// Dimensions are best-effort: a fast image.DecodeConfig works on
	// regular photos; RAW/HEIC/video would need exiftool again and aren't
	// worth the extra fork for an info-panel value.
	info.Dimensions = decodeDimensions(e)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(info)
}

// lookupEntry resolves a thumb id to an Entry, checking both the index and
// the trash listing. Used by handlers that should accept either source.
func (s *Server) lookupEntry(id string) (cache.Entry, bool) {
	if e, ok := s.index.GetEntryByThumbID(id); ok {
		return e, true
	}
	s.trashMu.Lock()
	e, ok := s.trashIndex[id]
	s.trashMu.Unlock()
	if ok {
		return e, true
	}
	// Trash index may be cold; refresh and try once more.
	s.trashEntries()
	s.trashMu.Lock()
	e, ok = s.trashIndex[id]
	s.trashMu.Unlock()
	return e, ok
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
		return "/dir?path=" + url.QueryEscape(vi.v.Dir)
	default:
		return "/"
	}
}

// renderSidebar emits the categories panel: Favorites, Trash, library root,
// top-level subdirectories (with year grouping), and a year list. Active row
// matches the currently-rendered view. Each link preserves the filter+raw
// toolbar state so toggling a category doesn't reset the view options.
func (s *Server) renderSidebar(w http.ResponseWriter, vi viewInfo) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	v := vi.v
	fmt.Fprint(w, `<nav class="sidebar"><div class="sec">Library</div>`)

	favCount := s.index.CountView(cache.View{Kind: "favorites", Filter: v.Filter, ShowRAW: v.ShowRAW})
	s.sidebarRow(w, withExtra("/"), "All media", "", s.index.CountView(cache.View{Kind: "all", Filter: v.Filter, ShowRAW: v.ShowRAW}), vi.kind == "all")
	s.sidebarRow(w, withExtra("/favorites"), "Favorites", "★", favCount, vi.kind == "favorites")

	trashCount := s.countTrash()
	if trashCount > 0 || vi.kind == "trash" {
		s.sidebarRow(w, "/trash", "Trash", "🗑", trashCount, vi.kind == "trash")
	}

	// Top-level subdirectories. Sourced from the filesystem (mirroring the
	// native sidebar) so newly-created folders show up without a rebuild.
	subdirs := listSubdirs(s.libraryRoot)
	if len(subdirs) > 0 {
		fmt.Fprint(w, `<div class="sec">Folders</div>`)
		s.renderSubdirsGrouped(w, s.libraryRoot, subdirs, vi)
	}

	// When viewing a directory, surface its child folders as a nested
	// section so the user can drill down instead of paginating through
	// a 100k-photo flat list.
	if v.Kind == "dir" {
		children := listSubdirs(v.Dir)
		if len(children) > 0 {
			fmt.Fprintf(w, `<div class="sec">In %s</div>`, html.EscapeString(filepath.Base(v.Dir)))
			s.renderSubdirsGrouped(w, v.Dir, children, vi)
		}
	}

	// Year list. Includes per-year counts that respect the active filter.
	years := s.index.Years(v.Filter, v.ShowRAW)
	if len(years) > 0 {
		fmt.Fprint(w, `<div class="sec">Years</div>`)
		for _, y := range years {
			ys := strconv.Itoa(y.Year)
			active := vi.kind == "year" && v.Year == y.Year
			s.sidebarRow(w, withExtra("/year/"+ys), ys, "", y.Count, active)
		}
	}
	fmt.Fprint(w, `</nav>`)
}

// renderSubdirsGrouped renders the immediate child directories of parent.
// Subdirs whose basename matches YYYY-MM-DD are bucketed under collapsible
// YYYY <details> groups so a folder with thousands of date-named children
// doesn't blow out the sidebar. Non-date directories render normally
// above the year buckets.
func (s *Server) renderSubdirsGrouped(w http.ResponseWriter, parent string, subdirs []string, vi viewInfo) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }
	counts := s.index.CountChildDirsFiltered(parent, vi.v.Filter, vi.v.ShowRAW)

	type bucket struct {
		dirs  []string
		total int
	}
	buckets := map[string]*bucket{}
	var nonDate []string
	for _, d := range subdirs {
		m := dateFolderRe.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			nonDate = append(nonDate, d)
			continue
		}
		b, ok := buckets[m[1]]
		if !ok {
			b = &bucket{}
			buckets[m[1]] = b
		}
		b.dirs = append(b.dirs, d)
		b.total += counts[d]
	}

	for _, d := range nonDate {
		q := url.Values{"path": []string{d}}.Encode()
		href := withExtra("/dir?" + q)
		active := vi.kind == "dir" && vi.v.Dir == d
		s.sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
	}

	years := make([]string, 0, len(buckets))
	for y := range buckets {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		b := buckets[y]
		// Open the bucket by default if it contains the active directory,
		// so users land on an expanded tree without an extra click.
		openAttr := ""
		for _, d := range b.dirs {
			if vi.kind == "dir" && vi.v.Dir == d {
				openAttr = " open"
				break
			}
		}
		fmt.Fprintf(w,
			`<details class="year-bucket" data-year="%s"%s><summary><span class="label">%s</span><span class="badge-count">%d</span></summary>`,
			html.EscapeString(y), openAttr, html.EscapeString(y), b.total)
		sort.Strings(b.dirs)
		for _, d := range b.dirs {
			q := url.Values{"path": []string{d}}.Encode()
			href := withExtra("/dir?" + q)
			active := vi.kind == "dir" && vi.v.Dir == d
			s.sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
		}
		fmt.Fprint(w, `</details>`)
	}
}

// renderSubdirChips renders a compact strip of child-folder chips above
// the grid for large directories. Cheap visual cue that there are
// subfolders worth drilling into without scrolling past the grid.
func (s *Server) renderSubdirChips(w http.ResponseWriter, dir string, vi viewInfo) {
	children := listSubdirs(dir)
	if len(children) == 0 {
		return
	}
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	counts := s.index.CountChildDirsFiltered(dir, vi.v.Filter, vi.v.ShowRAW)
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
		href := appendQuery("/dir?"+q, extra)
		fmt.Fprintf(w,
			`<a class="chip" href="%s"><span>%s</span><span class="chip-count">%d</span></a>`,
			html.EscapeString(href), html.EscapeString(c.name), c.count)
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
		cls, html.EscapeString(href), g, html.EscapeString(label), count,
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

// handleView renders a single-media page with the image or video centered
// and prev/next links into the surrounding list. The `from` query carries
// the list context — without it, the viewer renders without nav arrows.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/view/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	cur, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	vi, hasCtx := s.viewFromQuery(r.URL.Query())
	var prev, next *cache.Entry
	var pos, total int
	var ctxQ, backHref, backLabel string
	if hasCtx {
		switch vi.kind {
		case "trash":
			prev, next, pos, total = s.trashNeighbors(cur.Path)
		default:
			prev, next, pos, total = s.index.Neighbors(vi.v, cur.Path)
		}
		ctxQ = vi.ctxQuery
		backHref = vi.backHref
		backLabel = vi.backLabel
	} else {
		backHref = "/"
		backLabel = "Gallery"
	}

	s.renderViewer(w, cur, prev, next, ctxQ, backHref, backLabel, pos, total)
}

// trashNeighbors returns the prev/next/pos/total tuple for the trash listing,
// matching the shape of cache.Index.Neighbors so the viewer can treat the
// two sources identically.
func (s *Server) trashNeighbors(path string) (prev, next *cache.Entry, pos, total int) {
	entries := s.trashEntries()
	total = len(entries)
	for i, e := range entries {
		if e.Path == path {
			pos = i + 1
			if i > 0 {
				p := entries[i-1]
				prev = &p
			}
			if i < len(entries)-1 {
				n := entries[i+1]
				next = &n
			}
			return
		}
	}
	return nil, nil, 0, total
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

	star := "☆"
	starCls := "vfav"
	if cur.Favorite {
		star = "★"
		starCls += " on"
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
	fmt.Fprintf(w, `<button type="button" class="%s" id="favBtn" data-id="%s" title="Toggle favorite (f)">%s</button>`,
		starCls, cur.ThumbID, star)
	fmt.Fprintf(w, `<button type="button" class="vinfo" id="infoBtn" data-id="%s" title="Info (i)">i</button>`, cur.ThumbID)
	fmt.Fprint(w, `</header>`)

	fmt.Fprint(w, `<div class="vstage" id="vstage">`)
	mediaURL := "/media/" + cur.ThumbID
	if cur.Type == scan.TypeVideo {
		fmt.Fprintf(w, `<video class="vmedia" id="vmedia" controls preload="metadata" playsinline src="%s"></video>`, mediaURL)
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

	// Info panel, hidden by default. Populated from /api/info on first show.
	fmt.Fprint(w, `<aside class="vinfo-panel" id="infoPanel" hidden></aside>`)

	fmt.Fprint(w, `</div>`)

	// Inline the keybinding + favorite + info JS so the viewer doesn't
	// require an external script file.
	fmt.Fprintf(w, viewerScript, prevHref, nextHref, backHref)
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
	etag := `"` + id + `"`
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	e, ok := s.lookupEntry(id)
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
	e, ok := s.lookupEntry(id)
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

// decodeDimensions returns "WxH" for normal photo formats. RAW/HEIC/video
// need a heavier decoder and aren't worth the cost for an info panel; they
// get "—".
func decodeDimensions(e cache.Entry) string {
	switch e.Type {
	case scan.TypePhoto:
	default:
		return "—"
	}
	f, err := os.Open(e.Path)
	if err != nil {
		return "—"
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%d × %d", cfg.Width, cfg.Height)
}

// fallback returns alt when s is empty. Keeps info-panel cells readable
// when an exif tag isn't present.
func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// titleCase upper-cases the first byte of s. Trivial helper that mirrors
// ui.titleCaseGio so the type column in the info panel matches the GUI.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// formatBytes is a compact human size formatter used by the info panel.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// mobileTopBar is emitted at the top of every gallery's .content so the
// hamburger has a place to live. On desktop the bar is hidden via CSS.
const mobileTopBar = `<div class="topbar"><label for="navtoggle" class="hamburger" aria-label="Menu">☰</label><span class="brand">Photo Viewer</span></div>`

// gridScript handles infinite scroll plus keyboard navigation (j/k/h/l +
// arrows) over the cell grid. Cells are built via createElement so we never
// parse network strings as HTML.
const gridScript = `<script>
(function(){
  var grid = document.getElementById('grid');
  if (!grid) return;
  var loader = document.getElementById('loader');
  var nextPage = parseInt(grid.dataset.nextPage || '2', 10);
  var hasNext  = grid.dataset.hasNext === 'true';
  var from     = grid.dataset.from || '';
  var loading  = false;
  var selected = -1;

  function cells() { return grid.querySelectorAll('a.cell'); }

  function setSelected(i) {
    var all = cells();
    if (all.length === 0) return;
    if (i < 0) i = 0;
    if (i >= all.length) i = all.length - 1;
    if (selected >= 0 && selected < all.length) all[selected].classList.remove('focus');
    selected = i;
    all[i].classList.add('focus');
    all[i].scrollIntoView({block: 'nearest', inline: 'nearest'});
    all[i].focus({preventScroll: true});
  }

  function colsPerRow() {
    var all = cells();
    if (all.length < 2) return 1;
    var firstTop = all[0].getBoundingClientRect().top;
    for (var i = 1; i < all.length; i++) {
      if (all[i].getBoundingClientRect().top > firstTop + 1) return i;
    }
    return all.length;
  }

  function buildCell(item) {
    var a = document.createElement('a');
    a.className = 'cell';
    a.href = '/view/' + item.id + (from ? '?' + from : '');
    a.title = item.name;
    a.dataset.id = item.id;

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
    if (item.favorite) {
      var star = document.createElement('span');
      star.className = 'star';
      star.title = 'Favorite';
      star.textContent = '★';
      a.appendChild(star);
    }

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = item.name;
    a.appendChild(name);
    return a;
  }

  function fetchNext() {
    if (loading || !hasNext || !loader) return;
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
        if (!hasNext && loader) {
          loader.remove();
          obs.disconnect();
        }
      })
      .catch(function(){
        loading = false;
        if (loader) loader.textContent = 'Failed to load - scroll to retry.';
      });
  }

  var obs;
  if (loader) {
    obs = new IntersectionObserver(function(entries){
      entries.forEach(function(e){ if (e.isIntersecting) fetchNext(); });
    }, { rootMargin: '600px 0px' });
    obs.observe(loader);
  }

  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea|select)$/i.test(e.target.tagName)) return;
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    var all = cells();
    if (all.length === 0) return;
    var cols = colsPerRow();
    var key = e.key;
    if (key === 'j' || key === 'ArrowDown') { setSelected((selected < 0 ? 0 : selected + cols)); e.preventDefault(); return; }
    if (key === 'k' || key === 'ArrowUp')   { setSelected((selected < 0 ? 0 : selected - cols)); e.preventDefault(); return; }
    if (key === 'l' || key === 'ArrowRight'){ setSelected((selected < 0 ? 0 : selected + 1));    e.preventDefault(); return; }
    if (key === 'h' || key === 'ArrowLeft') { setSelected((selected < 0 ? 0 : selected - 1));    e.preventDefault(); return; }
    if (key === 'G') { setSelected(all.length - 1); e.preventDefault(); return; }
    if (key === 'g') {
      if (window._gPending) { setSelected(0); window._gPending = false; e.preventDefault(); return; }
      window._gPending = true;
      setTimeout(function(){ window._gPending = false; }, 600);
      return;
    }
    if (key === 'Enter' && selected >= 0) { all[selected].click(); e.preventDefault(); return; }
    if (key === 'f' && selected >= 0) {
      toggleFavorite(all[selected]);
      e.preventDefault();
      return;
    }
  });

  function toggleFavorite(cell) {
    if (!cell) return;
    var id = cell.dataset.id;
    if (!id) return;
    fetch('/api/favorite', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, toggle: true }),
    }).then(function(r){ return r.json(); })
      .then(function(d){
        var star = cell.querySelector('.star');
        if (d.favorite) {
          if (!star) {
            star = document.createElement('span');
            star.className = 'star';
            star.title = 'Favorite';
            star.textContent = '★';
            cell.appendChild(star);
          }
        } else if (star) {
          star.remove();
        }
      })
      .catch(function(){});
  }
})();
</script>`

// viewerScript wires keyboard nav, the favorite button, and the info
// panel toggle. Three placeholders are filled by Sprintf: prevHref,
// nextHref, backHref.
const viewerScript = `<script>
(function(){
  var prev=%q, next=%q, back=%q;
  var infoPanel = document.getElementById('infoPanel');
  var infoBtn   = document.getElementById('infoBtn');
  var favBtn    = document.getElementById('favBtn');
  var vmedia    = document.getElementById('vmedia');

  function go(href){ if (href) location.href = href; }

  function toggleInfo() {
    if (!infoPanel) return;
    if (infoPanel.hasAttribute('hidden')) {
      infoPanel.removeAttribute('hidden');
      if (!infoPanel.dataset.loaded) {
        loadInfo();
      }
    } else {
      infoPanel.setAttribute('hidden', '');
    }
  }

  function row(label, value) {
    var l = document.createElement('div');
    l.className = 'ip-label';
    l.textContent = label;
    var v = document.createElement('div');
    v.className = 'ip-value';
    v.textContent = value;
    infoPanel.appendChild(l);
    infoPanel.appendChild(v);
  }

  function loadInfo() {
    if (!infoBtn) return;
    var id = infoBtn.dataset.id;
    if (!id) return;
    infoPanel.textContent = 'Loading...';
    fetch('/api/info?id=' + encodeURIComponent(id), { credentials: 'same-origin' })
      .then(function(r){ return r.json(); })
      .then(function(d){
        infoPanel.textContent = '';
        var title = document.createElement('h3');
        title.className = 'ip-title';
        title.textContent = d.name || '';
        infoPanel.appendChild(title);
        row('Path', d.path);
        row('Type', d.type);
        row('Size', d.size);
        row('Created', d.created);
        row('Modified', d.modified);
        row('Dimensions', d.dimensions);
        row('Camera', d.camera);
        row('Lens', d.lens);
        var settings = [d.aperture, d.shutter_speed, 'ISO ' + d.iso, d.focal_length].join('  ');
        row('Settings', settings);
        row('Favorite', d.favorite ? 'Yes' : 'No');
        infoPanel.dataset.loaded = '1';
      })
      .catch(function(){ infoPanel.textContent = 'Failed to load info.'; });
  }

  function toggleFav() {
    if (!favBtn) return;
    var id = favBtn.dataset.id;
    if (!id) return;
    fetch('/api/favorite', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, toggle: true }),
    }).then(function(r){ return r.json(); })
      .then(function(d){
        if (d.favorite) {
          favBtn.classList.add('on');
          favBtn.textContent = '★';
        } else {
          favBtn.classList.remove('on');
          favBtn.textContent = '☆';
        }
        // Invalidate any cached info panel so its Favorite row re-renders
        // on the next open.
        if (infoPanel) infoPanel.dataset.loaded = '';
      })
      .catch(function(){});
  }

  if (infoBtn) infoBtn.addEventListener('click', toggleInfo);
  if (favBtn)  favBtn.addEventListener('click', toggleFav);

  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea)$/i.test(e.target.tagName)) return;
    var key = e.key;
    if (key === 'ArrowLeft' || key === 'h' || key === 'k') { go(prev); return; }
    if (key === 'ArrowRight'|| key === 'l' || key === 'j') { go(next); return; }
    if (key === 'Escape' || key === 'q') { go(back); return; }
    if (key === 'f') { toggleFav(); e.preventDefault(); return; }
    if (key === 'i') { toggleInfo(); e.preventDefault(); return; }
    if (key === ' ' && vmedia && vmedia.tagName === 'VIDEO') {
      if (vmedia.paused) vmedia.play(); else vmedia.pause();
      e.preventDefault();
      return;
    }
    if (key === 'm' && vmedia && vmedia.tagName === 'VIDEO') {
      vmedia.muted = !vmedia.muted;
      e.preventDefault();
      return;
    }
    if ((key === '[' || key === ']') && vmedia && vmedia.tagName === 'VIDEO') {
      vmedia.currentTime += (key === ']' ? 5 : -5);
      e.preventDefault();
      return;
    }
  });
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
  .year-bucket > summary { list-style: none; cursor: pointer; padding: 8px 16px;
                           color: #ddd; font-size: 12px; display: flex; align-items: center; gap: 6px;
                           user-select: none; }
  .year-bucket > summary::-webkit-details-marker { display: none; }
  .year-bucket > summary::before { content: '▸'; color: #888; font-size: 10px; width: 10px; }
  .year-bucket[open] > summary::before { content: '▾'; }
  .year-bucket > summary:hover { background: #232323; }
  .year-bucket > summary .label { flex: 1; }
  .year-bucket > summary .badge-count { color: #888; font-size: 11px; }
  .year-bucket .row { padding-left: 32px; }
  .content { min-width: 0; }

  /* Toolbar */
  .toolbar { display: flex; align-items: center; gap: 12px; padding: 8px 14px;
             border-bottom: 1px solid #2a2a2a; background: #161616; flex-wrap: wrap; }
  .seg-group { display: inline-flex; background: #1c1c1c; border-radius: 6px; overflow: hidden; }
  .seg { padding: 6px 12px; color: #ccc; text-decoration: none; font-size: 12px;
         border-right: 1px solid #2a2a2a; }
  .seg-group .seg:last-child { border-right: none; }
  .seg:hover { background: #243046; }
  .seg.active { background: #2a3a52; color: #fff; }
  .seg.toggle { border-radius: 6px; background: #1c1c1c; border: 1px solid #2a2a2a; }
  .seg.toggle.active { background: #2a3a52; border-color: #3a5a82; color: #fff; }

  h1 { padding: 12px 20px; margin: 0; font-size: 16px; font-weight: 600; border-bottom: 1px solid #2a2a2a; }
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
  .cell:hover, .cell.focus { outline: 2px solid #4a90e2; }
  .badge { position: absolute; top: 6px; right: 6px; padding: 2px 6px; border-radius: 4px;
           background: rgba(0,0,0,0.65); color: #fff; font-size: 10px; }
  .cell .star { position: absolute; top: 6px; left: 6px; color: #e0b03c;
                font-size: 14px; text-shadow: 0 0 4px rgba(0,0,0,0.8); }
  .loader { padding: 18px; text-align: center; color: #888; font-size: 12px; }
  .pager { display: inline-block; margin: 12px; padding: 8px 14px; background: #2a3a52; color: #eee;
           border-radius: 6px; text-decoration: none; }

  /* Single-media viewer */
  .viewer { display: flex; flex-direction: column; height: 100vh; background: #000; position: relative; }
  .vbar { display: flex; align-items: center; gap: 12px; padding: 10px 14px;
          background: rgba(0,0,0,0.6); color: #eee; font-size: 13px; }
  .vbar .back { color: #eee; text-decoration: none; padding: 6px 10px; border-radius: 6px; background: #1c1c1c; }
  .vbar .vname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #ccc; }
  .vbar .vpos { color: #888; font-size: 12px; min-width: 60px; text-align: right; }
  .vbar .vfav, .vbar .vinfo { background: #1c1c1c; color: #ddd; border: 1px solid #2a2a2a;
            border-radius: 6px; padding: 4px 10px; font-size: 14px; cursor: pointer; }
  .vbar .vfav.on { color: #e0b03c; border-color: #4a3a18; }
  .vbar .vinfo:hover, .vbar .vfav:hover { background: #243046; }
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

  .vinfo-panel { position: absolute; right: 0; top: 0; bottom: 0; width: 320px; max-width: 60vw;
                 background: rgba(20, 22, 26, 0.96); color: #eee; padding: 16px;
                 overflow-y: auto; border-left: 1px solid #2a2a2a;
                 display: grid; grid-template-columns: max-content 1fr; gap: 4px 10px;
                 align-content: start; font-size: 12px; }
  .vinfo-panel[hidden] { display: none; }
  .ip-title { grid-column: 1 / -1; margin: 0 0 8px; font-size: 14px; word-break: break-all; }
  .ip-label { color: #9095a0; font-weight: 600; font-size: 11px; text-transform: uppercase; padding-top: 4px; }
  .ip-value { color: #eee; word-break: break-word; }

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
    .vinfo-panel { width: 100vw; max-width: 100vw; }
  }
</style></head><body>`
