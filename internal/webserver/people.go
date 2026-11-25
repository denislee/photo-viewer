package webserver

import (
	"fmt"
	"html"
	"net/http"
	"strconv"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// This file serves the READ-ONLY People layer: the overview of named people,
// the per-person gallery, and the cropped avatars the two of them render.
// Naming, merging and hiding people stay in the native GUI; the web UI only
// reads what is already in the index.

// faceCacheControl is /face's Cache-Control. The crop for a face id is
// deterministic from the source thumbnail and the bbox, so a day's cache is
// safe and cheap; "private" keeps it out of shared proxies.
const faceCacheControl = "private, max-age=86400"

// handlePeople serves the People overview: one card per visible (not hidden)
// person, in ListPeople order (photo count descending, then name).
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	filter, showRAW := parseToggles(r.URL.Query())
	s.renderPeoplePage(w, r, s.newViewInfo("people", 0, "", filter, showRAW))
}

// renderPeoplePage writes the People overview. It reuses the gallery chrome
// (header, hamburger, sidebar, mobile bar) so moving between the grid and the
// people list doesn't change the page frame.
func (s *Server) renderPeoplePage(w http.ResponseWriter, r *http.Request, vi viewInfo) {
	people, err := s.index.ListPeople()
	if err != nil {
		http.Error(w, "failed to list people", http.StatusInternalServerError)
		return
	}
	visible := make([]cache.Person, 0, len(people))
	for _, p := range people {
		if !p.Hidden {
			visible = append(visible, p)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pageHeaderBytes)
	fmt.Fprint(w, `<input type="checkbox" id="navtoggle" class="navtoggle" hidden>`)
	fmt.Fprint(w, `<div class="layout">`)
	// The sidebar's trash row is the only count it can't derive itself; pass the
	// same cached listing length the gallery pages pass.
	s.renderSidebar(w, vi, nil, nil, len(s.trashEntries()))
	fmt.Fprint(w, `<main class="content">`)
	fmt.Fprint(w, mobileTopBar)
	fmt.Fprintf(w, `<h1>People <span class="count">%d people</span></h1>`, len(visible))
	fmt.Fprint(w, `<div class="grid">`)
	for _, p := range visible {
		writePersonCell(w, p)
	}
	fmt.Fprint(w, `</div></main></div>`)
	fmt.Fprint(w, `</body></html>`)
}

// writePersonCell emits one person card: the cover face's crop (when the
// person has one) plus a caption of the name and photo count. Every name is
// escaped — it is user-supplied text, not an identifier.
func writePersonCell(w http.ResponseWriter, p cache.Person) {
	img := ""
	if p.Cover.ID != 0 {
		img = fmt.Sprintf(`<img loading="lazy" decoding="async" src="/face/%d" alt="">`, p.Cover.ID)
	}
	name := html.EscapeString(p.Name)
	fmt.Fprintf(w,
		`<a class="cell person" href="/person/%d" title="%s">%s<span class="name">%s · %d photos</span></a>`,
		p.ID, name, img, name, p.Photos)
}

// handlePerson serves one person's gallery, rendered by the same code path as
// every other gallery view (so the toolbar, infinite scroll and viewer context
// all work unchanged). Unknown or malformed ids are 404s.
func (s *Server) handlePerson(w http.ResponseWriter, r *http.Request) {
	id, ok := positiveInt64(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	filter, showRAW := parseToggles(r.URL.Query())
	vi := s.personViewInfo(id, filter, showRAW)
	if vi.title == "" {
		http.NotFound(w, r)
		return
	}
	s.renderGalleryPage(w, r, vi)
}

// positiveInt64 parses a path/query value as a strictly positive int64 id, so
// zero, negatives and garbage are rejected before reaching the index.
func positiveInt64(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// personViewInfo builds the viewInfo for person id's gallery. A person has no
// slot in newViewInfo's (kind, year, dirAbs) parameters, so the id-dependent
// fields are filled in here on top of the shared constructor: the title is the
// person's name, the base path is /person/<id>, and the forwarded context query
// carries pid so the viewer, /api/page and prev/next all stay inside the person
// list. A zero title means the person does not exist and the caller answers
// 404.
func (s *Server) personViewInfo(id int64, filter string, showRAW bool) viewInfo {
	name, ok := s.index.PersonName(id)
	if !ok {
		return viewInfo{}
	}
	vi := s.newViewInfo("person", 0, "", filter, showRAW)
	vi.v.PersonID = id
	vi.title = name
	vi.base = "/person/" + strconv.FormatInt(id, 10)
	vi.ctxQuery = buildCtxQuery("person", "", "", filter, showRAW) + "&pid=" + strconv.FormatInt(id, 10)
	vi.backHref = vi.link(vi.base)
	return vi
}

// handleFace serves the cached JPEG crop for /face/<id>. The crop is built from
// the photo's own cached thumbnail (never the original: the bbox is in
// thumbnail pixel space), so a browser asking for an avatar never triggers a
// full-resolution decode. Every miss — no face, no entry for its path, no
// thumbnail, no crop cache dir — is a 404 rather than a 500: the ids are
// opaque to clients and a stale index row is the common cause.
func (s *Server) handleFace(w http.ResponseWriter, r *http.Request) {
	if s.faceCacheDir == "" {
		http.NotFound(w, r)
		return
	}
	id, ok := positiveInt64(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, ok := s.index.Face(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	entry, ok := s.index.GetEntry(f.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	src, err := s.store.Path(entry)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	paths, errs := face.EnsureThumbs(s.faceCacheDir, src,
		[]face.FaceCrop{{ID: id, BBox: f.BBox}})
	if len(paths) != 1 || len(errs) != 1 || errs[0] != nil || paths[0] == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", faceCacheControl)
	http.ServeFile(w, r, paths[0])
}
