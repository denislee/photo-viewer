package webserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// makeTestVideo writes a short synthetic clip at path using ffmpeg, in
// whatever container the extension implies. Returns the duration written.
func makeTestVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration="+itoa(seconds)+":size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+itoa(seconds),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture failed: %v\n%s", err, out)
	}
}

// hlsFixture wires an index holding a single video entry plus a test server
// exposing the HLS handler. Skips when ffmpeg/ffprobe aren't installed.
func hlsFixture(t *testing.T, videoName string, seconds int) (string, *httptest.Server, func()) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	tmp, err := os.MkdirTemp("", "pv-hls-")
	if err != nil {
		t.Fatal(err)
	}
	libRoot := filepath.Join(tmp, "lib")
	os.MkdirAll(libRoot, 0o755)

	vid := filepath.Join(libRoot, videoName)
	makeTestVideo(t, vid, seconds)
	info, _ := os.Stat(vid)

	idx, err := cache.Load(filepath.Join(tmp, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	idx.ReconcileBatch([]scan.Result{{
		Path:    vid,
		Type:    scan.TypeVideo,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}})

	s := New(idx, store, libRoot)
	mux := http.NewServeMux()
	mux.HandleFunc("/hls/", s.handleHLS)
	ts := httptest.NewServer(mux)

	id := cache.ThumbIDFor(vid)
	cleanup := func() {
		ts.Close()
		idx.Close()
		os.RemoveAll(tmp)
	}
	return id, ts, cleanup
}

func TestHLSPlaylist(t *testing.T) {
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 14)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/hls/" + id + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("playlist status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != hlsM3UMime {
		t.Errorf("content-type = %q, want %q", ct, hlsM3UMime)
	}
	body, _ := io.ReadAll(resp.Body)
	pl := string(body)
	if !strings.HasPrefix(pl, "#EXTM3U") {
		t.Errorf("playlist missing #EXTM3U header:\n%s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Errorf("VOD playlist missing #EXT-X-ENDLIST:\n%s", pl)
	}
	// 14s / 6s per segment → 3 segments (6, 6, 2).
	segs := strings.Count(pl, ".ts")
	if segs != 3 {
		t.Errorf("segment count = %d, want 3:\n%s", segs, pl)
	}
	if !strings.Contains(pl, "seg0.ts") || !strings.Contains(pl, "seg2.ts") {
		t.Errorf("expected seg0..seg2 references:\n%s", pl)
	}
}

func TestHLSSegmentTranscodes(t *testing.T) {
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 8)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/hls/" + id + "/seg0.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("segment status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != hlsSegMime {
		t.Errorf("content-type = %q, want %q", ct, hlsSegMime)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Fatal("segment body is empty")
	}
	// An mpegts segment starts with the 0x47 sync byte.
	if body[0] != 0x47 {
		t.Errorf("segment does not start with mpegts sync byte 0x47, got 0x%02x", body[0])
	}
}

func TestHLSSegmentCached(t *testing.T) {
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 8)
	defer cleanup()

	url := ts.URL + "/hls/" + id + "/seg0.ts"
	if _, err := http.Get(url); err != nil {
		t.Fatal(err)
	}
	// Second request should be served from the on-disk cache and return
	// quickly (no second transcode). We assert correctness, not timing:
	// the response must still be a valid segment.
	start := time.Now()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) == 0 || body[0] != 0x47 {
		t.Errorf("cached segment invalid: status=%d len=%d", resp.StatusCode, len(body))
	}
	t.Logf("cached fetch took %s", time.Since(start))
}

func TestHLSRejectsNonVideo(t *testing.T) {
	// A bogus id that resolves to nothing must 404, not 500.
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 4)
	defer cleanup()
	_ = id
	resp, err := http.Get(ts.URL + "/hls/" + strings.Repeat("a", 40) + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", resp.StatusCode)
	}
}
