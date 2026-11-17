package webserver

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestViewerPrefetchesNextImage pins W4-04: the viewer page advertises the
// next photo's /display rendition via pvNav.nextImg so viewer.js can warm it,
// and stays silent when the successor is a video or there is no successor.
func TestViewerPrefetchesNextImage(t *testing.T) {
	s, ts, cleanup := fixture(t, 3)
	defer cleanup()

	ids := make([]string, 3)
	for i := range ids {
		name := fmt.Sprintf("%06d.jpg", i)
		ids[i] = cache.ThumbIDFor(filepath.Join(s.libraryRoot, name))
	}

	view := func(t *testing.T, id string) string {
		t.Helper()
		resp, err := http.Get(ts.URL + "/view/" + id + "?from=all")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /view/%s = %d; want 200", id, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	t.Run("photo successor", func(t *testing.T) {
		body := view(t, ids[0])
		if want := "nextImg: \"/display/" + ids[1] + "\""; !strings.Contains(body, want) {
			t.Errorf("body missing %s", want)
		}
	})

	t.Run("last entry", func(t *testing.T) {
		body := view(t, ids[2])
		if !strings.Contains(body, `nextImg: ""`) {
			t.Errorf(`body missing nextImg: ""`)
		}
	})

	t.Run("video successor", func(t *testing.T) {
		// "000002a.mp4" sorts after "000002.jpg", so it becomes the successor.
		// Create the file too so path lookups in the handlers are safe.
		videoPath := filepath.Join(s.libraryRoot, "000002a.mp4")
		if err := os.WriteFile(videoPath, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
		s.index.ReconcileBatch([]scan.Result{{
			Path:    videoPath,
			Type:    scan.TypeVideo,
			Size:    10,
			ModTime: time.Now(),
		}})
		videoID := cache.ThumbIDFor(videoPath)

		body := view(t, ids[2])
		if !strings.Contains(body, `nextImg: ""`) {
			t.Errorf(`body missing nextImg: ""`)
		}
		if strings.Contains(body, "/display/"+videoID) {
			t.Errorf("body prefetches the video's display rendition")
		}
	})
}
