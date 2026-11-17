package webserver

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// Decoders for decodeDimensions (image.DecodeConfig).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// cellJSON is the wire shape returned by /api/page. Kept compact since
// 100k-photo libraries will fetch this hundreds of times per session.
type cellJSON struct {
	ID       string `json:"id"`       // ThumbID — drives /thumb/<id> and /view/<id>
	Name     string `json:"name"`     // basename for the caption + img alt
	Video    bool   `json:"video"`    // adds the "video" badge in the corner
	Favorite bool   `json:"favorite"` // toggles the star badge
}

// handleAPIPage serves the next page of cells as JSON for the
// infinite-scroll fetcher. Query:
// ?from=...&y=...&path=...&p=N&after=<id>
func (s *Server) handleAPIPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vi, ok := s.viewFromQuery(q)
	if !ok {
		http.Error(w, "invalid view", http.StatusBadRequest)
		return
	}
	page := pageNumber(q.Get("p"))

	var entries []cache.Entry
	var hasNext bool
	if vi.kind == "trash" {
		entries, hasNext = pageSlice(s.trashEntries(), page)
	} else {
		// keyset pagination (W4-05) — grid.js sends the last rendered cell's
		// id, which the index resolves to its path, so a deep scroll seeks
		// instead of walking OFFSET rows; p= stays as the fallback (noscript
		// pager, a cursor whose entry has since vanished, and trash, which is
		// an in-memory slice).
		keyset := false
		var raw []cache.Entry
		if after := q.Get("after"); validID(after) {
			if e, ok := s.index.GetEntryByThumbID(after); ok {
				raw = s.index.ListPageAfter(vi.v, e.Path, pageSize+1)
				keyset = true
			}
		}
		if !keyset {
			offset := (page - 1) * pageSize
			// Fetch one extra entry to detect the next page without a CountView scan.
			raw = s.index.ListPage(vi.v, offset, pageSize+1)
		}
		hasNext = len(raw) > pageSize
		if hasNext {
			entries = raw[:pageSize]
		} else {
			entries = raw
		}
	}

	items := make([]cellJSON, 0, len(entries))
	for _, e := range entries {
		items = append(items, cellJSON{
			ID:       e.ThumbID,
			Name:     filepath.Base(e.Path),
			Video:    e.Type == scan.TypeVideo,
			Favorite: e.Favorite,
		})
	}
	resp := struct {
		Items   []cellJSON `json:"items"`
		HasNext bool       `json:"hasNext"`
	}{
		Items:   items,
		HasNext: hasNext,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// favoriteRequest is the body shape for POST /api/favorite. on sets the
// resulting state; omitted, the flag is toggled. Older clients also send
// toggle:true, which the decoder ignores.
type favoriteRequest struct {
	ID string `json:"id"`
	On *bool  `json:"on,omitempty"`
}

// favoriteMaxBody caps the POST /api/favorite body. A real request is under
// 100 bytes; the cap stops a client from making the JSON decoder buffer an
// arbitrarily large body (e.g. one giant string field) into memory.
const favoriteMaxBody = 4 << 10

// handleAPIFavorite toggles or sets the favorite flag for a given thumb id.
// Returns the new state so the client can update the UI deterministically.
func (s *Server) handleAPIFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	// Browsers append Origin to every non-GET/HEAD request, including
	// same-origin POSTs (per the Fetch standard). Rejecting any non-empty
	// Origin would 403 every real-browser favorite toggle, so treat an
	// Origin whose host matches the page host (r.Host) as same-origin and
	// only reject a genuine cross-origin one.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	secFetch := r.Header.Get("Sec-Fetch-Site")
	if secFetch != "" && secFetch != "same-origin" && secFetch != "none" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req favoriteRequest
	r.Body = http.MaxBytesReader(w, r.Body, favoriteMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	e, ok := s.index.GetEntryByThumbID(req.ID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	want := !e.Favorite
	if req.On != nil {
		want = *req.On
	}
	if err := s.index.SetFavorite(e.Path, want); err != nil {
		http.Error(w, "set favorite failed", http.StatusInternalServerError)
		return
	}
	resp := struct {
		ID       string `json:"id"`
		Favorite bool   `json:"favorite"`
	}{ID: req.ID, Favorite: want}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// infoJSON is the wire shape returned by /api/info. Lazily fetched per
// /view/<id> request so opening the viewer doesn't pay the exiftool cost
// up front.
type infoJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	Size         string `json:"size"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	Dimensions   string `json:"dimensions"`
	Camera       string `json:"camera"`
	Lens         string `json:"lens"`
	Aperture     string `json:"aperture"`
	ShutterSpeed string `json:"shutter_speed"`
	ISO          string `json:"iso"`
	FocalLength  string `json:"focal_length"`
	Favorite     bool   `json:"favorite"`
}

// handleAPIInfo returns extended metadata for one entry. The viewer's info
// panel calls this on demand so we don't pay the exiftool cost up front.
func (s *Server) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	e, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	key := infoKey{id: e.ThumbID, mtime: e.ModTime.UnixNano(), size: e.Size}
	cached, ok := s.infoCache.get(key)
	if !ok {
		// Dimensions are best-effort: a fast image.DecodeConfig works on
		// regular photos; RAW/HEIC/video would need exiftool again and
		// aren't worth the extra fork for an info-panel value.
		cached = infoVal{mi: mediaInfoFn(e.Path), dims: decodeDimensions(e)}
		s.infoCache.put(key, cached)
	}
	mi := cached.mi

	info := infoJSON{
		ID:           e.ThumbID,
		Name:         filepath.Base(e.Path),
		Path:         s.relPath(e.Path),
		Type:         titleCase(e.Type.String()),
		Size:         formatBytes(e.Size),
		Modified:     e.ModTime.Local().Format("2006-01-02 15:04:05"),
		Camera:       cmp.Or(mi.Camera, "—"),
		Lens:         cmp.Or(mi.Lens, "—"),
		Aperture:     cmp.Or(mi.Aperture, "—"),
		ShutterSpeed: cmp.Or(mi.ShutterSpeed, "—"),
		ISO:          cmp.Or(mi.ISO, "—"),
		FocalLength:  cmp.Or(mi.FocalLength, "—"),
		Favorite:     e.Favorite,
	}
	if !mi.Created.IsZero() {
		info.Created = mi.Created.Local().Format("2006-01-02 15:04:05")
	} else {
		info.Created = "—"
	}
	info.Dimensions = cached.dims

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(info)
}

// decodeDimensions returns "WxH" for normal photo formats. RAW/HEIC/video
// need a heavier decoder and aren't worth the cost for an info panel; they
// get "—".
func decodeDimensions(e cache.Entry) string {
	if e.Type != scan.TypePhoto {
		return "—"
	}
	f, err := os.Open(e.Path)
	if err != nil {
		return "—"
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%d × %d", cfg.Width, cfg.Height)
}

// titleCase upper-cases the first byte of s. Trivial helper that mirrors
// ui.titleCaseGio so the type column in the info panel matches the GUI.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// formatBytes is a compact human size formatter used by the info panel.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
