package webserver

import (
	"encoding/json"
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
	buf := make([]byte, 200000)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

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

	// Conditional GET with matching If-None-Match must short-circuit to
	// 304 without touching the thumb store.
	req, _ := http.NewRequest("GET", ts.URL+"/thumb/"+id, nil)
	req.Header.Set("If-None-Match", `"`+id+`"`)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want 304", resp2.StatusCode)
	}
	if etag := resp2.Header.Get("ETag"); etag != `"`+id+`"` {
		t.Errorf("ETag = %q, want %q", etag, `"`+id+`"`)
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
