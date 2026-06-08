package webserver

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// HLS on-the-fly transcoding.
//
// iOS/Safari plays HLS (an .m3u8 playlist of short .ts segments) natively in
// a <video> tag, which is the one streaming format Safari understands without
// a JS shim. The GUI plays mkv/avi/webm/HEVC via mpv, but Safari can't decode
// those — so for the web viewer we transcode such files to H.264/AAC and
// hand iOS an HLS stream instead.
//
// The playlist is synthesised up front from the file's duration: fixed-length
// segments, each an independent ffmpeg transcode keyed by start offset. That
// makes playback start almost immediately (only the first segment has to be
// produced) and makes seeking cheap (the player just requests the segment it
// lands in). Segments are cached on disk so a re-watch or seek-back is free.
const (
	hlsSegDur  = 6.0  // seconds per segment; also the playlist TARGETDURATION
	hlsMaxW    = 1280 // cap width so per-segment transcode keeps up on the fly
	hlsSegMime = "video/mp2t"
	hlsM3UMime = "application/vnd.apple.mpegurl"
)

// handleHLS routes /hls/<id>/index.m3u8 (playlist) and /hls/<id>/seg<k>.ts
// (segments). Both resolve <id> to a video entry via the shared lookup so
// trash items work the same as indexed ones.
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/hls/")
	id, file, ok := strings.Cut(rest, "/")
	if !ok || !validID(id) {
		http.NotFound(w, r)
		return
	}
	e, ok := s.lookupEntry(id)
	if !ok || e.Type != scan.TypeVideo {
		http.NotFound(w, r)
		return
	}

	switch {
	case file == "index.m3u8":
		s.serveHLSPlaylist(w, r, id, e)
	case strings.HasPrefix(file, "seg") && strings.HasSuffix(file, ".ts"):
		k, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(file, "seg"), ".ts"))
		if err != nil || k < 0 {
			http.NotFound(w, r)
			return
		}
		s.serveHLSSegment(w, r, id, e, k)
	default:
		http.NotFound(w, r)
	}
}

// serveHLSPlaylist probes the source duration and emits a VOD playlist of
// fixed-length segments. Cheap enough to do per request (one ffprobe call),
// so the duration isn't cached.
func (s *Server) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, id string, e cache.Entry) {
	dur, err := probeDuration(e.Path)
	if err != nil || dur <= 0 {
		http.Error(w, "cannot probe video duration", http.StatusInternalServerError)
		return
	}
	n := int(math.Ceil(dur / hlsSegDur))

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(hlsSegDur)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < n; i++ {
		segLen := hlsSegDur
		if rem := dur - float64(i)*hlsSegDur; rem < segLen {
			segLen = rem
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.ts\n", segLen, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", hlsM3UMime)
	w.Header().Set("Cache-Control", "private, max-age=60")
	_, _ = w.Write([]byte(b.String()))
}

// serveHLSSegment serves segment k, transcoding (and caching) it on first
// request. Concurrent requests for the same segment share one transcode.
func (s *Server) serveHLSSegment(w http.ResponseWriter, r *http.Request, id string, e cache.Entry, k int) {
	dst := s.hlsSegPath(id, k)

	// Fresh cache hit: serve it. A segment is stale if the source changed
	// since it was written (same rule the thumb store uses).
	if info, err := os.Stat(dst); err == nil {
		if !info.ModTime().Before(e.ModTime) {
			s.serveSegmentFile(w, r, dst)
			return
		}
		os.Remove(dst)
	}

	if err := s.transcodeSegment(id, e, k, dst); err != nil {
		http.Error(w, "segment transcode failed", http.StatusInternalServerError)
		return
	}
	s.serveSegmentFile(w, r, dst)
}

func (s *Server) serveSegmentFile(w http.ResponseWriter, r *http.Request, dst string) {
	w.Header().Set("Content-Type", hlsSegMime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, dst)
}

// transcodeSegment produces one mpegts segment via ffmpeg, deduping
// concurrent identical requests through a per-path singleflight and bounding
// total parallel transcodes with a semaphore. Writes to a temp file and
// atomically renames, so a killed transcode never leaves a half-written
// segment in the cache.
func (s *Server) transcodeSegment(id string, e cache.Entry, k int, dst string) error {
	s.hlsInit()

	// Singleflight on the destination path.
	s.hlsFlightMu.Lock()
	if ch, ok := s.hlsInflight[dst]; ok {
		s.hlsFlightMu.Unlock()
		<-ch
		if info, err := os.Stat(dst); err == nil && !info.ModTime().Before(e.ModTime) {
			return nil
		}
		return fmt.Errorf("hls segment generation failed for %s", e.Path)
	}
	ch := make(chan struct{})
	s.hlsInflight[dst] = ch
	s.hlsFlightMu.Unlock()
	defer func() {
		s.hlsFlightMu.Lock()
		delete(s.hlsInflight, dst)
		close(ch)
		s.hlsFlightMu.Unlock()
	}()

	// Another goroutine may have finished it while we waited for the lock.
	if info, err := os.Stat(dst); err == nil && !info.ModTime().Before(e.ModTime) {
		return nil
	}

	s.hlsSem <- struct{}{}
	defer func() { <-s.hlsSem }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not installed")
	}

	start := float64(k) * hlsSegDur
	tmp := dst + ".tmp"
	// -ss before -i is a fast keyframe seek; -output_ts_offset re-anchors the
	// segment's timestamps to its real position so the player stitches
	// segments together without gaps. -force_key_frames guarantees the
	// segment opens on an IDR frame, making it independently decodable.
	args := []string{
		"-nostdin", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		"-t", strconv.FormatFloat(hlsSegDur, 'f', 3, 64),
		"-i", e.Path,
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", hlsMaxW),
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high",
		"-level", "4.0", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-b:a", "128k",
		"-force_key_frames", "expr:gte(t,0)",
		"-output_ts_offset", strconv.FormatFloat(start, 'f', 3, 64),
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts", tmp,
	}
	if err := exec.Command("ffmpeg", args...).Run(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// hlsSegPath maps (id, k) to <cache>/hls/<aa>/<id>/seg<k>.ts, sharded by the
// first two hex chars of the id the same way the thumb store shards.
func (s *Server) hlsSegPath(id string, k int) string {
	return filepath.Join(s.store.CacheDir(), "hls", id[:2], id, fmt.Sprintf("seg%d.ts", k))
}

// hlsInit lazily sets up the segment semaphore and singleflight map.
func (s *Server) hlsInit() {
	s.hlsOnce.Do(func() {
		n := runtime.NumCPU() / 2
		if n < 1 {
			n = 1
		}
		s.hlsSem = make(chan struct{}, n)
		s.hlsInflight = make(map[string]chan struct{})
	})
}

// probeDuration returns the container duration in seconds via ffprobe.
func probeDuration(path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, fmt.Errorf("ffprobe not installed")
	}
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1",
		path,
	).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}
