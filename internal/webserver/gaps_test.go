package webserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestHLSFollowerSharesLeaderResult pins the follower side of the per-segment
// singleflight: a request that finds another transcode already in flight waits
// on the leader's channel instead of forking a second ffmpeg, then reports the
// leader's outcome (a fresh segment on disk = success, nothing on disk =
// failure) while a cancelled follower bails out immediately without waiting.
func TestHLSFollowerSharesLeaderResult(t *testing.T) {
	s, id, cleanup := indexedVideoServer(t, 14000)
	defer cleanup()

	e, ok := s.lookupEntry(id)
	if !ok {
		t.Fatal("video entry not found")
	}
	s.hlsInit()
	dst := s.hlsSegPath(id, 0)

	// registerLeader installs a fake leader for dst by hand, returning the
	// channel the follower will wait on. Each subtest gets a clean slate: the
	// map entry is dropped and the destination removed when it ends.
	registerLeader := func(t *testing.T) chan struct{} {
		t.Helper()
		ch := make(chan struct{})
		s.hlsFlightMu.Lock()
		s.hlsInflight[dst] = ch
		s.hlsFlightMu.Unlock()
		t.Cleanup(func() {
			s.hlsFlightMu.Lock()
			delete(s.hlsInflight, dst)
			s.hlsFlightMu.Unlock()
			os.Remove(dst)
		})
		return ch
	}

	t.Run("leader success", func(t *testing.T) {
		ch := registerLeader(t)

		done := make(chan error, 1)
		go func() { done <- s.transcodeSegment(context.Background(), e, 0, dst) }()

		// The leader produced a segment newer than the source: the follower
		// must observe it and report success without transcoding anything.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("segment bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := e.ModTime.Add(time.Minute)
		if err := os.Chtimes(dst, mt, mt); err != nil {
			t.Fatal(err)
		}
		close(ch)

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("follower err = %v, want nil (leader wrote a fresh segment)", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("follower did not return within 2s")
		}
	})

	t.Run("leader failure", func(t *testing.T) {
		ch := registerLeader(t)

		done := make(chan error, 1)
		go func() { done <- s.transcodeSegment(context.Background(), e, 0, dst) }()

		// The leader died without leaving a segment behind.
		close(ch)

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("follower err = nil, want a generation failure")
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("follower err = %v, want a generation failure, not a context error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("follower did not return within 2s")
		}
	})

	t.Run("follower cancel", func(t *testing.T) {
		ch := registerLeader(t)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.transcodeSegment(ctx, e, 0, dst) }()

		// The client disconnected while still waiting on the leader.
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("follower err = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled follower blocked instead of returning promptly")
		}

		// The leader's channel must still be open: a cancelling follower does
		// not own (or close) the leader's signal.
		select {
		case <-ch:
			t.Error("leader channel was closed by the cancelled follower")
		default:
		}
		close(ch)
	})
}

// TestHLSConcurrentSegmentRequests guards the singleflight from the client's
// side: two simultaneous requests for the same not-yet-cached segment must
// both succeed with the identical mpegts bytes, not one 200 and one 500.
func TestHLSConcurrentSegmentRequests(t *testing.T) {
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 8)
	defer cleanup()

	url := ts.URL + "/hls/" + id + "/seg0.ts"
	type result struct {
		code int
		body []byte
		err  error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			results[i] = result{code: resp.StatusCode, body: body, err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d failed: %v", i, r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, r.code)
		}
		if len(r.body) == 0 {
			t.Fatalf("request %d body is empty", i)
		}
		if r.body[0] != 0x47 {
			t.Fatalf("request %d does not start with mpegts sync byte 0x47, got 0x%02x", i, r.body[0])
		}
	}
	if !bytes.Equal(results[0].body, results[1].body) {
		t.Errorf("concurrent responses differ: %d vs %d bytes", len(results[0].body), len(results[1].body))
	}
}

// TestHLSNonZeroSegment guards the -output_ts_offset re-anchoring: seg1 is
// transcoded from a 6 s seek, and its timestamps must be offset to ~6 s so the
// player stitches segments without a gap. A missing offset would leave the
// segment starting at 0 and make playback stutter/restart.
func TestHLSNonZeroSegment(t *testing.T) {
	id, ts, cleanup := hlsFixture(t, "clip.mkv", 14)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/hls/" + id + "/seg1.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seg1 status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("seg1 body is empty")
	}
	if body[0] != 0x47 {
		t.Fatalf("seg1 does not start with mpegts sync byte 0x47, got 0x%02x", body[0])
	}
	if len(body)%188 != 0 {
		t.Errorf("seg1 length %d is not a multiple of the 188-byte mpegts packet", len(body))
	}

	segPath := filepath.Join(t.TempDir(), "seg1.ts")
	if err := os.WriteFile(segPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=start_time",
		"-of", "default=nw=1:nk=1",
		segPath).Output()
	if err != nil {
		t.Fatalf("ffprobe on seg1: %v", err)
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parse start_time %q: %v", strings.TrimSpace(string(out)), err)
	}
	t.Logf("seg1 format start_time = %v s (hlsSegDur = %v)", start, hlsSegDur)
	if start < hlsSegDur-1 || start > hlsSegDur+2 {
		t.Errorf("seg1 start_time = %v, want within [%v, %v]", start, hlsSegDur-1, hlsSegDur+2)
	}
}

// TestHLSPhotoIDNotFound covers the "valid id, wrong media type" branch: a
// photo's thumb id resolves in the index, but /hls must still 404 both the
// playlist and a segment instead of trying to transcode a still image.
func TestHLSPhotoIDNotFound(t *testing.T) {
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	// fixture's first entry is lib/000000.jpg (a photo).
	id := cache.ThumbIDFor(filepath.Join(s.libraryRoot, "000000.jpg"))
	if e, ok := s.lookupEntry(id); !ok || e.Type != scan.TypePhoto {
		t.Fatalf("fixture entry %s = %+v, %v; want a photo", id, e, ok)
	}

	for _, name := range []string{"index.m3u8", "seg0.ts"} {
		rec := httptest.NewRecorder()
		s.handleHLS(rec, httptest.NewRequest(http.MethodGet, "/hls/"+id+"/"+name, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /hls/%s/%s = %d, want 404 (photo, not video)", id, name, rec.Code)
		}
	}
}

// TestAPIInfoStatusAndShape pins /api/info's error contract and wire shape: a
// missing/malformed id is a 400, a well-formed unknown id a 404, and a real
// entry yields JSON with library-relative paths, decoded photo dimensions and
// em-dash fallbacks for absent metadata.
func TestAPIInfoStatusAndShape(t *testing.T) {
	orig := mediaInfoFn
	mediaInfoFn = func(string) scan.MediaInfo {
		return scan.MediaInfo{Camera: "Cam", ISO: "200"}
	}
	defer func() { mediaInfoFn = orig }()

	// fixture registers no /api/info route, so the handler is driven directly.
	s, _, cleanup := fixture(t, 1)
	defer cleanup()

	photoPath := filepath.Join(s.libraryRoot, "p.jpg")
	jpegBytes := makeDisplayJPEG(t, 64, 48)
	if err := os.WriteFile(photoPath, jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(s.libraryRoot, "v.mp4")
	if err := os.WriteFile(videoPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.index.ReconcileBatch([]scan.Result{
		{Path: photoPath, Type: scan.TypePhoto, Size: int64(len(jpegBytes)), ModTime: now},
		{Path: videoPath, Type: scan.TypeVideo, Size: 1, ModTime: now},
	})

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleAPIInfo(rec, httptest.NewRequest(http.MethodGet, "/api/info"+query, nil))
		return rec
	}

	if rec := get(""); rec.Code != http.StatusBadRequest {
		t.Errorf("no id = %d, want 400", rec.Code)
	}
	if rec := get("?id=xyz"); rec.Code != http.StatusBadRequest {
		t.Errorf("id=xyz = %d, want 400", rec.Code)
	}
	if rec := get("?id=" + strings.Repeat("a", 40)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown well-formed id = %d, want 404", rec.Code)
	}

	photo := func() infoJSON {
		t.Helper()
		rec := get("?id=" + url.QueryEscape(cache.ThumbIDFor(photoPath)))
		if rec.Code != http.StatusOK {
			t.Fatalf("photo info status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("photo info content-type = %q, want application/json", ct)
		}
		var info infoJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatalf("decode photo info: %v", err)
		}
		return info
	}()

	for _, tc := range []struct{ field, got, want string }{
		{"Name", photo.Name, "p.jpg"},
		{"Path", photo.Path, "p.jpg"},
		{"Type", photo.Type, "Photo"},
		{"Dimensions", photo.Dimensions, "64 × 48"},
		{"Camera", photo.Camera, "Cam"},
		{"ISO", photo.ISO, "200"},
		{"Lens", photo.Lens, "—"},
	} {
		if tc.got != tc.want {
			t.Errorf("photo info %s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}

	rec := get("?id=" + url.QueryEscape(cache.ThumbIDFor(videoPath)))
	if rec.Code != http.StatusOK {
		t.Fatalf("video info status = %d, want 200", rec.Code)
	}
	var video infoJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &video); err != nil {
		t.Fatalf("decode video info: %v", err)
	}
	if video.Type != "Video" {
		t.Errorf("video info Type = %q, want Video", video.Type)
	}
	if video.Dimensions != "—" {
		t.Errorf("video info Dimensions = %q, want %q (video dimensions are not decoded)", video.Dimensions, "—")
	}
}
