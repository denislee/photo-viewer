package webserver

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	s := New(idx, store, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/favorites", s.handleFavorites)
	mux.HandleFunc("/year/", s.handleYear)
	mux.HandleFunc("/dir", s.handleDir)
	mux.HandleFunc("/view/", s.handleView)
	mux.HandleFunc("/thumb/", s.handleThumb)
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
