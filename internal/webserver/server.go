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

// view identifies which sidebar category is active so the renderer can
// highlight the matching row.
type view struct {
	kind string // "all" | "favorites" | "year" | "dir"
	key  string // "" for all/favorites; the year string or absolute dir path
}

// handleIndex serves the default gallery — every indexed media file.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	entries := s.index.All()
	s.renderGallery(w, view{kind: "all"}, "All media", entries)
}

// handleFavorites serves only the entries flagged as favorites.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	entries := s.index.ListFavorites()
	s.renderGallery(w, view{kind: "favorites"}, "Favorites", entries)
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
	entries := s.index.ListByYear(year, "All", true)
	s.renderGallery(w, view{kind: "year", key: rest},
		"Year "+rest, entries)
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
	entries := s.index.ListDir(abs)
	s.renderGallery(w, view{kind: "dir", key: abs},
		filepath.Base(abs), entries)
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

// renderGallery writes the full page (sidebar + grid) for the given list of
// entries. title shows above the grid; v drives the sidebar's "active row"
// highlight.
func (s *Server) renderGallery(w http.ResponseWriter, v view, title string, entries []cache.Entry) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)
	fmt.Fprint(w, `<input type="checkbox" id="navtoggle" class="navtoggle" hidden>`)
	fmt.Fprint(w, `<div class="layout">`)
	s.renderSidebar(w, v)
	fmt.Fprint(w, `<main class="content">`)
	fmt.Fprint(w, mobileTopBar)
	fmt.Fprintf(w, `<h1>%s <span class="count">%d items</span></h1><div class="grid">`,
		html.EscapeString(title), len(entries))
	ctxQ := contextQuery(v)
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
			`<a class="cell" href="%s" title="%s"><img loading="lazy" src="%s" alt="">%s<span class="name">%s</span></a>`,
			viewURL, name, thumbURL, badge, name,
		)
	}
	fmt.Fprint(w, `</div></main></div></body></html>`)
}

// contextQuery encodes a view as a "from=..." query string so the viewer
// page knows which list to walk for prev/next. Empty string means "no
// surrounding context" (treat the media as standalone).
func contextQuery(v view) string {
	switch v.kind {
	case "favorites":
		return "from=favorites"
	case "year":
		return "from=year&y=" + url.QueryEscape(v.key)
	case "dir":
		return "from=dir&path=" + url.QueryEscape(v.key)
	default:
		return "from=all"
	}
}

// renderSidebar emits the categories panel: Favorites, library root,
// top-level subdirectories, and a year list. Active row matches the
// currently-rendered view.
func (s *Server) renderSidebar(w http.ResponseWriter, v view) {
	fmt.Fprint(w, `<nav class="sidebar"><div class="sec">Library</div>`)

	favCount := s.index.CountFavorites("All", true)
	s.sidebarRow(w, "/", "All media", "", s.index.Count(), v.kind == "all")
	s.sidebarRow(w, "/favorites", "Favorites", "★", favCount, v.kind == "favorites")

	// Top-level subdirectories. Sourced from the filesystem (mirroring the
	// native sidebar) so newly-created folders show up without a rebuild.
	subdirs := listSubdirs(s.libraryRoot)
	if len(subdirs) > 0 {
		fmt.Fprint(w, `<div class="sec">Folders</div>`)
		for _, d := range subdirs {
			q := url.Values{"path": []string{d}}.Encode()
			href := "/dir?" + q
			active := v.kind == "dir" && v.key == d
			s.sidebarRow(w, href, filepath.Base(d), "", s.index.CountDir(d), active)
		}
	}

	// Year list. ListByYear gives counts directly via Years().
	years := s.index.Years("All", true)
	if len(years) > 0 {
		fmt.Fprint(w, `<div class="sec">Years</div>`)
		for _, y := range years {
			ys := strconv.Itoa(y.Year)
			active := v.kind == "year" && v.key == ys
			s.sidebarRow(w, "/year/"+ys, ys, "", y.Count, active)
		}
	}
	fmt.Fprint(w, `</nav>`)
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

	list, ctxQ, backHref, backLabel := s.resolveContext(r)

	var prev, next *cache.Entry
	curIdx := -1
	for i := range list {
		if list[i].ThumbID == id {
			curIdx = i
			if i > 0 {
				prev = &list[i-1]
			}
			if i+1 < len(list) {
				next = &list[i+1]
			}
			break
		}
	}

	s.renderViewer(w, cur, prev, next, ctxQ, backHref, backLabel, curIdx, len(list))
}

// resolveContext re-reads the list identified by the `from` query so the
// viewer can compute prev/next. Returns the list, the query string used
// to forward context onward, and a "back to gallery" link + label. When
// `from` is missing or invalid, returns (nil, "", "/", "Gallery").
func (s *Server) resolveContext(r *http.Request) (list []cache.Entry, ctxQ, backHref, backLabel string) {
	q := r.URL.Query()
	switch q.Get("from") {
	case "all":
		return s.index.All(), "from=all", "/", "All media"
	case "favorites":
		return s.index.ListFavorites(), "from=favorites", "/favorites", "Favorites"
	case "year":
		y, err := strconv.Atoi(q.Get("y"))
		if err != nil {
			return nil, "", "/", "Gallery"
		}
		return s.index.ListByYear(y, "All", true),
			"from=year&y=" + url.QueryEscape(q.Get("y")),
			"/year/" + url.QueryEscape(q.Get("y")),
			"Year " + q.Get("y")
	case "dir":
		path := q.Get("path")
		abs, err := filepath.Abs(path)
		if err != nil || !s.withinRoot(abs) {
			return nil, "", "/", "Gallery"
		}
		return s.index.ListDir(abs),
			"from=dir&path=" + url.QueryEscape(abs),
			"/dir?path=" + url.QueryEscape(abs),
			filepath.Base(abs)
	}
	return nil, "", "/", "Gallery"
}

// renderViewer writes the single-media page. curIdx is the position in
// the surrounding list (-1 when no context was provided); total is the
// list length. prev/next are nil at the edges.
func (s *Server) renderViewer(w http.ResponseWriter,
	cur cache.Entry, prev, next *cache.Entry,
	ctxQ, backHref, backLabel string, curIdx, total int,
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
	if curIdx >= 0 && total > 0 {
		fmt.Fprintf(w, `<span class="vpos">%d / %d</span>`, curIdx+1, total)
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
	w.Header().Set("Cache-Control", "public, max-age=86400")
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
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 8px; padding: 12px; }
  .cell { position: relative; display: flex; flex-direction: column; align-items: center;
          background: #1c1c1c; border-radius: 6px; overflow: hidden; text-decoration: none; color: inherit; }
  .cell img { width: 100%; aspect-ratio: 1 / 1; object-fit: cover; background: #000; display: block; }
  .cell .name { width: 100%; padding: 6px 8px; font-size: 11px; overflow: hidden;
                text-overflow: ellipsis; white-space: nowrap; }
  .cell:hover { outline: 2px solid #4a90e2; }
  .badge { position: absolute; top: 6px; right: 6px; padding: 2px 6px; border-radius: 4px;
           background: rgba(0,0,0,0.65); color: #fff; font-size: 10px; }

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
