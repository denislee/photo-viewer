package webserver

import (
	"compress/gzip"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// contentSecurityPolicy is the CSP served on every response (W-11). The pages
// are fully self-contained: a single inline <style> block in <head>
// (style-src 'unsafe-inline'), inline <script> blocks for the grid/viewer
// keybindings and the HLS src picker (script-src 'unsafe-inline'), and
// <img>/<video> sources that are all same-origin /thumb, /media and /hls URLs.
// No external host, font, or XHR target is ever loaded, so pinning every fetch
// directive to 'self' costs the pages nothing while blocking any injected
// off-origin resource. object-src 'none', base-uri 'self', form-action 'self'
// and frame-ancestors 'none' cover the directives that don't fall back to
// default-src; the last stops another page framing the gallery to clickjack
// the favorite/delete controls (W-17).
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self'; media-src 'self'; " +
	"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// secureHeaders wraps next so every response carries hardening headers (W-11).
// X-Content-Type-Options: nosniff matters most on /media, where a RAW original
// ships without a Content-Type and would otherwise be MIME-sniffed by the
// browser. The CSP and Referrer-Policy are meaningful on the HTML documents;
// setting them on binary/JSON responses too is harmless — a directly fetched
// image is not a document that loads sub-resources — and keeps the middleware a
// single unconditional header write with no Content-Type-dependent branching.
// Installed as the outermost wrapper so 401/429 responses are hardened too.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// frame-ancestors 'none' above, for browsers that predate it.
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Auth brute-force throttle (W-11). basicAuth admits at most authFailBurst
// wrong-password attempts per remote IP before answering 429 instead of 401,
// refilling one slot every authFailWindow/authFailBurst. Only requests that
// actually present a wrong password consume the budget — the credential-less
// first request of a browser's Basic-auth handshake does not — and a correct
// password clears the IP's record.
const (
	authFailBurst  = 10
	authFailWindow = time.Minute
)

// authThrottle bounds password guessing without leaking a map entry per
// attacker IP forever (mirrors the viewCountCache sweep, W-07): an IP idle for
// a full refill window carries no state — its bucket has refilled to capacity,
// making it indistinguishable from a fresh one — so the insert path sweeps such
// entries once the map grows past authThrottleSweepAt. A hard cap stops growth
// under a distinct-IP flood; past it new IPs simply aren't tracked (they still
// need the password, they just skip throttling — fail open on tracking, never
// on auth).
const (
	authThrottleSweepAt = 1024
	authThrottleHardCap = 4096
)

type authBucket struct {
	tokens float64
	last   time.Time
}

type authThrottle struct {
	mu      sync.Mutex
	buckets map[string]*authBucket
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{buckets: make(map[string]*authBucket)}
}

// allowFailure records one wrong-password attempt from ip and reports whether
// it is still within budget (true → answer 401; false → throttled, answer 429).
func (t *authThrottle) allowFailure(ip string) bool {
	now := time.Now()
	rate := float64(authFailBurst) / authFailWindow.Seconds() // tokens per second

	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.buckets[ip]
	if !ok {
		if len(t.buckets) >= authThrottleSweepAt {
			t.sweepLocked(now)
		}
		if len(t.buckets) >= authThrottleHardCap {
			return true // map saturated with active offenders; don't grow it
		}
		b = &authBucket{tokens: authFailBurst, last: now}
		t.buckets[ip] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > authFailBurst {
			b.tokens = authFailBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// reset clears any failure record for ip after a successful auth.
func (t *authThrottle) reset(ip string) {
	t.mu.Lock()
	delete(t.buckets, ip)
	t.mu.Unlock()
}

// sweepLocked drops buckets idle long enough to have refilled to capacity;
// such entries are behaviourally identical to being absent. Caller holds mu.
func (t *authThrottle) sweepLocked(now time.Time) {
	for ip, b := range t.buckets {
		if now.Sub(b.last) >= authFailWindow {
			delete(t.buckets, ip)
		}
	}
}

// clientIP is r.RemoteAddr with the port stripped, so the throttle keys on the
// host regardless of the ephemeral source port each connection uses.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// basicAuth wraps next so that every request must present the configured
// password via HTTP Basic auth. The username is fixed ("viewer") — browsers
// require one for the dialog but it carries no security on its own. Wrong
// passwords are rate-limited per remote IP (W-11) so LAN-speed guessing trips a
// 429 after authFailBurst misses; the compare is constant-time with no length
// short-circuit (subtle.ConstantTimeCompare already returns 0 for a mismatched
// length, so there is nothing to gain from leaking it first).
func basicAuth(next http.Handler, password string) http.Handler {
	pw := []byte(password)
	throttle := newAuthThrottle()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, given, ok := r.BasicAuth()
		if ok && subtle.ConstantTimeCompare([]byte(given), pw) == 1 {
			throttle.reset(clientIP(r))
			next.ServeHTTP(w, r)
			return
		}
		// A presented-but-wrong password is a real guess; count it against the
		// IP's budget. A missing credential (ok == false) is the browser's
		// normal pre-auth request, so it gets a plain 401 without consuming the
		// budget — otherwise every legitimate first page load would burn a slot.
		if ok && !throttle.allowFailure(clientIP(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(int(authFailWindow.Seconds())))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="photo-viewer"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// gzipMiddleware wraps next so text responses are gzip-compressed when the
// client advertises support. The ~50 KB gallery HTML and the infinite-scroll
// JSON compress roughly 5×, which matters on the phone-over-WiFi use case this
// server targets. Whether to compress is decided from the Content-Type the
// handler sets (see gzipResponseWriter), so binary responses accidentally
// routed through here still pass straight through.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close() //nolint:errcheck // the response is already committed; nothing to report to
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter compresses the body only when the handler's declared
// Content-Type is a text payload worth compressing (text/html, application/
// json). It decides lazily at the first WriteHeader/Write so it can read the
// Content-Type the handler set; anything else — images, video, an empty 304 —
// passes through uncompressed with headers untouched.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	ct := g.Header().Get("Content-Type")
	if g.Header().Get("Content-Encoding") == "" &&
		(strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/json")) {
		// Length changes once compressed; drop the handler's declared length so
		// net/http switches to chunked framing.
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		// Mirror net/http's implicit 200 on first Write so the Content-Type
		// sniff above runs against the header the handler has already set.
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Close flushes and finishes the gzip stream. A no-op when the response was
// not compressed. Called via defer by gzipMiddleware.
func (g *gzipResponseWriter) Close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}
