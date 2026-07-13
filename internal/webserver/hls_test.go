package webserver

import (
	"context"
	"errors"
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

	s := New(idx, store, nil, libRoot)
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

// indexedVideoServer wires a Server around an index holding one video entry
// with the given duration, without touching ffmpeg. The source file is created
// empty just so lookups resolve; tests here exercise paths that never decode
// it (playlist synthesis from the indexed duration, out-of-range rejection).
func indexedVideoServer(t *testing.T, durationMs int64) (*Server, string, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "pv-hls-idx-")
	if err != nil {
		t.Fatal(err)
	}
	libRoot := filepath.Join(tmp, "lib")
	os.MkdirAll(libRoot, 0o755)
	vid := filepath.Join(libRoot, "clip.mkv")
	os.WriteFile(vid, []byte("x"), 0o644)
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
		Path:       vid,
		Type:       scan.TypeVideo,
		Size:       info.Size(),
		ModTime:    info.ModTime(),
		DurationMs: durationMs,
	}})
	s := New(idx, store, nil, libRoot)
	return s, cache.ThumbIDFor(vid), func() {
		idx.Close()
		os.RemoveAll(tmp)
	}
}

func TestHLSPlaylistUsesIndexDuration(t *testing.T) {
	// 14s indexed → 3 segments, synthesised without any ffprobe fork (ffmpeg
	// isn't even required for this path).
	s, id, cleanup := indexedVideoServer(t, 14000)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(s.handleHLS))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/hls/" + id + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("playlist status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	pl := string(body)
	if segs := strings.Count(pl, ".ts"); segs != 3 {
		t.Errorf("segment count = %d, want 3 (14s/6s):\n%s", segs, pl)
	}
	if !strings.Contains(pl, "seg2.ts") || strings.Contains(pl, "seg3.ts") {
		t.Errorf("expected seg0..seg2 only:\n%s", pl)
	}
}

func TestHLSSegmentRejectsOutOfRange(t *testing.T) {
	// 8s indexed → ceil(8/6) = 2 segments (seg0, seg1). seg2+ is past EOF and
	// must 404 rather than spawn an ffmpeg seek past the end and cache junk.
	s, id, cleanup := indexedVideoServer(t, 8000)
	defer cleanup()
	ts := httptest.NewServer(http.HandlerFunc(s.handleHLS))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/hls/" + id + "/seg5.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("out-of-range segment status = %d, want 404", resp.StatusCode)
	}
}

func TestHLSSegmentHonorsCancellation(t *testing.T) {
	// A request whose client has already disconnected must not block waiting for
	// a transcode slot when the semaphore is saturated (each held slot is up to
	// a hlsSegTimeout-long transcode). It should return context.Canceled
	// promptly, without taking/releasing a slot it never acquired, and must
	// release its singleflight leadership so a later live request can retake it.
	// This exercises the pre-ffmpeg cancel path, so no ffmpeg fixture is needed.
	s, id, cleanup := indexedVideoServer(t, 60000)
	defer cleanup()

	e, ok := s.lookupEntry(id)
	if !ok {
		t.Fatal("video entry not found")
	}
	dst := s.hlsSegPath(id, 0)

	// Initialise the semaphore/singleflight, then occupy every slot so a real
	// transcode could not proceed.
	s.hlsInit()
	for range cap(s.hlsSem) {
		s.hlsSem <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone

	done := make(chan error, 1)
	go func() { done <- s.transcodeSegment(ctx, id, e, 0, dst) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("transcodeSegment err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transcodeSegment blocked on a saturated semaphore despite a cancelled context")
	}

	// The cancelled request must not have consumed (or released) a slot.
	if got := len(s.hlsSem); got != cap(s.hlsSem) {
		t.Errorf("semaphore occupancy = %d, want %d (cancel path must not take or release a slot)", got, cap(s.hlsSem))
	}
	// Leadership for dst must have been released via the deferred delete.
	s.hlsFlightMu.Lock()
	_, stillLeader := s.hlsInflight[dst]
	s.hlsFlightMu.Unlock()
	if stillLeader {
		t.Error("singleflight leadership not released on the cancel path")
	}
}

// TestHLSDurationPersistsProbe is the W-06 write-back guard. A duration-less
// indexed entry falls back to ffprobe on its first playlist/segment request;
// the probed result is persisted to the row so later requests read it off the
// index instead of re-forking ffprobe. The probe is stubbed (no ffprobe binary
// needed) and its forks counted.
func TestHLSDurationPersistsProbe(t *testing.T) {
	s, id, cleanup := indexedVideoServer(t, 0) // duration-less on purpose
	defer cleanup()

	e, ok := s.lookupEntry(id)
	if !ok {
		t.Fatal("video entry not found")
	}
	if e.DurationMs != 0 {
		t.Fatalf("fixture DurationMs = %d, want 0", e.DurationMs)
	}

	var probes int
	orig := probeDurationFn
	probeDurationFn = func(_ context.Context, _ string) (float64, error) {
		probes++
		return 12.0, nil
	}
	defer func() { probeDurationFn = orig }()

	// First call: falls back to the probe and persists the 12s result.
	if got := s.hlsDuration(context.Background(), e); got != 12.0 {
		t.Fatalf("first hlsDuration = %v, want 12", got)
	}
	if probes != 1 {
		t.Fatalf("probe forks after first call = %d, want 1", probes)
	}

	// The index row now carries the probed duration (12s -> 12000ms).
	persisted, ok := s.lookupEntry(id)
	if !ok {
		t.Fatal("entry vanished from index")
	}
	if persisted.DurationMs != 12000 {
		t.Errorf("persisted DurationMs = %d, want 12000", persisted.DurationMs)
	}

	// Second call with the re-fetched entry: reads the fresh row (DurationMs >
	// 0) and must NOT re-fork the probe — the whole point of W-06.
	if got := s.hlsDuration(context.Background(), persisted); got != 12.0 {
		t.Fatalf("second hlsDuration = %v, want 12", got)
	}
	if probes != 1 {
		t.Errorf("probe forks after second call = %d, want 1 (index read must not re-fork)", probes)
	}
}

// TestHLSDurationSkipsPersistForTrash guards the trash case: a trashed video
// has no index row, so a probed duration must not be persisted (there's nothing
// to UPDATE) and every request stays probe-only.
func TestHLSDurationSkipsPersistForTrash(t *testing.T) {
	s, _, cleanup := indexedVideoServer(t, 0)
	defer cleanup()
	if s.trashDir == "" {
		t.Fatal("test server has no trash dir")
	}

	// A synthetic trash-dir entry, shaped like ListTrash's output: a path under
	// the trash dir with no matching index row and no recorded duration.
	trashEntry := cache.Entry{
		Path:       filepath.Join(s.trashDir, "20260101T000000-deadbeef-clip.mkv"),
		Type:       scan.TypeVideo,
		DurationMs: 0,
	}

	var probes int
	orig := probeDurationFn
	probeDurationFn = func(_ context.Context, _ string) (float64, error) {
		probes++
		return 9.0, nil
	}
	defer func() { probeDurationFn = orig }()

	if got := s.hlsDuration(context.Background(), trashEntry); got != 9.0 {
		t.Fatalf("first hlsDuration = %v, want 9", got)
	}
	// Nothing must have been written to the index for the trash path.
	if _, ok := s.index.GetEntryByThumbID(cache.ThumbIDFor(trashEntry.Path)); ok {
		t.Error("trash entry unexpectedly written to the index")
	}
	// With nothing persisted to short-circuit it, a second call must re-probe.
	if got := s.hlsDuration(context.Background(), trashEntry); got != 9.0 {
		t.Fatalf("second hlsDuration = %v, want 9", got)
	}
	if probes != 2 {
		t.Errorf("probe forks = %d, want 2 (trash entry must stay probe-only)", probes)
	}
}

func TestSweepHLSDir(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Helper to place a file with a specific size and mtime under root.
	place := func(rel string, size int, age time.Duration) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		os.Chtimes(p, mt, mt)
		return p
	}

	oldTmp := place("aa/aaid/seg9.ts.tmp", 10, 2*time.Hour)   // crash-orphaned, stale
	freshTmp := place("aa/aaid/seg8.ts.tmp", 10, time.Minute) // in-flight, keep
	oldSeg := place("aa/aaid/seg0.ts", 1000, 3*time.Hour)     // evict first
	midSeg := place("bb/bbid/seg0.ts", 1000, 2*time.Hour)     // evict second
	newSeg := place("cc/ccid/seg0.ts", 1000, time.Minute)     // keep (newest)

	// Cap at 1500 bytes: total seg bytes = 3000, so two oldest must go.
	sweepHLSDir(root, 1500, time.Hour, now)

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(oldTmp) {
		t.Errorf("stale .tmp not swept")
	}
	if !exists(freshTmp) {
		t.Errorf("fresh .tmp wrongly swept")
	}
	if exists(oldSeg) || exists(midSeg) {
		t.Errorf("oldest segments not evicted under cap")
	}
	if !exists(newSeg) {
		t.Errorf("newest segment wrongly evicted")
	}
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
