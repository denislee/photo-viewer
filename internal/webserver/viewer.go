package webserver

import (
	"fmt"
	"html"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// handleView renders a single-media page with the image or video centered
// and prev/next links into the surrounding list. The `from` query carries
// the list context — without it, the viewer renders without nav arrows.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/view/")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	cur, ok := s.lookupEntry(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	vi, hasCtx := s.viewFromQuery(r.URL.Query())
	var prev, next *cache.Entry
	var pos, total int
	var ctxQ, backHref, backLabel string
	if hasCtx {
		switch vi.kind {
		case "trash":
			prev, next, pos, total = s.trashNeighbors(cur.Path)
		default:
			// Reuse the generation-checked, TTL-cached view total (same value the
			// gallery page renders) so a navigation burst amortizes the O(view)
			// count; each keystroke then only pays the bounded position probe.
			total = s.cachedCountView(vi.v)
			prev, next, pos, total = s.index.NeighborsWithTotal(vi.v, cur.Path, total)
		}
		ctxQ = vi.ctxQuery
		backHref = vi.backHref
		backLabel = vi.backLabel
	} else {
		backHref = "/"
		backLabel = "Gallery"
	}

	s.renderViewer(w, cur, prev, next, ctxQ, backHref, backLabel, pos, total)
}

// trashNeighbors returns the prev/next/pos/total tuple for the trash listing,
// matching the shape of cache.Index.Neighbors so the viewer can treat the
// two sources identically.
func (s *Server) trashNeighbors(path string) (prev, next *cache.Entry, pos, total int) {
	entries := s.trashEntries()
	total = len(entries)
	for i, e := range entries {
		if e.Path == path {
			pos = i + 1
			if i > 0 {
				p := entries[i-1]
				prev = &p
			}
			if i < len(entries)-1 {
				n := entries[i+1]
				next = &n
			}
			return
		}
	}
	return nil, nil, 0, total
}

// renderViewer writes the single-media page. pos is the 1-based position
// in the surrounding list (0 when no context was provided); total is the
// list length. prev/next are nil at the edges.
func (s *Server) renderViewer(w http.ResponseWriter,
	cur cache.Entry, prev, next *cache.Entry,
	ctxQ, backHref, backLabel string, pos, total int,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHeader)

	prevHref := ""
	if prev != nil {
		prevHref = "/view/" + prev.ThumbID
		if ctxQ != "" {
			prevHref += "?" + ctxQ
		}
	}
	nextHref := ""
	if next != nil {
		nextHref = "/view/" + next.ThumbID
		if ctxQ != "" {
			nextHref += "?" + ctxQ
		}
	}

	star := "☆"
	starCls := "vfav"
	if cur.Favorite {
		star = "★"
		starCls += " on"
	}

	fmt.Fprint(w, `<div class="viewer">`)
	fmt.Fprint(w, `<header class="vbar">`)
	fmt.Fprintf(w, `<a class="back" href="%s">‹ %s</a>`, html.EscapeString(backHref), html.EscapeString(backLabel))
	fmt.Fprintf(w, `<span class="vname">%s</span>`, html.EscapeString(filepath.Base(cur.Path)))
	if pos > 0 && total > 0 {
		fmt.Fprintf(w, `<span class="vpos">%d / %d</span>`, pos, total)
	} else {
		fmt.Fprint(w, `<span class="vpos"></span>`)
	}
	fmt.Fprintf(w, `<button type="button" class="%s" id="favBtn" data-id="%s" title="Toggle favorite (f)">%s</button>`,
		starCls, cur.ThumbID, star)
	fmt.Fprintf(w, `<button type="button" class="vinfo" id="infoBtn" data-id="%s" title="Info (i)">i</button>`, cur.ThumbID)
	fmt.Fprint(w, `</header>`)

	fmt.Fprint(w, `<div class="vstage" id="vstage">`)
	mediaURL := "/media/" + cur.ThumbID
	if cur.Type == scan.TypeVideo {
		// iOS Safari (and desktop Safari) play HLS natively in a <video>
		// tag, but cannot decode the containers/codecs the GUI handles via
		// mpv (mkv, avi, webm, mts…). For those we serve an on-the-fly HLS
		// transcode; for already-Safari-friendly containers (mp4/mov) we
		// serve the original directly so we don't burn CPU transcoding a
		// file the device can already play. The src is picked client-side
		// because only the browser knows whether it has native HLS support.
		//
		// poster shows the cached thumbnail (the same /thumb/<id> URL the grid
		// uses) until the first frame decodes, instead of a black box (W-13).
		//
		// Fallback (W-13): a container that needs transcoding (needsHls) points
		// at hlsURL on every browser, including non-Safari ones (canHls ==
		// false). They can't play HLS without MSE, but pointing at the working
		// transcode URL is strictly better than handing them a raw original they
		// definitely can't decode — and we reveal a caption with a direct
		// download link client-side so the user isn't left with a silent black
		// box. The natively-playable path (mp4/mov → original) is unchanged. The
		// full hls.js/MSE embed is deliberately deferred as a separate decision.
		hlsURL := "/hls/" + cur.ThumbID + "/index.m3u8"
		posterURL := "/thumb/" + cur.ThumbID
		needs := needsHLS(cur.Path)
		fmt.Fprintf(w, `<video class="vmedia" id="vmedia" controls preload="metadata" playsinline poster="%s"></video>`, posterURL)
		if needs {
			// Hidden until the picker confirms the browser lacks native HLS.
			fmt.Fprintf(w, `<p class="vnote" id="vnote" hidden>Unsupported format — <a href="%s" download>download original</a></p>`, mediaURL)
		}
		fmt.Fprintf(w, `<script>(function(){var v=document.getElementById('vmedia');`+
			`var canHls=!!v.canPlayType('application/vnd.apple.mpegurl');`+
			`var needsHls=%t;v.src=needsHls?%q:%q;`+
			`if(needsHls&&!canHls){var n=document.getElementById('vnote');if(n)n.hidden=false;}})();</script>`,
			needs, hlsURL, mediaURL)
	} else {
		// Point the <img> at /display, not /media: /display serves a
		// browser-renderable image for every entry — the original bytes for a
		// small JPEG/PNG/WebP/GIF, or a ~2048 px JPEG rendition for RAW/HEIC/TIFF
		// and oversized originals — so RAW/HEIC render at all and plain photos
		// ship a downscaled copy instead of tens of MB (W-03). /media stays the
		// download-original route.
		displayURL := "/display/" + cur.ThumbID
		fmt.Fprintf(w, `<img class="vmedia" src="%s" alt="%s">`,
			displayURL, html.EscapeString(filepath.Base(cur.Path)))
	}
	// Tap zones — large invisible areas on the left/right so anywhere
	// outside the controls advances. The visible arrow buttons are
	// duplicated for clarity.
	if prevHref != "" {
		fmt.Fprintf(w, `<a class="varrow vprev" href="%s" aria-label="Previous">‹</a>`, prevHref)
	}
	if nextHref != "" {
		fmt.Fprintf(w, `<a class="varrow vnext" href="%s" aria-label="Next">›</a>`, nextHref)
	}
	fmt.Fprint(w, `</div>`)

	// Info panel, hidden by default. Populated from /api/info on first show.
	fmt.Fprint(w, `<aside class="vinfo-panel" id="infoPanel" hidden></aside>`)

	fmt.Fprint(w, `</div>`)

	// Inline the keybinding + favorite + info JS so the viewer doesn't
	// require an external script file.
	fmt.Fprint(w, viewerScript(prevHref, nextHref, backHref))
	fmt.Fprint(w, `</body></html>`)
}
