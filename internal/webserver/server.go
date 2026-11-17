// Package webserver exposes the photo-viewer index over HTTP so a
// remote browser can list and view every indexed media file.
//
// Media is addressed by ThumbID (sha1 of the absolute path) rather than
// by filesystem path — that keeps the wire format opaque and prevents
// directory-traversal probes from reaching files outside the index.
//
// Layout: server.go (Server, lifecycle, routes), middleware.go (security
// headers, Basic auth + throttle, gzip), gallery.go / sidebar.go (HTML
// gallery pages), viewer.go (/view), api.go (/api/*), media.go (/thumb,
// /media, /display), hls.go (/hls), and assets/ (page CSS/JS, embedded).
package webserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

// Server wraps a net/http.Server scoped to one index + thumb store.
// All methods are safe for concurrent use.
type Server struct {
	index       *cache.Index
	store       *cache.ThumbStore
	display     *cache.ThumbStore // larger renditions for /display; may be nil
	libraryRoot string
	trashDir    string // resolved by the caller (cache.TrashDir); "" when no trash

	// trash caches the trash listing (entries + id index) so /trash,
	// /thumb/<trash id>, and /media/<trash id> don't stat the dir per request.
	trash ttlCache[struct{}, trashSnapshot]
	// sidebar caches the root sidebar aggregate per filter/raw toggles.
	sidebar ttlCache[sidebarKey, *sidebarAgg]
	// viewCounts caches CountView per View for the gallery header and sidebar.
	viewCounts ttlCache[cache.View, int]
	// hlsDurations memoises probed HLS source durations per file version, so a
	// trashed or probe-failed entry doesn't fork ffprobe on every playlist and
	// segment request (W4-06). See hlsDuration.
	hlsDurations ttlCache[hlsDurKey, float64]

	mu      sync.Mutex
	srv     *http.Server
	addr    string // resolved listen address (host:port) once running
	running bool

	// HLS on-the-fly transcode state (see hls.go). Lazily initialised by
	// hlsInit so New stays a plain literal. hlsSem bounds concurrent ffmpeg
	// transcodes; hlsInflight is a per-segment singleflight.
	hlsOnce     sync.Once
	hlsSem      chan struct{}
	hlsFlightMu sync.Mutex
	hlsInflight map[string]chan struct{}

	// hlsSweepAt throttles the segment-cache sweep (see maybeSweepHLS): the
	// cache is walked and evicted back under its size cap at most once per
	// hlsSweepInterval, tied to segment-serving activity.
	hlsSweepMu sync.Mutex
	hlsSweepAt time.Time

	// infoCache holds /api/info's exiftool metadata + dimensions per file
	// version, so reopening the info panel doesn't fork exiftool again.
	infoCache infoCache
}

// New constructs an idle server. libraryRoot is used to enumerate the
// top-level directory categories and to confine /dir requests to paths
// rooted at the library. display is a second thumb store rooted at
// <cacheDir>/display that produces the larger browser-viewer renditions the
// /display route serves for RAW/HEIC/TIFF/oversized originals (W-03); it may be
// nil, in which case /display serves every original untouched (correct for
// JPEG/PNG, no rendition for RAW/HEIC). trashDir is the soft-delete directory
// the caller resolved with cache.TrashDir (which falls back to <cacheDir>/trash
// on a read-only library, W4-13); "" means no trash. Call Start to bind a port.
func New(index *cache.Index, store, display *cache.ThumbStore, libraryRoot, trashDir string) *Server {
	return &Server{
		index: index, store: store, display: display, libraryRoot: libraryRoot, trashDir: trashDir,
		trash:      ttlCache[struct{}, trashSnapshot]{ttl: trashEntriesTTL},
		sidebar:    ttlCache[sidebarKey, *sidebarAgg]{ttl: sidebarCacheTTL},
		viewCounts: ttlCache[cache.View, int]{ttl: viewCountTTL, sweepAt: viewCountCacheCap},
		hlsDurations: ttlCache[hlsDurKey, float64]{
			ttl: hlsDurationTTL, sweepAt: hlsDurationCacheCap,
		},
	}
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
	// gz wraps text routes (HTML pages + JSON APIs) with gzip. The binary
	// routes (/thumb, /media, /hls segments) are left on the raw
	// ResponseWriter: their payloads are already compressed, and http.ServeFile
	// there relies on range requests and the io.ReaderFrom sendfile fast path
	// that a wrapping writer would defeat.
	gz := func(h http.HandlerFunc) http.HandlerFunc { return gzipMiddleware(h).ServeHTTP }
	mux.HandleFunc("/", gz(s.handleIndex))
	mux.HandleFunc("/favorites", gz(s.handleFavorites))
	mux.HandleFunc("/trash", gz(s.handleTrash))
	mux.HandleFunc("/year/", gz(s.handleYear))
	mux.HandleFunc("/dir", gz(s.handleDir))
	mux.HandleFunc("/view/", gz(s.handleView))
	mux.HandleFunc("/thumb/", s.handleThumb)
	mux.HandleFunc("/media/", s.handleMedia)
	mux.HandleFunc("/display/", s.handleDisplay)
	mux.HandleFunc("/hls/", s.handleHLS)
	mux.HandleFunc("/api/page", gz(s.handleAPIPage))
	mux.HandleFunc("/api/favorite", gz(s.handleAPIFavorite))
	mux.HandleFunc("/api/info", gz(s.handleAPIInfo))

	var handler http.Handler = mux
	if password != "" {
		handler = basicAuth(handler, password)
	}
	// Outermost wrapper so hardening headers (W-11) land on every response,
	// including the 401/429 that basicAuth returns before the mux is reached.
	handler = secureHeaders(handler)

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming large videos can exceed any fixed budget
		IdleTimeout:  120 * time.Second,
	}
	s.srv = srv
	s.addr = ln.Addr().String()
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

// shutdownTimeout bounds how long Stop waits for in-flight requests to drain
// gracefully before force-closing the remaining connections. It is effectively
// a constant; it stays a var only so tests can shorten it to exercise the
// timeout→Close path quickly.
//
// Deliberately short: with WriteTimeout: 0 (chosen so long video/HLS streams
// aren't cut by a fixed budget), http.Server.Shutdown never interrupts an
// active connection, so a /media download or HLS stream would otherwise
// outlive an explicit Stop indefinitely. For a personal LAN server, cutting a
// stream the moment the user hits Stop is the right bias (privacy over
// completing the transfer), so 2 s is plenty of grace before the hard close.
var shutdownTimeout = 2 * time.Second

// Stop gracefully shuts down the running server, force-closing any connection
// still active once shutdownTimeout elapses. No-op when stopped.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.running = false
	s.addr = ""
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shutdown stops accepting and waits for in-flight requests to drain, but
	// it never interrupts an active connection. On timeout (or any other
	// Shutdown error) hard-close the listeners and connections outright so a
	// streaming /media or HLS response actually ends when the user hits Stop,
	// rather than running on with WriteTimeout: 0 after the GUI reports the
	// server stopped (W-02).
	if err := srv.Shutdown(ctx); err != nil {
		return srv.Close()
	}
	return nil
}

// lookupEntry resolves a thumb id to an Entry, checking both the index and
// the trash listing. Used by handlers that should accept either source.
//
// The trash lookup is a single cache read: trashSnap holds one lock for the
// whole snapshot, replacing the old lock/refresh/lock dance (W4-08).
func (s *Server) lookupEntry(id string) (cache.Entry, bool) {
	if e, ok := s.index.GetEntryByThumbID(id); ok {
		return e, true
	}
	e, ok := s.trashSnap().byID[id]
	return e, ok
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
