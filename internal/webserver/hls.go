package webserver

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

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

	// hlsSegTimeout caps one on-the-fly segment transcode and hlsProbeTimeout
	// caps the playlist duration probe. Both guard the same wedge: ffmpeg /
	// ffprobe holds one of only NumCPU/2 hlsSem slots, so a handful of hung
	// children would starve HLS entirely until restart. The per-op deadlines
	// are further derived from r.Context() (see below) so a client disconnect
	// — e.g. a seek-happy Safari session that has already moved on — cancels
	// the transcode instead of leaving minutes of dead work queued ahead of
	// the segment actually wanted.
	hlsSegTimeout   = 2 * time.Minute
	hlsProbeTimeout = 15 * time.Second

	// HLS cache lifecycle. Every watched non-Safari video is transcoded into
	// segments cached under <cache>/hls/ and, left alone, that tree grows
	// without bound on the media drive. hlsCacheMaxBytes caps the total; the
	// sweep evicts oldest-first back under the cap and also clears
	// crash-orphaned .tmp files older than hlsTmpMaxAge. hlsSweepInterval
	// throttles how often an active session re-sweeps (the sweep is tied to
	// segment-serving activity, so an idle server spawns no timers).
	hlsCacheMaxBytes = 4 << 30 // 4 GiB
	hlsTmpMaxAge     = time.Hour
	hlsSweepInterval = 5 * time.Minute
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

// serveHLSPlaylist emits a VOD playlist of fixed-length segments. The source
// duration comes from the index row the caller already fetched (via
// hlsDuration), so the common path costs no ffprobe fork — only videos indexed
// without a duration fall back to probing.
func (s *Server) serveHLSPlaylist(w http.ResponseWriter, r *http.Request, _ string, e cache.Entry) {
	dur := s.hlsDuration(r.Context(), e)
	if dur <= 0 {
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
	for i := range n {
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
	// Reject segment indices past the end of the video. The playlist only ever
	// references seg0…seg(n-1); an out-of-range k (a stale player, a manual
	// probe) would otherwise spawn an ffmpeg seek past EOF and cache a junk
	// segment. Bound only when the duration is known for free (index row) — a
	// duration-less entry skips the check rather than paying a probe fork here.
	if e.DurationMs > 0 && k >= int(math.Ceil(float64(e.DurationMs)/1000.0/hlsSegDur)) {
		http.NotFound(w, r)
		return
	}

	dst := s.hlsSegPath(id, k)

	// Fresh cache hit: serve it. A segment is stale if the source changed
	// since it was written (same rule the thumb store uses).
	if info, err := os.Stat(dst); err == nil {
		if !info.ModTime().Before(e.ModTime) {
			s.serveSegmentFile(w, r, dst)
			return
		}
		if rerr := os.Remove(dst); rerr != nil {
			log.Printf("webserver: HLS: remove stale segment %s: %v", dst, rerr)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("webserver: HLS: stat segment %s: %v", dst, err)
	}

	if err := s.transcodeSegment(r.Context(), id, e, k, dst); err != nil {
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
func (s *Server) transcodeSegment(ctx context.Context, _ string, e cache.Entry, k int, dst string) error {
	s.hlsInit()

	// Singleflight on the destination path.
	s.hlsFlightMu.Lock()
	if ch, ok := s.hlsInflight[dst]; ok {
		s.hlsFlightMu.Unlock()
		// Wait for the leader, but not on a dead transcode: if *this* client
		// has disconnected (Safari seeked away), bail immediately rather than
		// block on a leader that may itself be a minutes-long transcode.
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
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

	// Wait for a transcode slot, but abandon the wait if the client has already
	// disconnected. An unconditional send would keep a dead request queued for
	// one of only NumCPU/2 slots (each held by up to a hlsSegTimeout-long
	// transcode) while it still holds singleflight leadership for dst — stalling
	// live followers of the same segment behind work no one is waiting for. Only
	// the success case takes a slot, so the release defer is set up after it and
	// never fires on the cancel path (we don't return a slot we never took). The
	// leadership defer installed above still runs on this early return, so the
	// inflight entry is deleted and followers are released to retry.
	select {
	case s.hlsSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
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
	// Deadline derived from the client's request ctx: a client disconnect
	// cancels ffmpeg (no more dead transcodes for a browser that's gone), and
	// hlsSegTimeout caps a single transcode so a stuck ffmpeg can't hold its
	// hlsSem slot indefinitely. Capture stderr so a failure surfaces the real
	// ffmpeg diagnostic in the logs instead of an opaque generic 500.
	tctx, cancel := context.WithTimeout(ctx, hlsSegTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(tctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		log.Printf("hls: transcode %s seg%d failed: %v: %s",
			e.Path, k, err, strings.TrimSpace(stderr.String()))
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	// Each new segment grows the cache; nudge the throttled sweeper so a long
	// viewing session keeps the tree under hlsCacheMaxBytes.
	s.maybeSweepHLS()
	return nil
}

// hlsSegPath maps (id, k) to <cache>/hls/<aa>/<id>/seg<k>.ts, sharded by the
// first two hex chars of the id the same way the thumb store shards.
func (s *Server) hlsSegPath(id string, k int) string {
	return filepath.Join(s.store.CacheDir(), "hls", id[:2], id, fmt.Sprintf("seg%d.ts", k))
}

// hlsInit lazily sets up the segment semaphore and singleflight map, and kicks
// a one-shot startup sweep to reclaim any .tmp files a previous run's killed
// transcodes orphaned and to enforce the size cap from segments cached earlier.
func (s *Server) hlsInit() {
	s.hlsOnce.Do(func() {
		n := max(runtime.NumCPU()/2, 1)
		s.hlsSem = make(chan struct{}, n)
		s.hlsInflight = make(map[string]chan struct{})
		s.hlsSweepMu.Lock()
		s.hlsSweepAt = time.Now()
		s.hlsSweepMu.Unlock()
		go s.sweepHLS()
	})
}

// probeDurationFn is the ffprobe indirection hlsDuration uses. It's a package
// var only so tests can substitute a deterministic stub without an ffprobe
// binary and count how often the probe actually forks; production never
// reassigns it.
var probeDurationFn = probeDuration

// hlsDuration returns the source video's duration in seconds. It prefers the
// DurationMs the scanner already recorded on the index row (no fork) and only
// falls back to an ffprobe for entries indexed without one (e.g. via a walk
// that skipped the duration probe). Returns 0 when the duration can't be found.
//
// On a successful fallback probe of an *indexed* entry it persists the result
// via Index.SetDurationMs, so subsequent playlist and segment requests for the
// same file read the duration off the row instead of re-forking ffprobe (and so
// serveHLSSegment can bound out-of-range segments, which it skips while the
// duration is unknown). Trash entries have no index row and stay probe-only, so
// they're excluded from the write. The persistence is a pure side effect: the
// return value is identical to what a probe-only implementation would yield.
func (s *Server) hlsDuration(ctx context.Context, e cache.Entry) float64 {
	if e.DurationMs > 0 {
		return float64(e.DurationMs) / 1000.0
	}
	dur, err := probeDurationFn(ctx, e.Path)
	if err != nil {
		return 0
	}
	// Persist only a real, positive duration for an indexed (non-trash) entry.
	// A non-positive probe result is left unpersisted (SetDurationMs would
	// ignore it anyway) so the "unknown" sentinel is never cemented.
	if dur > 0 && !s.isTrashPath(e.Path) {
		if perr := s.index.SetDurationMs(e.Path, int64(math.Round(dur*1000))); perr != nil {
			log.Printf("webserver: HLS: persist probed duration for %s: %v", e.Path, perr)
		}
	}
	return dur
}

// isTrashPath reports whether path lives inside the server's trash directory.
// Trash entries are read straight from that directory and have no index row, so
// their probed durations must not be persisted — there's nothing to UPDATE.
func (s *Server) isTrashPath(path string) bool {
	if s.trashDir == "" {
		return false
	}
	return path == s.trashDir || strings.HasPrefix(path, s.trashDir+string(filepath.Separator))
}

// maybeSweepHLS launches an asynchronous HLS cache sweep at most once per
// hlsSweepInterval. It's called after each cached segment so the tree stays
// bounded during an active viewing session; gating on the last sweep time
// keeps the walk cost off the hot path and means an idle server sweeps only
// the once at init (no background timer to leak on Stop).
func (s *Server) maybeSweepHLS() {
	s.hlsSweepMu.Lock()
	// hlsInit always stamps hlsSweepAt before any caller can reach here, so the
	// zero value never survives to gate the first real sweep.
	if time.Since(s.hlsSweepAt) < hlsSweepInterval {
		s.hlsSweepMu.Unlock()
		return
	}
	s.hlsSweepAt = time.Now()
	s.hlsSweepMu.Unlock()
	go s.sweepHLS()
}

// sweepHLS reclaims the HLS segment cache under <cache>/hls. See sweepHLSDir
// for the mechanics; this just binds the live cache root, the configured cap,
// and the current time.
func (s *Server) sweepHLS() {
	sweepHLSDir(filepath.Join(s.store.CacheDir(), "hls"), hlsCacheMaxBytes, hlsTmpMaxAge, time.Now())
}

// sweepHLSDir walks root, deleting crash-orphaned .tmp files older than
// tmpMaxAge and, when the remaining segments exceed maxBytes, evicting whole
// segments oldest-first until back under the cap. Best-effort throughout:
// races with concurrent transcodes just skip the affected file on the next
// sweep. Split from sweepHLS (and given explicit cap/age/now parameters) so
// it's testable without a multi-GB fixture.
func sweepHLSDir(root string, maxBytes int64, tmpMaxAge time.Duration, now time.Time) {
	type segFile struct {
		path string
		size int64
		mod  time.Time
	}
	var segs []segFile
	var total int64

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable / vanished dir: skip, don't abort the walk
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			if now.Sub(info.ModTime()) > tmpMaxAge {
				os.Remove(path)
			}
			return nil
		}
		segs = append(segs, segFile{path: path, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
		return nil
	})

	if total <= maxBytes {
		return
	}
	// Oldest-first: a still-playing session's fresh segments outlive the stale
	// ones from earlier videos. Empty <id>/<shard> directories are intentionally
	// left in place — pruning them would race a concurrent transcode that has
	// just MkdirAll'd its segment dir but not yet written the file, and an empty
	// dir costs one inode (the whole tree is dropped on Rebuild regardless).
	sort.Slice(segs, func(i, j int) bool { return segs[i].mod.Before(segs[j].mod) })
	for _, f := range segs {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}

// probeDuration returns the container duration in seconds via ffprobe. The
// probe is bounded by hlsProbeTimeout (and cancelled if the requesting client
// disconnects) so a wedged ffprobe can't hang the playlist request or hold a
// worker; on failure the caller returns a 500, exactly as before.
func probeDuration(ctx context.Context, path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, fmt.Errorf("ffprobe not installed")
	}
	pctx, cancel := context.WithTimeout(ctx, hlsProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(pctx, "ffprobe",
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
