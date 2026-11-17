package webserver

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sync"
	"testing"
	"time"
)

// compressibleHandler serves a body taken from the "b" query parameter as
// text/html, so every response goes through the gzip path.
func compressibleHandler() http.Handler {
	return gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, r.URL.Query().Get("b"))
	}))
}

// decodeBody gunzips a recorded response body and returns the plain bytes.
func decodeBody(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading gzip stream: %v", err)
	}
	return plain
}

// TestGzipPooledWriterSequentialResponses reuses one middleware across two
// responses, which is what exercises the writer coming back from gzipPool: a
// writer whose state was not fully reset (or that is written to twice) shows up
// here as a corrupt or truncated stream on the second, shorter body.
func TestGzipPooledWriterSequentialResponses(t *testing.T) {
	h := compressibleHandler()

	for _, want := range []string{"first-response", "second"} {
		req := httptest.NewRequest(http.MethodGet, "/?b="+url.QueryEscape(want), nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if got := string(decodeBody(t, rec.Body.Bytes())); got != want {
			t.Fatalf("decoded body = %q, want %q", got, want)
		}
	}
}

// TestGzipMiddlewareAllocs pins the allocation cost of a small compressed
// response. An unpooled gzip.Writer allocates ~1 MB of deflate state per
// response, so a few hundred bytes of JSON cost a megabyte; the pool brings
// that down to a few kilobytes (W4-01). The 100 KB budget leaves room for the
// recorder, the headers and the compressed output while still failing loudly if
// the writer starts being allocated per response again.
func TestGzipMiddlewareAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops Puts at random under -race; the budget only holds in a normal build")
	}
	const body = `{"status": "ok","items":[1,2,3,4,5,6,7]}`
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	// Warm up once so the pool, the header maps and the recorder buffers are
	// already sized when the measurement starts.
	h.ServeHTTP(httptest.NewRecorder(), req)

	const runs = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	perOp := (after.TotalAlloc - before.TotalAlloc) / runs
	t.Logf("gzip middleware: %d bytes allocated per 40 B JSON response", perOp)
	if perOp > 100<<10 {
		t.Fatalf("gzip middleware allocated %d bytes per response, want <= %d", perOp, 100<<10)
	}
}

// TestGzipFlushReachesClient proves a streaming handler behind the middleware
// can push a chunk to the client while it is still working: the handler writes
// chunk1, flushes, and then blocks until the test releases it, so chunk1 can
// only arrive early via gzipResponseWriter.Flush (W3-07).
func TestGzipFlushReachesClient(t *testing.T) {
	const (
		chunk1 = "chunk1"
		chunk2 = "chunk2"
	)

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	flushErr := make(chan error, 1)
	srv := httptest.NewServer(gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, chunk1)
		flushErr <- http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, chunk2)
	})))
	defer srv.Close()
	defer unblock() // runs before srv.Close, so a failed assertion can't wedge it

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	// Set by hand: when the transport negotiates gzip itself it decompresses
	// transparently and resp.Body would not be a gzip stream.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}

	type readResult struct {
		buf []byte
		err error
	}
	got := make(chan readResult, 1)
	go func() {
		buf := make([]byte, len(chunk1))
		_, err := io.ReadFull(zr, buf)
		got <- readResult{buf: buf, err: err}
	}()

	select {
	case res := <-got:
		if res.err != nil {
			t.Fatalf("reading first chunk: %v", res.err)
		}
		if string(res.buf) != chunk1 {
			t.Fatalf("first chunk = %q, want %q", res.buf, chunk1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the flushed first chunk: Flush did not reach the client")
	}

	select {
	case err := <-flushErr:
		if err != nil {
			t.Fatalf("ResponseController.Flush: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reported its Flush result")
	}

	unblock()

	rest, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading rest of stream: %v", err)
	}
	if string(rest) != chunk2 {
		t.Fatalf("remainder = %q, want %q", rest, chunk2)
	}
}
