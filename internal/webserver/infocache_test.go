package webserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

func TestInfoCacheEvictsLRU(t *testing.T) {
	var c infoCache
	key := func(i int) infoKey { return infoKey{id: strconv.Itoa(i)} }
	for i := range infoCacheCap {
		c.put(key(i), infoVal{dims: strconv.Itoa(i)})
	}
	c.get(key(0)) // touch: key(1) is now the least recently used
	c.put(key(infoCacheCap), infoVal{})

	if _, ok := c.get(key(1)); ok {
		t.Error("least recently used entry survived eviction")
	}
	if v, ok := c.get(key(0)); !ok || v.dims != "0" {
		t.Errorf("recently used entry = %+v, %v; want kept", v, ok)
	}
	if n := c.ll.Len(); n != infoCacheCap || len(c.m) != infoCacheCap {
		t.Errorf("size = %d/%d, want %d", n, len(c.m), infoCacheCap)
	}
}

// TestAPIInfoCachesMetadata is the W-16 guard: reopening the info panel for
// the same file must not fork exiftool again, while per-request fields
// (favorite) stay fresh and a changed file (new mtime) is re-read.
func TestAPIInfoCachesMetadata(t *testing.T) {
	tmp := t.TempDir()
	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(tmp, "IMG_0001.jpg")
	if err := os.WriteFile(photo, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	reindex := func(mt time.Time) {
		idx.ReconcileBatch([]scan.Result{{Path: photo, Type: scan.TypePhoto, Size: 1, ModTime: mt}})
	}
	reindex(mtime)

	var calls int
	orig := mediaInfoFn
	mediaInfoFn = func(string) scan.MediaInfo {
		calls++
		return scan.MediaInfo{Camera: "Test Cam"}
	}
	defer func() { mediaInfoFn = orig }()

	s := New(idx, store, nil, tmp)
	ts := httptest.NewServer(http.HandlerFunc(s.handleAPIInfo))
	defer ts.Close()
	get := func() infoJSON {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/info?id=" + cache.ThumbIDFor(photo))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var info infoJSON
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		return info
	}

	if info := get(); info.Camera != "Test Cam" || info.Favorite {
		t.Fatalf("first info = %+v", info)
	}
	if err := idx.SetFavorite(photo, true); err != nil {
		t.Fatal(err)
	}
	if info := get(); info.Camera != "Test Cam" || !info.Favorite {
		t.Errorf("second info = %+v, want cached camera and fresh favorite", info)
	}
	if calls != 1 {
		t.Errorf("exiftool lookups after two requests = %d, want 1", calls)
	}

	reindex(mtime.Add(time.Hour))
	get()
	if calls != 2 {
		t.Errorf("exiftool lookups after the file changed = %d, want 2", calls)
	}
}
