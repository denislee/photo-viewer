package webserver

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// fixture wires up an Index seeded with n entries plus an httptest.Server
// exposing the webserver handlers. Each entry is a synthetic JPG under
// root so paths sort deterministically.
func fixture(t *testing.T, n int) (*Server, *httptest.Server, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "pv-web-")
	if err != nil {
		t.Fatal(err)
	}
	libRoot := filepath.Join(tmp, "lib")
	os.MkdirAll(libRoot, 0755)

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	display, err := cache.NewDisplayStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	results := make([]scan.Result, 0, n)
	for i := range n {
		// Pad numerically so lexicographic order matches insertion order.
		name := strings.Repeat("0", 6-len(itoa(i))) + itoa(i) + ".jpg"
		results = append(results, scan.Result{
			Path:    filepath.Join(libRoot, name),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC),
		})
	}
	idx.ReconcileBatch(results)

	s := New(idx, store, display, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/favorites", s.handleFavorites)
	mux.HandleFunc("/year/", s.handleYear)
	mux.HandleFunc("/dir", s.handleDir)
	mux.HandleFunc("/view/", s.handleView)
	mux.HandleFunc("/thumb/", s.handleThumb)
	mux.HandleFunc("/display/", s.handleDisplay)
	mux.HandleFunc("/api/page", s.handleAPIPage)
	ts := httptest.NewServer(mux)

	return s, ts, func() {
		ts.Close()
		idx.Close()
		os.RemoveAll(tmp)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestIndexPageIsBounded(t *testing.T) {
	_, ts, cleanup := fixture(t, 500)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	// Expect at most pageSize cells in the initial render, even though
	// the index holds 500 entries.
	cells := strings.Count(body, `class="cell"`)
	if cells != pageSize {
		t.Errorf("initial page cell count = %d, want %d (pageSize)", cells, pageSize)
	}
	if !strings.Contains(body, `id="loader"`) {
		t.Errorf("loader sentinel missing — infinite scroll won't trigger")
	}
	if !strings.Contains(body, "500 items") {
		t.Errorf("total count not shown in header")
	}
}

func TestAPIPageReturnsJSON(t *testing.T) {
	_, ts, cleanup := fixture(t, 500)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/api/page?from=all&p=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want JSON", ct)
	}
	var payload struct {
		Items []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Video bool   `json:"video"`
		} `json:"items"`
		HasNext bool `json:"hasNext"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != pageSize {
		t.Errorf("page 2 items = %d, want %d", len(payload.Items), pageSize)
	}
	if !payload.HasNext {
		t.Errorf("hasNext = false after page 2 of 3 — should still be more")
	}
	// First item on page 2 should be the (pageSize)-th entry overall.
	wantFirstSuffix := strings.Repeat("0", 6-len(itoa(pageSize))) + itoa(pageSize) + ".jpg"
	if !strings.HasSuffix(payload.Items[0].Name, wantFirstSuffix) {
		t.Errorf("page 2 first item = %s, want suffix %s", payload.Items[0].Name, wantFirstSuffix)
	}
}

func TestAPIPageLastPage(t *testing.T) {
	_, ts, cleanup := fixture(t, 500)
	defer cleanup()
	// pageSize=200 → 500 entries → page 3 has 100 items and hasNext=false.
	resp, err := http.Get(ts.URL + "/api/page?from=all&p=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Items   []struct{} `json:"items"`
		HasNext bool       `json:"hasNext"`
	}
	json.NewDecoder(resp.Body).Decode(&payload)
	if len(payload.Items) != 100 {
		t.Errorf("final page items = %d, want 100", len(payload.Items))
	}
	if payload.HasNext {
		t.Errorf("hasNext = true on final page")
	}
}

// TestAPIPageHasNextBoundary verifies the pageSize+1 heuristic at the exact
// boundary: with pageSize+1 entries, page 1 must report hasNext=true (there is
// a page 2) and page 2 must report hasNext=false (nothing more).
func TestAPIPageHasNextBoundary(t *testing.T) {
	_, ts, cleanup := fixture(t, pageSize+1)
	defer cleanup()

	type resp struct {
		Items   []struct{} `json:"items"`
		HasNext bool       `json:"hasNext"`
	}

	fetch := func(p int) resp {
		t.Helper()
		r, err := http.Get(fmt.Sprintf("%s/api/page?from=all&p=%d", ts.URL, p))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out resp
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}

	p1 := fetch(1)
	if len(p1.Items) != pageSize {
		t.Errorf("page 1 items = %d, want %d", len(p1.Items), pageSize)
	}
	if !p1.HasNext {
		t.Errorf("page 1 hasNext = false, want true (one more page)")
	}

	p2 := fetch(2)
	if len(p2.Items) != 1 {
		t.Errorf("page 2 items = %d, want 1", len(p2.Items))
	}
	if p2.HasNext {
		t.Errorf("page 2 hasNext = true, want false (no more pages)")
	}
}

func TestThumbETag(t *testing.T) {
	_, ts, cleanup := fixture(t, 1)
	defer cleanup()

	// Fetch the API page to learn the thumb id.
	resp, _ := http.Get(ts.URL + "/api/page?from=all&p=1")
	var payload struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if len(payload.Items) == 0 {
		t.Fatal("no items returned")
	}
	id := payload.Items[0].ID

	// The ETag now folds in the source mtime so an in-place edit invalidates
	// the browser cache instead of 304'ing on the stale thumb forever. Entry 0
	// (lexicographically first) is the fixture's ModTime base second=0.
	mtime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	wantETag := `"` + id + "-" + itoa(int(mtime.Unix())) + `"`

	// A stale conditional GET with the old bare-id ETag must NOT match — that
	// was the bug (a content edit kept the same ETag and 304'd forever).
	staleReq, _ := http.NewRequest("GET", ts.URL+"/thumb/"+id, nil)
	staleReq.Header.Set("If-None-Match", `"`+id+`"`)
	staleResp, err := http.DefaultClient.Do(staleReq)
	if err != nil {
		t.Fatal(err)
	}
	staleResp.Body.Close()
	if staleResp.StatusCode == http.StatusNotModified {
		t.Errorf("bare-id ETag still 304'd — mtime is not part of the validator")
	}

	// Conditional GET with the matching mtime-keyed ETag must short-circuit to
	// 304 without touching the thumb store.
	req, _ := http.NewRequest("GET", ts.URL+"/thumb/"+id, nil)
	req.Header.Set("If-None-Match", wantETag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want 304", resp2.StatusCode)
	}
	if etag := resp2.Header.Get("ETag"); etag != wantETag {
		t.Errorf("ETag = %q, want %q", etag, wantETag)
	}
	// The response must no longer be marked immutable, or browsers would skip
	// the revalidation that picks up edits.
	if cc := resp2.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, must not be immutable", cc)
	}
}

func TestGzipMiddleware(t *testing.T) {
	// A large-ish HTML body so compression is unambiguous.
	htmlBody := "<!doctype html>" + strings.Repeat("<div>hello world</div>", 500)
	htmlHandler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, htmlBody)
	}))
	// A binary route: same wrapper, but image/* must pass through untouched so
	// we never re-compress already-compressed media or break range requests.
	imgBody := strings.Repeat("\x00\x01\x02\x03", 500)
	imgHandler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		fmt.Fprint(w, imgBody)
	}))

	mux := http.NewServeMux()
	mux.Handle("/page", htmlHandler)
	mux.Handle("/img", imgHandler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1. HTML with Accept-Encoding: gzip → compressed, transparently decoded.
	req, _ := http.NewRequest("GET", ts.URL+"/page", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	// DisableCompression so the transport doesn't strip Content-Encoding before
	// we can observe it; decode the gzip stream ourselves.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("HTML Content-Encoding = %q, want gzip", enc)
	}
	if vary := resp.Header.Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q, want to include Accept-Encoding", vary)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("response not valid gzip: %v", err)
	}
	got, _ := io.ReadAll(gz)
	resp.Body.Close()
	if string(got) != htmlBody {
		t.Errorf("decoded HTML body mismatch (len got=%d want=%d)", len(got), len(htmlBody))
	}

	// 2. Image route must NOT be gzip-encoded even with Accept-Encoding: gzip.
	ireq, _ := http.NewRequest("GET", ts.URL+"/img", nil)
	ireq.Header.Set("Accept-Encoding", "gzip")
	iresp, err := client.Do(ireq)
	if err != nil {
		t.Fatal(err)
	}
	defer iresp.Body.Close()
	if enc := iresp.Header.Get("Content-Encoding"); enc == "gzip" {
		t.Errorf("image response was gzip-encoded; binary routes must pass through")
	}
	ibody, _ := io.ReadAll(iresp.Body)
	if string(ibody) != imgBody {
		t.Errorf("image body altered by middleware")
	}

	// 3. No Accept-Encoding → plain, uncompressed HTML.
	presp, err := http.Get(ts.URL + "/page")
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if enc := presp.Header.Get("Content-Encoding"); enc == "gzip" {
		t.Errorf("client without Accept-Encoding got gzip")
	}
}

func TestDirOutsideRootRejected(t *testing.T) {
	_, ts, cleanup := fixture(t, 5)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/dir?path=/etc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestAPIPageEmptyDirPathRejected guards W-09: an empty dir path must be
// rejected before filepath.Abs turns "" into the process CWD, which would
// otherwise silently serve the library root as a "dir" view. A valid path
// under the root must still succeed.
func TestAPIPageEmptyDirPathRejected(t *testing.T) {
	s, ts, cleanup := fixture(t, 5)
	defer cleanup()

	// Empty path → 400 (viewFromQuery returns false, handleAPIPage 400s).
	resp, err := http.Get(ts.URL + "/api/page?from=dir&path=&p=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty path status = %d, want 400", resp.StatusCode)
	}

	// A real directory under the root still works.
	valid, err := http.Get(ts.URL + "/api/page?from=dir&path=" + url.QueryEscape(s.libraryRoot) + "&p=1")
	if err != nil {
		t.Fatal(err)
	}
	defer valid.Body.Close()
	if valid.StatusCode != http.StatusOK {
		t.Fatalf("valid path status = %d, want 200", valid.StatusCode)
	}
	var payload struct {
		Items []struct{} `json:"items"`
	}
	if err := json.NewDecoder(valid.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 5 {
		t.Errorf("valid dir items = %d, want 5", len(payload.Items))
	}
}

// apiFixture creates a minimal test server with only /api/favorite registered
// — enough for CSRF boundary tests that need no actual index data.
func apiFixture(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "pv-api-")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	s := New(idx, store, nil, tmp)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/favorite", s.handleAPIFavorite)
	ts := httptest.NewServer(mux)
	return ts, func() {
		ts.Close()
		idx.Close()
		os.RemoveAll(tmp)
	}
}

func TestAPIFavoriteRejectsBadContentType(t *testing.T) {
	ts, cleanup := apiFixture(t)
	defer cleanup()

	body := strings.NewReader(`{"id":"` + strings.Repeat("a", 40) + `","toggle":true}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/favorite", body)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 (Unsupported Media Type)", resp.StatusCode)
	}
}

func TestAPIFavoriteRejectsCrossOrigin(t *testing.T) {
	ts, cleanup := apiFixture(t)
	defer cleanup()

	body := strings.NewReader(`{"id":"` + strings.Repeat("a", 40) + `","toggle":true}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/favorite", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (Forbidden)", resp.StatusCode)
	}
}

// TestAPIFavoriteAcceptsSameOrigin guards W-01: real browsers send an Origin
// header matching the page host on same-origin POSTs, and the handler must
// accept those (and actually flip the flag) rather than 403 them.
func TestAPIFavoriteAcceptsSameOrigin(t *testing.T) {
	tmp, err := os.MkdirTemp("", "pv-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	mediaPath := filepath.Join(tmp, "IMG_0001.jpg")
	idx.ReconcileBatch([]scan.Result{{
		Path:    mediaPath,
		Type:    scan.TypePhoto,
		Size:    3,
		ModTime: time.Now(),
	}})
	id := cache.ThumbIDFor(mediaPath)

	s := New(idx, store, nil, tmp)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/favorite", s.handleAPIFavorite)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"id":"` + id + `","on":true}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/favorite", body)
	req.Header.Set("Content-Type", "application/json")
	// A browser fetch() to the same origin carries these headers.
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (OK)", resp.StatusCode)
	}
	var got struct {
		ID       string `json:"id"`
		Favorite bool   `json:"favorite"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Favorite {
		t.Errorf("response favorite = false, want true")
	}
	if e, ok := idx.GetEntryByThumbID(id); !ok || !e.Favorite {
		t.Errorf("favorite not persisted: ok=%v favorite=%v", ok, e.Favorite)
	}
}

// TestGalleryPageScansSubdirsOnce guards W-08: a large-directory gallery page
// renders both the sidebar "In <dir>" section and the above-grid chip strip,
// but the child-directory scan for the open directory must run exactly once.
// Before W-08 renderSidebar and renderSubdirChips each scanned it independently
// (two os.ReadDir + two O(subtree) grouped-count queries per page); now
// renderGalleryPage computes it once and shares it with both.
func TestGalleryPageScansSubdirsOnce(t *testing.T) {
	tmp, err := os.MkdirTemp("", "pv-web-w08-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	libRoot := filepath.Join(tmp, "lib")
	bigDir := filepath.Join(libRoot, "big")
	subDir := filepath.Join(bigDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Enough entries directly under big/ to cross largeFolderThreshold (so the
	// chip strip renders), plus a handful in the child sub/ so it earns a
	// non-zero drill-down count.
	var results []scan.Result
	for i := range largeFolderThreshold + 5 {
		results = append(results, scan.Result{
			Path:    filepath.Join(bigDir, fmt.Sprintf("%06d.jpg", i)),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	for i := range 5 {
		results = append(results, scan.Result{
			Path:    filepath.Join(subDir, fmt.Sprintf("%06d.jpg", i)),
			Type:    scan.TypePhoto,
			Size:    int64(2000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	idx.ReconcileBatch(results)

	s := New(idx, store, nil, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/dir", s.handleDir)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Count listSubdirs calls for the open directory only. The separate scan of
	// the library root (for the "Folders" section) keys on a different path and
	// doesn't affect this assertion.
	var mu sync.Mutex
	calls := map[string]int{}
	listSubdirsHook = func(dir string) {
		mu.Lock()
		calls[dir]++
		mu.Unlock()
	}
	defer func() { listSubdirsHook = nil }()

	resp, err := http.Get(ts.URL + "/dir?path=" + url.QueryEscape(bigDir))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	mu.Lock()
	got := calls[bigDir]
	mu.Unlock()
	if got != 1 {
		t.Errorf("listSubdirs(%q) called %d times, want 1 (sidebar + chips must share one scan)", bigDir, got)
	}

	// Both surfaces that consume the shared scan must still render.
	page := string(raw)
	if !strings.Contains(page, `class="chips"`) {
		t.Errorf("large-dir chip strip missing from gallery page")
	}
	if !strings.Contains(page, `class="chip"`) {
		t.Errorf("no chip rendered for child directory")
	}
	if !strings.Contains(page, "In big") {
		t.Errorf(`sidebar "In <dir>" section missing from gallery page`)
	}
}

// TestStopForceClosesActiveConnections guards W-02: an explicit Stop must not
// let an in-flight streaming response outlive the "stopped" state. With
// WriteTimeout: 0, http.Server.Shutdown never interrupts an active connection,
// so on shutdown-timeout Stop must fall back to srv.Close() and drop the
// connection. The test parks a handler that only returns when its request
// context is cancelled (mimicking a long /media stream), then asserts Stop
// returns promptly and the parked handler + in-flight client both observe the
// force-close.
func TestStopForceClosesActiveConnections(t *testing.T) {
	// Shorten the graceful-drain window so the timeout→Close path is fast.
	orig := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	defer func() { shutdownTimeout = orig }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	released := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // ship headers so the client's request returns immediately
		}
		close(entered)
		// Simulate a long stream: keep serving until the connection is
		// force-closed (context cancel) rather than finishing on our own.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		close(released)
	})

	// WriteTimeout: 0 mirrors the production server (server.go:213) — the very
	// setting that lets a stream run forever absent the hard close.
	srv := &http.Server{Handler: handler, WriteTimeout: 0}
	s := &Server{srv: srv, listener: ln, running: true, addr: ln.Addr().String()}
	go func() { _ = srv.Serve(ln) }()

	// Fire an in-flight request and wait until it's parked inside the handler.
	type getResult struct {
		resp *http.Response
		err  error
	}
	reqDone := make(chan getResult, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
		reqDone <- getResult{resp, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never entered — request didn't reach the server")
	}

	// Stop must return promptly: Shutdown drains for shutdownTimeout, then the
	// hard close drops the still-active connection. It must NOT block for the
	// handler's own 10 s ceiling.
	stopped := make(chan error, 1)
	start := time.Now()
	go func() { stopped <- s.Stop() }()

	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Stop returned error: %v", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Stop took %v — force-close path not reached promptly", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return — force-close path not reached")
	}

	// The force-close must cancel the parked handler's request context.
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still parked after Stop — connection was not force-closed")
	}

	// And the in-flight client must see the dropped connection: either the
	// request itself errored, or reading the (chunked, unterminated) body fails.
	select {
	case r := <-reqDone:
		if r.err == nil {
			_, readErr := io.ReadAll(r.resp.Body)
			r.resp.Body.Close()
			if readErr == nil {
				t.Errorf("in-flight response completed cleanly; want a broken/closed connection")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never returned after force-close")
	}
}

// trashFixture builds a server whose mux registers /trash (the shared fixture
// omits it) plus n seeded index entries so the sidebar renders category links.
func trashFixture(t *testing.T, n int) (*httptest.Server, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "pv-trash-")
	if err != nil {
		t.Fatal(err)
	}
	libRoot := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(libRoot, 0755); err != nil {
		t.Fatal(err)
	}
	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]scan.Result, 0, n)
	for i := range n {
		results = append(results, scan.Result{
			Path:    filepath.Join(libRoot, fmt.Sprintf("%06d.jpg", i)),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC),
		})
	}
	idx.ReconcileBatch(results)

	s := New(idx, store, nil, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/trash", s.handleTrash)
	ts := httptest.NewServer(mux)
	return ts, func() {
		ts.Close()
		idx.Close()
		os.RemoveAll(tmp)
	}
}

// sidebarHTML extracts the <nav class="sidebar">…</nav> fragment so an
// assertion targets only the sidebar links, not the whole page.
func sidebarHTML(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="sidebar">`)
	if start < 0 {
		t.Fatalf("sidebar nav not found in page")
	}
	rest := body[start:]
	inner, _, ok := strings.Cut(rest, `</nav>`)
	if !ok {
		t.Fatalf("sidebar nav not closed")
	}
	return inner
}

// TestTrashSidebarDefaultState guards W-04: the /trash page must build its
// sidebar links from the default toolbar state (filter "All", RAW visible),
// not a zero-value View. Before the fix, renderSidebar computed
// extraQuery("", false), so every sidebar link carried ?filter=&raw=0 —
// clicking "All media" from /trash silently hid all RAW files.
func TestTrashSidebarDefaultState(t *testing.T) {
	ts, cleanup := trashFixture(t, 3)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/trash")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sb := sidebarHTML(t, string(raw))
	if strings.Contains(sb, "raw=0") {
		t.Errorf("trash sidebar links carry raw=0 (would flip RAW off): %s", sb)
	}
	if strings.Contains(sb, "filter=") {
		t.Errorf("trash sidebar links carry filter= (would flip filter): %s", sb)
	}
	// The "All media" link must be the bare default, matching the sidebar
	// rendered from /.
	if !strings.Contains(sb, `href="/"`) {
		t.Errorf(`trash sidebar missing default "All media" href="/": %s`, sb)
	}
}

// TestTrashSidebarPreservesRAWToggle guards W-04's other half: an explicit
// raw=0 on /trash must thread through the sidebar links so navigating out to a
// gallery keeps the user's RAW-hidden toggle instead of resetting it.
func TestTrashSidebarPreservesRAWToggle(t *testing.T) {
	ts, cleanup := trashFixture(t, 3)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/trash?raw=0")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	sb := sidebarHTML(t, string(raw))
	// The "All media" and "Favorites" category links must preserve raw=0.
	if !strings.Contains(sb, `href="/?raw=0"`) {
		t.Errorf(`trash sidebar "All media" link dropped raw=0: %s`, sb)
	}
	if !strings.Contains(sb, `href="/favorites?raw=0"`) {
		t.Errorf(`trash sidebar "Favorites" link dropped raw=0: %s`, sb)
	}
	// Only the raw toggle was set — no spurious filter= should appear.
	if strings.Contains(sb, "filter=") {
		t.Errorf("trash sidebar links carry filter= with only raw toggled: %s", sb)
	}
}

func TestContentDispositionEscapesQuote(t *testing.T) {
	tmp, err := os.MkdirTemp("", "pv-media-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	// Create a real file whose name contains a double-quote character.
	// On Linux any character except '/' and NUL is valid in a filename.
	fname := `a"b.jpg`
	fpath := filepath.Join(tmp, fname)
	if err := os.WriteFile(fpath, []byte("\xff\xd8\xff"), 0644); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	idx.ReconcileBatch([]scan.Result{{
		Path:    fpath,
		Type:    scan.TypePhoto,
		Size:    3,
		ModTime: time.Now(),
	}})

	s := New(idx, store, nil, tmp)
	mux := http.NewServeMux()
	mux.HandleFunc("/media/", s.handleMedia)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	id := cache.ThumbIDFor(fpath)
	resp, err := http.Get(ts.URL + "/media/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /media/%s status = %d, want 200", id, resp.StatusCode)
	}

	cd := resp.Header.Get("Content-Disposition")
	want := mime.FormatMediaType("inline", map[string]string{"filename": fname})
	if cd != want {
		t.Errorf("Content-Disposition = %q, want %q", cd, want)
	}
}

// TestViewCountCacheSweepsPastCap verifies the W-07 fix: once viewCountCache
// grows past its cap, the locked miss path sweeps entries the TTL has already
// made stale, shrinking the map, while fresh entries survive.
func TestViewCountCacheSweepsPastCap(t *testing.T) {
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	s.viewCountMu.Lock()
	s.viewCountCache = make(map[cache.View]viewCountEntry)
	// Seed past the cap with entries whose timestamps are older than the TTL,
	// so the sweep considers every one of them stale.
	stale := time.Now().Add(-time.Hour)
	for i := range viewCountCacheCap + 50 {
		key := cache.View{Kind: "dir", Dir: fmt.Sprintf("/stale/%d", i)}
		s.viewCountCache[key] = viewCountEntry{n: i, at: stale}
	}
	// Two fresh entries that must survive the sweep.
	freshKeyA := cache.View{Kind: "dir", Dir: "/fresh/a"}
	freshKeyB := cache.View{Kind: "dir", Dir: "/fresh/b"}
	s.viewCountCache[freshKeyA] = viewCountEntry{n: 1, at: time.Now()}
	s.viewCountCache[freshKeyB] = viewCountEntry{n: 2, at: time.Now()}
	before := len(s.viewCountCache)
	s.viewCountMu.Unlock()

	if before <= viewCountCacheCap {
		t.Fatalf("seed size %d is not past cap %d", before, viewCountCacheCap)
	}

	// A brand-new key is a miss, so cachedCountView takes the locked insert
	// path — which runs the sweep because len(cache) > cap.
	s.cachedCountView(cache.View{Kind: "all", Filter: "All", ShowRAW: true})

	s.viewCountMu.Lock()
	defer s.viewCountMu.Unlock()
	after := len(s.viewCountCache)
	if after >= before {
		t.Fatalf("cache did not shrink: before=%d after=%d", before, after)
	}
	if _, ok := s.viewCountCache[freshKeyA]; !ok {
		t.Errorf("fresh entry A was evicted by the sweep")
	}
	if _, ok := s.viewCountCache[freshKeyB]; !ok {
		t.Errorf("fresh entry B was evicted by the sweep")
	}
	// Every stale entry is gone; only the two fresh entries plus the just-
	// inserted "all" view remain.
	if after != 3 {
		t.Errorf("post-sweep size = %d, want 3 (two fresh + inserted)", after)
	}
}

// TestSecureHeaders verifies the W-11 hardening headers: nosniff/CSP/
// Referrer-Policy on the HTML gallery and, crucially, nosniff on the binary
// /thumb route (the same guarantee protects /media, where a RAW original ships
// without a Content-Type and would otherwise be MIME-sniffed).
func TestSecureHeaders(t *testing.T) {
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/thumb/", s.handleThumb)
	mux.HandleFunc("/api/page", s.handleAPIPage)
	ts := httptest.NewServer(secureHeaders(mux))
	defer ts.Close()

	// HTML page carries nosniff + CSP + Referrer-Policy.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("HTML X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("HTML Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
	}
	// The pages rely on inline <style> and <script>; the CSP must allow them or
	// the gallery/viewer break.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") ||
		!strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP %q missing inline script/style allowance", csp)
	}
	if got := resp.Header.Get("Referrer-Policy"); got == "" {
		t.Error("HTML Referrer-Policy header missing")
	}

	// Learn a real thumb id, then confirm the binary route also carries nosniff
	// (set unconditionally by the middleware ahead of the handler).
	apiResp, err := http.Get(ts.URL + "/api/page?from=all&p=1")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	json.NewDecoder(apiResp.Body).Decode(&payload)
	apiResp.Body.Close()
	if len(payload.Items) == 0 {
		t.Fatal("no items returned")
	}
	tResp, err := http.Get(ts.URL + "/thumb/" + payload.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	tResp.Body.Close()
	if got := tResp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("/thumb X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestAuthThrottle verifies the W-11 brute-force throttle: a correct password
// always passes (even after the throttle trips), authFailBurst wrong attempts
// answer 401, the next answers 429, and a subsequent success resets the budget.
func TestAuthThrottle(t *testing.T) {
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	ts := httptest.NewServer(secureHeaders(basicAuth(mux, "secret")))
	defer ts.Close()

	do := func(pass string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.SetBasicAuth("viewer", pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Correct password passes.
	if code := do("secret"); code != http.StatusOK {
		t.Fatalf("correct password status = %d, want 200", code)
	}

	// authFailBurst wrong attempts are answered 401.
	for i := range authFailBurst {
		if code := do("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d status = %d, want 401", i+1, code)
		}
	}
	// The next wrong attempt is throttled.
	if code := do("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt status = %d, want 429", code)
	}

	// A correct password bypasses the throttle (a legitimate user is never
	// locked out) and clears the IP's failure budget.
	if code := do("secret"); code != http.StatusOK {
		t.Fatalf("correct password after throttle status = %d, want 200", code)
	}
	// Budget reset: a wrong attempt is a plain 401 again, not an immediate 429.
	if code := do("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("post-reset wrong attempt status = %d, want 401", code)
	}
}

// TestVideoViewerPosterAndTranscodeFallback guards W-13. Every video viewer
// carries a poster frame pointing at the same /thumb/<id> the grid uses, so the
// element shows the thumbnail instead of a black box before metadata loads. A
// transcode-needed container (.mts) routes its src at the HLS URL even on
// non-Safari browsers (canHls == false) and renders a download-original caption,
// while a natively-playable container (.mp4) keeps the original-media path with
// no fallback note.
func TestVideoViewerPosterAndTranscodeFallback(t *testing.T) {
	tmp, err := os.MkdirTemp("", "pv-vid-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	mtsPath := filepath.Join(tmp, "CLIP_0001.mts")
	mp4Path := filepath.Join(tmp, "CLIP_0002.mp4")
	idx.ReconcileBatch([]scan.Result{
		{Path: mtsPath, Type: scan.TypeVideo, Size: 10, ModTime: time.Now()},
		{Path: mp4Path, Type: scan.TypeVideo, Size: 10, ModTime: time.Now()},
	})

	s := New(idx, store, nil, tmp)
	mux := http.NewServeMux()
	mux.HandleFunc("/view/", s.handleView)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	get := func(id string) string {
		resp, err := http.Get(ts.URL + "/view/" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /view/%s status = %d, want 200", id, resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// .mts needs transcoding: poster present, src routed to HLS, and the
	// download-original fallback caption rendered.
	mtsID := cache.ThumbIDFor(mtsPath)
	mtsBody := get(mtsID)
	if want := `poster="/thumb/` + mtsID + `"`; !strings.Contains(mtsBody, want) {
		t.Errorf(".mts viewer missing poster frame: want %q", want)
	}
	if want := `v.src=needsHls?"/hls/` + mtsID + `/index.m3u8"`; !strings.Contains(mtsBody, want) {
		t.Errorf(".mts viewer does not route src to HLS: want %q", want)
	}
	if !strings.Contains(mtsBody, `var needsHls=true`) {
		t.Errorf(".mts viewer should mark needsHls=true")
	}
	if want := `href="/media/` + mtsID + `" download`; !strings.Contains(mtsBody, want) {
		t.Errorf(".mts viewer missing download-original fallback link: want %q", want)
	}
	if !strings.Contains(mtsBody, "Unsupported format") {
		t.Errorf(".mts viewer missing unsupported-format caption")
	}

	// .mp4 plays natively: poster present, but no transcode fallback note and
	// needsHls=false so the picker keeps the original-media path unchanged.
	mp4ID := cache.ThumbIDFor(mp4Path)
	mp4Body := get(mp4ID)
	if want := `poster="/thumb/` + mp4ID + `"`; !strings.Contains(mp4Body, want) {
		t.Errorf(".mp4 viewer missing poster frame: want %q", want)
	}
	if !strings.Contains(mp4Body, `var needsHls=false`) {
		t.Errorf(".mp4 viewer should mark needsHls=false")
	}
	if strings.Contains(mp4Body, "Unsupported format") {
		t.Errorf(".mp4 viewer should not render the transcode fallback note")
	}
}

// makeDisplayJPEG returns the bytes of a valid w×h JPEG. jpeg.Encode is
// deterministic, so a caller can regenerate the same bytes to byte-compare a
// pass-through response.
func makeDisplayJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// displayFixture wires an index + real on-disk files + a display store behind
// just the /display route. Unlike fixture(), the media files actually exist on
// disk so /display can serve/render them. It seeds two entries sharing one set
// of JPEG bytes: small.jpg (a natively-renderable original, under the
// pass-through threshold) and photo.dng (typed RAW; the JPEG magic makes
// LoadRAWPreview return the bytes whole, so the rendition needs no exiftool).
func displayFixture(t *testing.T) (ts *httptest.Server, ids map[string]string, jpegBytes []byte, mtime time.Time, cleanup func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "pv-display-")
	if err != nil {
		t.Fatal(err)
	}
	libRoot := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	display, err := cache.NewDisplayStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	jpegBytes = makeDisplayJPEG(t, 64, 48)
	mtime = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	files := []struct {
		name string
		typ  scan.MediaType
	}{
		{"small.jpg", scan.TypePhoto},
		{"photo.dng", scan.TypeRAW},
	}
	var results []scan.Result
	ids = make(map[string]string, len(files))
	for _, f := range files {
		p := filepath.Join(libRoot, f.name)
		if err := os.WriteFile(p, jpegBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		results = append(results, scan.Result{
			Path:    p,
			Type:    f.typ,
			Size:    int64(len(jpegBytes)),
			ModTime: mtime,
		})
		ids[f.name] = cache.ThumbIDFor(p)
	}
	idx.ReconcileBatch(results)

	s := New(idx, store, display, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/display/", s.handleDisplay)
	ts = httptest.NewServer(mux)
	return ts, ids, jpegBytes, mtime, func() {
		ts.Close()
		idx.Close()
		os.RemoveAll(tmp)
	}
}

// TestDisplayPassesThroughSmallJPEG verifies that a small, natively-renderable
// original is served byte-for-byte (no rendition) with the original mime type.
func TestDisplayPassesThroughSmallJPEG(t *testing.T) {
	ts, ids, jpegBytes, _, cleanup := displayFixture(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/display/" + ids["small.jpg"])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jpegBytes) {
		t.Errorf("body = %d bytes, want the %d-byte original passed through unchanged", len(got), len(jpegBytes))
	}
}

// TestDisplayRendersRAW verifies that a RAW entry (un-renderable in a browser)
// is served as a JPEG rendition, not the original bytes. Uses the JPEG-preview
// fast path so it needs no exiftool/ffmpeg; a genuinely-encoded RAW would.
func TestDisplayRendersRAW(t *testing.T) {
	ts, ids, jpegBytes, _, cleanup := displayFixture(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/display/" + ids["photo.dng"])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// The rendition is a re-encoded JPEG, so it must decode as one and must not
	// be the raw original bytes.
	if _, _, err := image.DecodeConfig(bytes.NewReader(got)); err != nil {
		t.Errorf("display rendition did not decode as an image: %v", err)
	}
	if bytes.Equal(got, jpegBytes) {
		t.Errorf("RAW display served the original bytes; expected a re-encoded rendition")
	}
}

// TestDisplayETag verifies the mtime-keyed validator: a stale bare-id ETag must
// not 304 (an in-place edit has to invalidate), while the matching mtime-keyed
// ETag short-circuits to 304.
func TestDisplayETag(t *testing.T) {
	ts, ids, _, mtime, cleanup := displayFixture(t)
	defer cleanup()

	id := ids["small.jpg"]
	wantETag := `"` + id + "-" + itoa(int(mtime.Unix())) + `"`

	// Stale bare-id ETag (the pre-fix validator) must NOT match.
	staleReq, _ := http.NewRequest("GET", ts.URL+"/display/"+id, nil)
	staleReq.Header.Set("If-None-Match", `"`+id+`"`)
	staleResp, err := http.DefaultClient.Do(staleReq)
	if err != nil {
		t.Fatal(err)
	}
	staleResp.Body.Close()
	if staleResp.StatusCode == http.StatusNotModified {
		t.Errorf("bare-id ETag still 304'd — mtime is not part of the validator")
	}

	// Matching mtime-keyed ETag must short-circuit to 304.
	req, _ := http.NewRequest("GET", ts.URL+"/display/"+id, nil)
	req.Header.Set("If-None-Match", wantETag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want 304", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != wantETag {
		t.Errorf("ETag = %q, want %q", etag, wantETag)
	}
	if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, must not be immutable", cc)
	}
}
