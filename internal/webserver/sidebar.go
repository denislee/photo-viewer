package webserver

import (
	"fmt"
	"html"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

// dateFolderRe matches subdir basenames the import flow produces: YYYY-MM-DD.
// Mirrors internal/ui.dateFolderRe so the web sidebar's year grouping bins
// directories the same way the native sidebar does.
var dateFolderRe = regexp.MustCompile(`^(\d{4})-\d{2}-\d{2}$`)

// sidebarKey identifies a cached sidebar aggregate: the two toolbar toggles
// that change every count in it.
type sidebarKey struct {
	filter  string
	showRAW bool
}

// sidebarAgg holds the expensive, view-independent inputs the sidebar needs:
// the All/Favorites totals, the library-root child directories with their
// filtered counts, and the per-year totals. Active-row highlighting and the
// per-directory drill-down are cheap and stay out of the cache.
type sidebarAgg struct {
	allCount int
	favCount int
	subdirs  []string
	counts   map[string]int
	years    []cache.YearStat
}

type sidebarEntry struct {
	agg      *sidebarAgg
	at       time.Time
	indexGen uint64 // Index.Generation() at cache-fill time; stale if changed
}

type viewCountEntry struct {
	n        int
	at       time.Time
	indexGen uint64 // Index.Generation() at cache-fill time; stale if changed
}

// renderSidebar emits the categories panel: Favorites, Trash, library root,
// top-level subdirectories (with year grouping), and a year list. Active row
// matches the currently-rendered view. Each link preserves the filter+raw
// toolbar state so toggling a category doesn't reset the view options.
func (s *Server) renderSidebar(w http.ResponseWriter, vi viewInfo, dirChildren []string, dirChildCounts map[string]int) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	v := vi.v
	agg := s.rootSidebarAgg(v.Filter, v.ShowRAW)
	fmt.Fprint(w, `<nav class="sidebar"><div class="sec">Library</div>`)

	s.sidebarRow(w, withExtra("/"), "All media", "", agg.allCount, vi.kind == "all")
	s.sidebarRow(w, withExtra("/favorites"), "Favorites", "★", agg.favCount, vi.kind == "favorites")

	trashCount := s.countTrash()
	if trashCount > 0 || vi.kind == "trash" {
		s.sidebarRow(w, "/trash", "Trash", "🗑", trashCount, vi.kind == "trash")
	}

	// Top-level subdirectories. Sourced from the filesystem (mirroring the
	// native sidebar) so newly-created folders show up without a rebuild.
	if len(agg.subdirs) > 0 {
		fmt.Fprint(w, `<div class="sec">Folders</div>`)
		s.renderSubdirsGrouped(w, agg.subdirs, agg.counts, vi)
	}

	// When viewing a directory, surface its child folders as a nested
	// section so the user can drill down instead of paginating through
	// a 100k-photo flat list. This is per-directory, so it stays off the
	// cached root aggregate and pays one live count query for the open dir —
	// which renderGalleryPage runs once and passes in as dirChildren/
	// dirChildCounts, shared with renderSubdirChips (W-08).
	if v.Kind == "dir" && len(dirChildren) > 0 {
		fmt.Fprintf(w, `<div class="sec">In %s</div>`, html.EscapeString(filepath.Base(v.Dir)))
		s.renderSubdirsGrouped(w, dirChildren, dirChildCounts, vi)
	}

	// Year list. Includes per-year counts that respect the active filter.
	if len(agg.years) > 0 {
		fmt.Fprint(w, `<div class="sec">Years</div>`)
		for _, y := range agg.years {
			ys := strconv.Itoa(y.Year)
			active := vi.kind == "year" && v.Year == y.Year
			s.sidebarRow(w, withExtra("/year/"+ys), ys, "", y.Count, active)
		}
	}
	fmt.Fprint(w, `</nav>`)
}

// sidebarCacheTTL bounds how long a cached sidebar aggregate is reused. Like
// trashEntriesTTL, a few seconds coalesces a navigation burst (every page
// render re-derives the same aggregate scans) without letting counts drift
// noticeably after an import, rebuild, or favorite toggle — all of which
// self-heal on the next refresh past the window.
const sidebarCacheTTL = 5 * time.Second

const viewCountTTL = 5 * time.Second

// viewCountCacheCap bounds viewCountCache before the miss path sweeps it. The
// cache keys on the full cache.View (dir path × filter × raw), so a long-lived
// server browsing many directories would otherwise accumulate one entry per
// distinct view ever rendered — the TTL only marks entries stale and overwrites
// them in place, it never deletes, so the map grows without bound (W-07). Past
// this many entries the miss path drops everything the TTL has already made
// stale in one O(n) pass; the cap keeps that pass off the common path.
const viewCountCacheCap = 1024

// cachedCountView returns the count for v, reusing a cached value if it is
// fresh enough and the index has not been cleared since the value was computed.
func (s *Server) cachedCountView(v cache.View) int {
	gen := s.index.Generation()
	s.viewCountMu.Lock()
	defer s.viewCountMu.Unlock()
	if e, ok := s.viewCountCache[v]; ok && e.indexGen == gen && time.Since(e.at) < viewCountTTL {
		return e.n
	}
	n := s.index.CountView(v)
	if s.viewCountCache == nil {
		s.viewCountCache = make(map[cache.View]viewCountEntry)
	}
	// Bound the map: once it grows past the cap, drop every entry the TTL has
	// already made stale in a single pass so browsing many dir views over a
	// long uptime doesn't leak one entry per view forever (W-07). Only runs
	// past the cap, so the common path stays a plain insert; the fresh entry
	// written just below survives the sweep by definition.
	if len(s.viewCountCache) > viewCountCacheCap {
		for k, e := range s.viewCountCache {
			if time.Since(e.at) >= viewCountTTL {
				delete(s.viewCountCache, k)
			}
		}
	}
	s.viewCountCache[v] = viewCountEntry{n: n, at: time.Now(), indexGen: gen}
	return n
}

// rootSidebarAgg returns the cached library-root sidebar aggregate for the
// given filter/raw toggles, recomputing it when the entry is missing, older
// than sidebarCacheTTL, or the index was cleared (generation mismatch). The
// lock is held across the recompute (as trashEntries does) so a navigation
// burst blocks briefly on the first render then hits the cache.
func (s *Server) rootSidebarAgg(filter string, showRAW bool) *sidebarAgg {
	gen := s.index.Generation()
	key := sidebarKey{filter: filter, showRAW: showRAW}
	s.sidebarMu.Lock()
	defer s.sidebarMu.Unlock()
	if e, ok := s.sidebarCache[key]; ok && e.indexGen == gen && time.Since(e.at) < sidebarCacheTTL {
		return e.agg
	}
	agg := &sidebarAgg{
		allCount: s.index.CountView(cache.View{Kind: "all", Filter: filter, ShowRAW: showRAW}),
		favCount: s.index.CountView(cache.View{Kind: "favorites", Filter: filter, ShowRAW: showRAW}),
		subdirs:  listSubdirs(s.libraryRoot),
		counts:   s.index.CountChildDirsFiltered(s.libraryRoot, filter, showRAW),
		years:    s.index.Years(filter, showRAW),
	}
	if s.sidebarCache == nil {
		s.sidebarCache = make(map[sidebarKey]sidebarEntry)
	}
	s.sidebarCache[key] = sidebarEntry{agg: agg, at: time.Now(), indexGen: gen}
	return agg
}

// renderSubdirsGrouped renders the given child directories, using the supplied
// per-directory counts (fetched once by the caller so the cached root sidebar
// doesn't re-query). Subdirs whose basename matches YYYY-MM-DD are bucketed
// under collapsible YYYY <details> groups so a folder with thousands of
// date-named children doesn't blow out the sidebar. Non-date directories render
// normally above the year buckets.
func (s *Server) renderSubdirsGrouped(w http.ResponseWriter, subdirs []string, counts map[string]int, vi viewInfo) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	type bucket struct {
		dirs  []string
		total int
	}
	buckets := map[string]*bucket{}
	var nonDate []string
	for _, d := range subdirs {
		m := dateFolderRe.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			nonDate = append(nonDate, d)
			continue
		}
		b, ok := buckets[m[1]]
		if !ok {
			b = &bucket{}
			buckets[m[1]] = b
		}
		b.dirs = append(b.dirs, d)
		b.total += counts[d]
	}

	for _, d := range nonDate {
		href := withExtra(s.dirHref(d))
		active := vi.kind == "dir" && vi.v.Dir == d
		s.sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
	}

	years := make([]string, 0, len(buckets))
	for y := range buckets {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		b := buckets[y]
		// Open the bucket by default if it contains the active directory,
		// so users land on an expanded tree without an extra click.
		openAttr := ""
		for _, d := range b.dirs {
			if vi.kind == "dir" && vi.v.Dir == d {
				openAttr = " open"
				break
			}
		}
		fmt.Fprintf(w,
			`<details class="year-bucket" data-year="%s"%s><summary><span class="label">%s</span><span class="badge-count">%d</span></summary>`,
			html.EscapeString(y), openAttr, html.EscapeString(y), b.total)
		sort.Strings(b.dirs)
		for _, d := range b.dirs {
			href := withExtra(s.dirHref(d))
			active := vi.kind == "dir" && vi.v.Dir == d
			s.sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
		}
		fmt.Fprint(w, `</details>`)
	}
}

// renderSubdirChips renders a compact strip of child-folder chips above
// the grid for large directories. Cheap visual cue that there are
// subfolders worth drilling into without scrolling past the grid. The
// child directories and their filtered counts are supplied by the caller
// (renderGalleryPage) so the os.ReadDir + grouped-count scan run once per
// page, shared with the sidebar's "In <dir>" section (W-08).
func (s *Server) renderSubdirChips(w http.ResponseWriter, vi viewInfo, children []string, counts map[string]int) {
	if len(children) == 0 {
		return
	}
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	type chip struct {
		path  string
		name  string
		count int
	}
	chips := make([]chip, 0, len(children))
	for _, c := range children {
		chips = append(chips, chip{path: c, name: filepath.Base(c), count: counts[c]})
	}
	// Heaviest subfolders first so the user sees the chunkiest drill-downs
	// without scrolling the chip strip.
	sort.Slice(chips, func(i, j int) bool {
		if chips[i].count != chips[j].count {
			return chips[i].count > chips[j].count
		}
		return chips[i].name < chips[j].name
	})
	fmt.Fprint(w, `<div class="chips">`)
	for _, c := range chips {
		href := appendQuery(s.dirHref(c.path), extra)
		fmt.Fprintf(w,
			`<a class="chip" href="%s"><span>%s</span><span class="chip-count">%d</span></a>`,
			html.EscapeString(href), html.EscapeString(c.name), c.count)
	}
	fmt.Fprint(w, `</div>`)
}

// sidebarRow renders one anchor in the sidebar with an optional leading
// glyph (used for the Favorites star) and a right-aligned count badge.
func (s *Server) sidebarRow(w http.ResponseWriter, href, label, glyph string, count int, active bool) {
	cls := "row"
	if active {
		cls += " active"
	}
	g := ""
	if glyph != "" {
		g = `<span class="glyph">` + html.EscapeString(glyph) + `</span>`
	}
	fmt.Fprintf(w,
		`<a class="%s" href="%s">%s<span class="label">%s</span><span class="badge-count">%d</span></a>`,
		cls, html.EscapeString(href), g, html.EscapeString(label), count,
	)
}
