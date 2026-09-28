package webserver

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
)

// handleThumb serves the cached thumbnail JPEG for /thumb/<id>.
// Generates on demand via ThumbStore.Path so unseen thumbs are created
// the same way the GUI would.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	// Resolve the entry before the conditional check so the ETag can fold in
	// the source mtime — cheap now that GetEntryByThumbID is index-backed
	// (I-08). The old ETag was the bare thumb id (== sha1 of the path), so it
	// never changed when a file was edited in place and remote browsers 304'd
	// on the stale thumb forever. Keying on mtime as well means an edit yields
	// a fresh ETag and the next revalidation fetches the regenerated thumb.
	e, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, id, e.ModTime.Unix())
	// Cacheable for a day but no longer "immutable": immutable told browsers to
	// skip revalidation entirely within max-age, which — combined with the
	// static ETag — is what pinned stale thumbs. Dropping it lets a reload
	// revalidate against the mtime-keyed ETag and pick up edits.
	const thumbCacheControl = "public, max-age=86400"
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", thumbCacheControl)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	thumbPath, err := s.store.Path(e)
	if err != nil {
		http.Error(w, "thumbnail unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", thumbCacheControl)
	w.Header().Set("ETag", etag)
	http.ServeFile(w, r, thumbPath)
}

// handleMedia serves the original media file for /media/<id>. Uses
// http.ServeFile so range requests are honored (important for video).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/media/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	e, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ct := mimeFor(e); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	// Make browser save with the original filename instead of the opaque id.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(e.Path)}))
	http.ServeFile(w, r, e.Path)
}

// displayPassThroughExts are the extensions a mainstream browser renders
// natively in an <img>. A small original in one of these formats is served
// as-is by /display; anything else (RAW/HEIC/TIFF) — and any oversized member
// of this set — gets a JPEG rendition instead.
var displayPassThroughExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

// displayPassThroughMaxBytes is the size ceiling under which a natively-
// renderable original is passed through untouched. Above it, even a plain
// JPEG is downscaled to a ~2048 px rendition — a phone screen can't tell the
// difference, and a multi-MB original is pure wasted bandwidth. 3 MiB keeps
// ordinary web-sized photos direct while catching full-resolution camera
// JPEGs.
const displayPassThroughMaxBytes = 3 << 20

// displayCacheControl matches handleThumb's: cacheable for a day but revalidated
// against the mtime-keyed ETag, so an in-place source edit is picked up.
const displayCacheControl = "private, max-age=86400"

// etagMatches reports whether an If-None-Match header value matches etag,
// per RFC 7232 §3.2: a comma-separated list of entity tags compared weakly
// (a W/ prefix is ignored), or "*" for any current representation.
func etagMatches(ifNoneMatch, etag string) bool {
	etag = strings.TrimPrefix(etag, "W/")
	for tag := range strings.SplitSeq(ifNoneMatch, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}

// handleDisplay serves a browser-renderable image for /display/<id>. For a
// small original in a format browsers render natively (JPEG/PNG/WebP/GIF) it
// passes the bytes straight through; otherwise — RAW, HEIC, TIFF, or an
// oversized JPEG — it serves a cached ~2048 px JPEG rendition produced by the
// display store (which reuses the thumb pipeline at DisplaySize). The viewer's
// <img src> points here so RAW/HEIC render at all and plain photos ship a
// downscaled copy instead of the full-resolution original (W-03); /media stays
// the download-original / video route. The ETag folds in the source mtime
// exactly like handleThumb, so editing the file in place invalidates any
// cached response.
func (s *Server) handleDisplay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/display/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	e, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, id, e.ModTime.Unix())
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", displayCacheControl)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Pass through a small, natively-renderable original untouched — a browser
	// renders it directly, so a rendition would only add a decode/encode round
	// trip for no visual gain. Also the fallback when no display store is wired
	// (a RAW would still be un-renderable, but there's nothing better to serve).
	ext := strings.ToLower(filepath.Ext(e.Path))
	if s.display == nil || (displayPassThroughExts[ext] && e.Size < displayPassThroughMaxBytes) {
		if ct := mimeFor(e); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", displayCacheControl)
		w.Header().Set("ETag", etag)
		http.ServeFile(w, r, e.Path)
		return
	}

	renditionPath, err := s.display.Path(e)
	if err != nil {
		http.Error(w, "display rendition unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", displayCacheControl)
	w.Header().Set("ETag", etag)
	http.ServeFile(w, r, renditionPath)
}

// needsHLS reports whether a video's container should be served via the
// on-the-fly HLS transcode rather than streamed directly. mp4/m4v/mov are
// the containers Safari/iOS decode natively, so they go direct; everything
// else the scanner recognises as video (mkv, webm, avi, mts, m2ts) is
// undecodable in a <video> tag and must be transcoded. HEVC inside an mp4
// is the one case this misses — if that surfaces, add a codec probe here.
func needsHLS(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		return false
	default:
		return true
	}
}

// mimeFor returns a content type for common extensions. The Go stdlib's
// http.ServeFile sniffs content from the first 512 bytes if we leave the
// header unset, but that misclassifies HEIC and CR2 — set explicitly when
// we can so browsers don't try to render RAW as plain text.
func mimeFor(e cache.Entry) string {
	ext := strings.ToLower(filepath.Ext(e.Path))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".heic", ".heif":
		return "image/heic"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	}
	return ""
}
