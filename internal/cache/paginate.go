package cache

import (
	"database/sql"
	"errors"
	"log"
	"slices"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// View describes a sub-list of the index used by the web UI. Each view is
// expressible as a (WHERE, args, total-count) tuple over the entries table
// ordered by path, which lets pagination and neighbor lookup share one code
// path instead of duplicating four queries.
type View struct {
	Kind    string // "all" | "favorites" | "year" | "dir"
	Dir     string // absolute path; only for Kind == "dir"
	Year    int    // only for Kind == "year"
	Filter  string // "All" | "Photos" | "Videos"
	ShowRAW bool   // when false, RAW entries are excluded
}

// whereClause renders the SQL WHERE fragment (without the leading "WHERE")
// and the bound args for v. The media-type filter (Filter + ShowRAW) is
// applied to every kind so the web UI's toolbar toggles take effect in any
// view.
func (v View) whereClause() (string, []any) {
	typeWhere, typeArgs := typeFilterClause(v.Filter, v.ShowRAW)
	switch v.Kind {
	case "favorites":
		return "favorite = 1" + typeWhere, typeArgs
	case "year":
		args := []any{v.Year}
		args = append(args, typeArgs...)
		return yearExpr + " = ?" + typeWhere, args
	case "dir":
		// Pure range only — no "OR path = ?" disjunct. The disjunct forced a
		// MULTI-INDEX OR + "USE TEMP B-TREE FOR ORDER BY", so one viewer
		// keystroke re-sorted the whole subtree. The range form streams off the
		// covering PK index. The exact-dir row (present only when a *file* path
		// is passed as Dir) is folded back in by the callers via dirExactEntry —
		// it sorts strictly before the whole range, so it's always element 0.
		lower, upper := dirRange(v.Dir)
		args := []any{lower, upper}
		args = append(args, typeArgs...)
		return "(path >= ? AND path < ?)" + typeWhere, args
	default: // "all"
		// typeFilterClause returns "" or " AND ...", so anchor with 1=1 so
		// the filter slots in cleanly when set.
		return "1=1" + typeWhere, typeArgs
	}
}

// whereWithCursor renders whereClause() plus one keyset bound on path, op being
// one of ">", "<" or "<=". It is the single place the neighbour probes and the
// keyset page build their WHERE from, so the SQL they issue is also the SQL the
// query-plan tests inspect.
//
// C5-01: for every non-dir kind the cursor is simply appended (`AND path op ?`),
// which is harmless because those kinds have no path range of their own. A dir
// view does: whereClause already renders `(path >= lower AND path < upper)`.
// SQLite gives an index column at most one lower and one upper bound, so a
// third `AND path > cursor` term degrades to a per-row filter evaluated across
// the entire subtree — the statement becomes O(dir size) per viewer keystroke
// instead of a range SEARCH. So for "dir" the cursor is clamped against
// [lower, upper) in Go and replaces one of the two existing bounds, keeping
// exactly one lower and one upper path bound in the statement:
//
//   - ">"  — cursor at/after lower means "everything strictly after cursor in
//     the range", i.e. (path > cursor AND path < upper). A cursor before the
//     range is already implied by the lower bound, so the whole range is used.
//   - "<"  — cursor at/before upper means (path >= lower AND path < cursor); a
//     cursor at/after upper selects the whole range.
//   - "<=" — like "<" but inclusive, except at the range's own upper bound
//     (path == upper is not in the view) where the plain half-open range is
//     both equivalent and strictly cheaper.
//
// The media-type filter and its args always follow the path bounds, matching
// whereClause's fragment order.
func (v View) whereWithCursor(op, cursor string) (string, []any) {
	if v.Kind != "dir" {
		where, args := v.whereClause()
		args = append(args, cursor)
		return where + " AND path " + op + " ?", args
	}

	lower, upper := dirRange(v.Dir)
	typeWhere, typeArgs := typeFilterClause(v.Filter, v.ShowRAW)

	frag := "(path >= ? AND path < ?)"
	args := []any{lower, upper}
	switch op {
	case ">":
		if cursor >= lower {
			frag = "(path > ? AND path < ?)"
			args = []any{cursor, upper}
		}
	case "<":
		if cursor <= upper {
			frag = "(path >= ? AND path < ?)"
			args = []any{lower, cursor}
		}
	case "<=":
		if cursor < upper {
			frag = "(path >= ? AND path <= ?)"
			args = []any{lower, cursor}
		}
	}
	args = append(args, typeArgs...)
	return frag + typeWhere, args
}

// dirExactEntry returns the "exact path" row for a dir view: the at-most-one
// entry whose path equals v.Dir itself (honoring the media-type filter). It
// exists only when a caller passes a *file* path where a directory is expected
// — the web /dir?path= handler only checks that the target is within the
// library root, not that it is a directory. whereClause's dir range covers
// [Dir+"/", ...) but not Dir itself, so the pre-I-08 "OR path = Dir" arm
// captured this row. That arm forced a MULTI-INDEX OR + temp b-tree; we replace
// it with this covering-index point lookup and stitch the row back in Go.
//
// The row's path == Dir sorts strictly before every range row (Dir < Dir+"/"),
// so combined view order is always [exact] ++ range. Returns nil for non-dir
// views and, in the common case, for dir views (entries are files, so a bare
// directory path is not itself a row).
func (i *Index) dirExactEntry(v View) *Entry {
	if v.Kind != "dir" {
		return nil
	}
	typeWhere, typeArgs := typeFilterClause(v.Filter, v.ShowRAW)
	args := append([]any{v.Dir}, typeArgs...)
	return i.queryOne(
		entrySelect+" WHERE path = ?"+
			typeWhere+" LIMIT 1",
		args...)
}

// CountView returns the number of entries that match the view.
func (i *Index) CountView(v View) int {
	where, args := v.whereClause()
	var n int
	if err := i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE "+where, args...).Scan(&n); err != nil {
		log.Printf("cache: CountView: %v", err)
		return 0
	}
	// The dir range excludes the exact-dir row (see dirExactEntry); count it
	// separately. It's disjoint from the range (Dir < Dir+"/"), so there's no
	// double counting. nil in the common case, leaving n unchanged.
	if i.dirExactEntry(v) != nil {
		n++
	}
	return n
}

// ListPage returns entries matching v in path order, offset/limit applied.
// limit <= 0 means "no limit" (used by code paths that intentionally want
// everything; the web UI always passes a positive limit).
func (i *Index) ListPage(v View, offset, limit int) []Entry {
	where, args := v.whereClause()

	// The dir view's exact-path row (see dirExactEntry) sorts before the whole
	// range, so it's combined element 0. Fold it into the LIMIT/OFFSET math: a
	// non-zero offset "spends" it, and when it lands on the current page it
	// steals one slot from limit. nil in every non-dir view and the common dir
	// case, so this collapses to the plain range page.
	exact := i.dirExactEntry(v)
	rangeOffset, rangeLimit := offset, limit
	prependExact := false
	if exact != nil {
		switch {
		case limit <= 0:
			// No-limit path returns everything (offset ignored, as before),
			// exact first.
			prependExact = true
		case offset == 0:
			prependExact = true
			rangeLimit = limit - 1 // exact takes one of the limit slots
		default:
			rangeOffset = offset - 1 // exact is skipped by the offset
		}
	}

	q := entrySelect + " WHERE " +
		where + " ORDER BY path"
	if limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, rangeLimit, rangeOffset)
	}
	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := scanEntries(rows, limit)
	if prependExact {
		out = append([]Entry{*exact}, out...)
	}
	return out
}

// NeighborsWithTotal returns (prev, next) entries adjacent to path within v,
// plus a 1-based position; prev or next are nil at the list edges and pos is 0
// when path isn't part of v. Only the bounded work runs here:
//
//   - prev/next use the path index directly — "biggest path strictly less than
//     X within the view" for prev, "smallest strictly greater" for next — so
//     they cost O(log N) regardless of view size.
//   - pos is `COUNT(*) WHERE <where-with-<=-cursor>`. SQLite bounds the scan
//     at the probe path, so the position costs O(pos) instead of the O(view)
//     COUNT+SUM aggregate the old combined query paid.
//
// Each statement's WHERE is whereClause() narrowed by the probe (see
// whereWithCursor), so it selects exactly the rows CountView totals over and
// pos can never disagree about which rows belong to the view.
func (i *Index) NeighborsWithTotal(v View, path string) (prev, next *Entry, pos int) {
	// Each probe clamps a cursor onto the view's own range (see
	// whereWithCursor): for a dir view that keeps one lower and one upper path
	// bound, so prev/next stay O(log N) and pos O(pos) instead of degrading to
	// an O(dir size) per-row filter (C5-01).
	prevWhere, prevArgs := v.whereWithCursor("<", path)
	prev = i.queryOne(
		entrySelect+" WHERE "+
			prevWhere+" ORDER BY path DESC LIMIT 1",
		prevArgs...)

	nextWhere, nextArgs := v.whereWithCursor(">", path)
	next = i.queryOne(
		entrySelect+" WHERE "+
			nextWhere+" ORDER BY path ASC LIMIT 1",
		nextArgs...)

	// Bounded position: rows at or before path within the view. The cursor
	// bound lets SQLite stop the index walk at the probe path, so this equals
	// the old query's SUM(path<=?) without scanning the whole view range.
	posWhere, posArgs := v.whereWithCursor("<=", path)
	_ = i.db.QueryRow(
		"SELECT COUNT(*) FROM entries WHERE "+posWhere,
		posArgs...).Scan(&pos)

	// Fold in the dir view's exact-path row (see dirExactEntry). Its path
	// (== Dir) sorts strictly before every range row, so relative to the probe
	// `path` it can only ever be the smallest element. Therefore:
	//   - it bumps pos when it is <= path (CountView folds the same row in,
	//     so the two never disagree);
	//   - it becomes `prev` only when no range row precedes path (it's the sole
	//     predecessor);
	//   - it becomes `next` only when it itself follows path (then it's smaller
	//     than any range successor, so it wins over the range's `next`).
	// nil in every non-dir view and the common dir case, leaving the range-only
	// results untouched.
	if exact := i.dirExactEntry(v); exact != nil {
		if exact.Path <= path {
			pos++
		}
		if prev == nil && exact.Path < path {
			prev = exact
		}
		if exact.Path > path {
			next = exact
		}
	}
	return prev, next, pos
}

// rowScanner is the shared surface of *sql.Row and *sql.Rows: anything that can
// decode one entry row. entryDecoder is written against it so single-row and
// multi-row readers use the same decoder.
type rowScanner interface{ Scan(dest ...any) error }

// entryDecoder decodes the entryCols column list into an Entry. It exists to
// keep every Entry read on one code path while avoiding two per-row costs of the
// old direct Scan: the fresh locals that escaped per row, and the reflection
// fallback database/sql takes for *scan.MediaType / *int64 / *int destinations.
//
// The nullable/numeric columns are scanned into `any` fields (the *any case is
// handled natively by database/sql, with no reflection), then converted by
// decode. dest holds the matching *any pointers in entryCols order and is what
// goes to Scan. One decoder is reused across every row of a drain: decode
// resets the numeric fields to nil before each Scan so a row can never inherit
// the previous row's values.
type entryDecoder struct {
	path, thumbID string
	typ, size     any
	mtime, fav    any
	dur           any
	dest          [7]any
}

// newEntryDecoder returns a decoder wired to dest in entryCols order: path,
// type, size, mtime, thumb_id, favorite, duration_ms.
func newEntryDecoder() *entryDecoder {
	d := &entryDecoder{}
	d.dest = [7]any{&d.path, &d.typ, &d.size, &d.mtime, &d.thumbID, &d.fav, &d.dur}
	return d
}

// asInt64 reports the value as an int64 when the driver handed back an integer
// (SQLite's only integer representation) and false otherwise, including NULL.
func asInt64(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

// decode scans exactly one row through d and converts it to an Entry. type,
// size and mtime are NOT NULL in every row the writer produces, so a NULL or
// otherwise unexpected value there is a malformed row and reported as an error;
// the multi-row drain skips it, exactly as the old typed Scan did when it hit a
// NULL. favorite and duration_ms fall back to 0 (false / unknown) on NULL.
func (d *entryDecoder) decode(s rowScanner) (Entry, error) {
	d.typ, d.size, d.mtime, d.fav, d.dur = nil, nil, nil, nil, nil
	if err := s.Scan(d.dest[:]...); err != nil {
		return Entry{}, err
	}
	typ, ok := asInt64(d.typ)
	if !ok {
		return Entry{}, errors.New("cache: malformed entry row")
	}
	size, ok := asInt64(d.size)
	if !ok {
		return Entry{}, errors.New("cache: malformed entry row")
	}
	mtime, ok := asInt64(d.mtime)
	if !ok {
		return Entry{}, errors.New("cache: malformed entry row")
	}
	fav, _ := asInt64(d.fav)
	dur, _ := asInt64(d.dur)
	return Entry{
		Path:       d.path,
		Type:       scan.MediaType(typ),
		Size:       size,
		ModTime:    time.Unix(mtime, 0),
		ThumbID:    d.thumbID,
		DurationMs: dur,
		Favorite:   fav != 0,
	}, nil
}

// scanEntry decodes one row of the entryCols column list. Most Entry reads go
// through it; FindDuplicates hand-scans its own narrower six-column projection,
// so it needs updating alongside entryCols.
func scanEntry(s rowScanner) (Entry, error) {
	return newEntryDecoder().decode(s)
}

// queryOne runs a single-row SELECT and decodes one Entry. Returns nil when
// the query produces no rows or fails.
func (i *Index) queryOne(query string, args ...any) *Entry {
	e, err := scanEntry(i.db.QueryRow(query, args...))
	if err != nil {
		return nil
	}
	return &e
}

// scanEntries drains rows produced by the standard 7-column SELECT. hint is the
// caller's expected row count (its page limit) and sizes the initial slice; the
// slice then grows by doubling, which keeps the drain amortized O(n) with at
// most one copy of the live entries instead of the old 1.25x regrowth.
func scanEntries(rows *sql.Rows, hint int) []Entry {
	if hint <= 0 {
		hint = 64
	}
	out := make([]Entry, 0, hint)
	dec := newEntryDecoder()
	for rows.Next() {
		e, err := dec.decode(rows)
		if err != nil {
			continue
		}
		if len(out) == cap(out) {
			out = slices.Grow(out, cap(out))
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: scanEntries: %v", err)
	}
	return out
}

// ListPageAfter is the keyset form of ListPage: up to limit entries of v whose
// path sorts strictly after afterPath, in path order (W4-05). An empty
// afterPath starts at the beginning, so it equals ListPage(v, 0, limit).
// Unlike OFFSET, which walks and discards every earlier row, the `path > ?`
// bound seeks straight to the page, so deep infinite scroll stays O(page)
// instead of O(offset). limit <= 0 means no limit.
func (i *Index) ListPageAfter(v View, afterPath string, limit int) []Entry {
	if afterPath == "" {
		return i.ListPage(v, 0, limit)
	}
	// The dir view's exact-path row (see dirExactEntry) sorts before every
	// range row. It belongs on this page only if it also sorts after the
	// cursor — never true for a cursor taken from the view itself, but the
	// stitch keeps the function correct for any afterPath.
	var exact *Entry
	if e := i.dirExactEntry(v); e != nil && e.Path > afterPath {
		exact = e
		if limit == 1 {
			return []Entry{*e}
		}
		if limit > 1 {
			limit--
		}
	}
	q, args := listPageAfterQuery(v, afterPath, limit)
	rows, err := i.db.Query(q, args...)
	if err != nil {
		log.Printf("cache: ListPageAfter: %v", err)
		return nil
	}
	defer rows.Close()
	out := scanEntries(rows, limit)
	if exact != nil {
		out = append([]Entry{*exact}, out...)
	}
	return out
}

// listPageAfterQuery builds ListPageAfter's range statement. Split out so the
// query-plan test checks the exact SQL that runs.
func listPageAfterQuery(v View, afterPath string, limit int) (string, []any) {
	where, args := v.whereWithCursor(">", afterPath)
	q := entrySelect + " WHERE " + where + " ORDER BY path"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return q, args
}
