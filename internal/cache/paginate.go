package cache

import (
	"database/sql"
	"log"
	"time"
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
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE path = ?"+
			typeWhere+" LIMIT 1",
		args...)
}

// CountView returns the number of entries that match the view.
func (i *Index) CountView(v View) int {
	where, args := v.whereClause()
	var n int
	if err := i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE "+where, args...).Scan(&n); err != nil {
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

	q := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE " +
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
	out := scanEntries(rows)
	if prependExact {
		out = append([]Entry{*exact}, out...)
	}
	return out
}

// Neighbors returns (prev, next) entries adjacent to path within v, plus a
// 1-based position and total. prev or next are nil at the list edges; pos is
// 0 when path isn't part of v.
//
// The lookups use the path index directly — "biggest path strictly less than X
// within the view" for prev, "smallest path strictly greater" for next — so
// they cost O(log N) regardless of view size. The position requires a COUNT
// over the WHERE clause but still uses the existing indexes.
func (i *Index) Neighbors(v View, path string) (prev, next *Entry, pos, total int) {
	where, args := v.whereClause()

	prevArgs := append([]any{}, args...)
	prevArgs = append(prevArgs, path)
	prev = i.queryOne(
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" AND path < ? ORDER BY path DESC LIMIT 1",
		prevArgs...)

	nextArgs := append([]any{}, args...)
	nextArgs = append(nextArgs, path)
	next = i.queryOne(
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" AND path > ? ORDER BY path ASC LIMIT 1",
		nextArgs...)

	// Single pass: COUNT(*) = total rows, SUM(path<=?) = rows at or before path = pos.
	countArgs := append([]any{path}, args...)
	_ = i.db.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(path <= ?), 0) FROM entries WHERE "+where,
		countArgs...).Scan(&total, &pos)

	// Fold in the dir view's exact-path row (see dirExactEntry). Its path
	// (== Dir) sorts strictly before every range row, so relative to the probe
	// `path` it can only ever be the smallest element. Therefore:
	//   - it bumps total, and bumps pos when it is <= path;
	//   - it becomes `prev` only when no range row precedes path (it's the sole
	//     predecessor);
	//   - it becomes `next` only when it itself follows path (then it's smaller
	//     than any range successor, so it wins over the range's `next`).
	// nil in every non-dir view and the common dir case, leaving the range-only
	// results untouched.
	if exact := i.dirExactEntry(v); exact != nil {
		total++
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
	return prev, next, pos, total
}

// queryOne runs a single-row SELECT and decodes one Entry. Returns nil when
// the query produces no rows or fails.
func (i *Index) queryOne(query string, args ...any) *Entry {
	row := i.db.QueryRow(query, args...)
	var e Entry
	var mtimeUnix int64
	var fav int
	if err := row.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err != nil {
		return nil
	}
	e.ModTime = time.Unix(mtimeUnix, 0)
	e.Favorite = fav != 0
	return &e
}

// scanEntries drains rows produced by the standard 7-column SELECT.
func scanEntries(rows *sql.Rows) []Entry {
	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err != nil {
			continue
		}
		e.ModTime = time.Unix(mtimeUnix, 0)
		e.Favorite = fav != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: scanEntries: %v", err)
	}
	return out
}
