package webserver

import (
	"cmp"
	"fmt"
	"html"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
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

// renderSidebar emits the categories panel: Favorites, Trash, library root,
// top-level subdirectories (with year grouping), and a year list. Active row
// matches the currently-rendered view. Each link preserves the filter+raw
// toolbar state so toggling a category doesn't reset the view options.
func (s *Server) renderSidebar(w http.ResponseWriter, vi viewInfo, dirChildren []string, dirChildCounts map[string]int, trashCount int) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	v := vi.v
	agg := s.rootSidebarAgg(v.Filter, v.ShowRAW)
	fmt.Fprint(w, `<nav class="sidebar"><div class="sec">Library</div>`)

	sidebarRow(w, withExtra("/"), "All media", "", agg.allCount, vi.kind == "all")
	sidebarRow(w, withExtra("/favorites"), "Favorites", "★", agg.favCount, vi.kind == "favorites")

	if trashCount > 0 || vi.kind == "trash" {
		sidebarRow(w, "/trash", "Trash", "🗑", trashCount, vi.kind == "trash")
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
	// cached root aggregate and pays one cached count lookup for the open
	// dir — which renderGalleryPage resolves once and passes in as
	// dirChildren/dirChildCounts, shared with renderSubdirChips (W-08).
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
			sidebarRow(w, withExtra("/year/"+ys), ys, "", y.Count, active)
		}
	}
	fmt.Fprint(w, `</nav>`)
}

// sidebarCacheTTL bounds how long a cached sidebar aggregate is reused. Like
// trashEntriesTTL, a few seconds coalesces a navigation burst (every page
// render re-derives the same aggregate scans) without letting counts drift
// noticeably after an import, rebuild, or favorite toggle. Past the TTL the
// stale aggregate is served while one background refresh runs (W5-01). It
// wraps view-count and child-count caches that are themselves
// stale-while-revalidate, so counts can lag by up to two refresh cycles after
// an import or a GUI-side change (W6-01). The web UI's own favorite toggle
// clears the sidebar and view-count caches explicitly.
const sidebarCacheTTL = 5 * time.Second

const viewCountTTL = 5 * time.Second

// viewCountCacheCap bounds the view-count cache before the insert path sweeps
// it. The cache keys on the full cache.View (dir path × filter × raw), so a
// long-lived server browsing many directories would otherwise accumulate one
// entry per distinct view ever rendered — the TTL only marks entries stale and
// overwrites them in place, it never deletes, so the map grows without bound
// (W-07). Past this many entries the insert path drops everything the TTL has
// already made stale in one O(n) pass; the cap keeps that pass off the common
// path. The child-count cache uses the same cap.
const viewCountCacheCap = 1024

// cachedCountView returns the count for v, reusing a cached value if it is
// fresh enough and the index has not been cleared since the value was computed.
// The compute runs outside the cache lock, so a slow CountView doesn't stall
// unrelated lookups.
func (s *Server) cachedCountView(v cache.View) int {
	return s.viewCounts.get(v, s.index.Generation(), func() int {
		return s.index.CountView(v)
	})
}

// rootSidebarAgg returns the cached library-root sidebar aggregate for the
// given filter/raw toggles, recomputing it when the entry is missing or the
// index was cleared (generation mismatch). An entry older than
// sidebarCacheTTL is served stale while one background refresh recomputes it
// (stale-while-revalidate). The recompute runs outside the cache lock, so
// concurrent renders of other toggles don't queue behind it, and the
// All/Favorites totals share the view-count cache with the gallery header
// (W3-01).
func (s *Server) rootSidebarAgg(filter string, showRAW bool) *sidebarAgg {
	key := sidebarKey{filter: filter, showRAW: showRAW}
	return s.sidebar.get(key, s.index.Generation(), func() *sidebarAgg {
		return &sidebarAgg{
			allCount: s.cachedCountView(cache.View{Kind: "all", Filter: filter, ShowRAW: showRAW}),
			favCount: s.cachedCountView(cache.View{Kind: "favorites", Filter: filter, ShowRAW: showRAW}),
			subdirs:  s.cachedSubdirs(s.libraryRoot),
			counts:   s.cachedChildCounts(s.libraryRoot, filter, showRAW),
			years:    s.index.Years(filter, showRAW),
		}
	})
}

// renderSubdirsGrouped renders the given child directories, using the supplied
// per-directory counts (fetched once by the caller so the cached root sidebar
// doesn't re-query). Subdirs whose basename matches YYYY-MM-DD are bucketed
// under collapsible YYYY <details> groups so a folder with thousands of
// date-named children doesn't blow out the sidebar. Non-date directories render
// normally above the year buckets.
//
// It runs in a single pass and requires its input to be sorted by name, with
// every element sharing one parent — which is what listSubdirs guarantees by
// relying on os.ReadDir's lexical order. That ordering makes same-year
// YYYY-MM-DD entries contiguous within the date dirs and the years ascend, so
// the year buckets can be accumulated against the last one opened: no map, no
// re-sort, no re-parse of the year (W4-09).
func (s *Server) renderSubdirsGrouped(w http.ResponseWriter, subdirs []string, counts map[string]int, vi viewInfo) {
	extra := extraQuery(vi.v.Filter, vi.v.ShowRAW)
	withExtra := func(href string) string { return appendQuery(href, extra) }

	type bucket struct {
		year  string
		dirs  []string
		total int
		open  bool
	}
	var buckets []bucket
	var nonDate []string
	for _, d := range subdirs {
		base := filepath.Base(d)
		if !dateFolderRe.MatchString(base) {
			nonDate = append(nonDate, d)
			continue
		}
		y := base[:4]
		if len(buckets) == 0 || buckets[len(buckets)-1].year != y {
			buckets = append(buckets, bucket{year: y})
		}
		b := &buckets[len(buckets)-1]
		b.dirs = append(b.dirs, d)
		b.total += counts[d]
		// Open the bucket containing the active directory, so users land on
		// an expanded tree without an extra click.
		if vi.kind == "dir" && vi.v.Dir == d {
			b.open = true
		}
	}

	for _, d := range nonDate {
		href := withExtra(s.dirHref(d))
		active := vi.kind == "dir" && vi.v.Dir == d
		sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
	}

	for _, b := range buckets {
		openAttr := ""
		if b.open {
			openAttr = " open"
		}
		fmt.Fprintf(w,
			`<details class="year-bucket"%s><summary><span class="label">%s</span><span class="badge-count">%d</span></summary>`,
			openAttr, html.EscapeString(b.year), b.total)
		for _, d := range b.dirs {
			href := withExtra(s.dirHref(d))
			active := vi.kind == "dir" && vi.v.Dir == d
			sidebarRow(w, href, filepath.Base(d), "", counts[d], active)
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
	slices.SortFunc(chips, func(a, b chip) int {
		if c := cmp.Compare(b.count, a.count); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
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
func sidebarRow(w http.ResponseWriter, href, label, glyph string, count int, active bool) {
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
