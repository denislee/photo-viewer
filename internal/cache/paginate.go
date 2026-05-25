package cache

import (
	"database/sql"
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
		lower, upper := dirRange(v.Dir)
		args := []any{lower, upper, v.Dir}
		args = append(args, typeArgs...)
		return "((path >= ? AND path < ?) OR path = ?)" + typeWhere, args
	default: // "all"
		// typeFilterClause returns "" or " AND ...", so anchor with 1=1 so
		// the filter slots in cleanly when set.
		return "1=1" + typeWhere, typeArgs
	}
}

// CountView returns the number of entries that match the view.
func (i *Index) CountView(v View) int {
	where, args := v.whereClause()
	var n int
	if err := i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE "+where, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ListPage returns entries matching v in path order, offset/limit applied.
// limit <= 0 means "no limit" (used by code paths that intentionally want
// everything; the web UI always passes a positive limit).
func (i *Index) ListPage(v View, offset, limit int) []Entry {
	where, args := v.whereClause()
	q := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE " +
		where + " ORDER BY path"
	if limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
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

	posArgs := append([]any{}, args...)
	posArgs = append(posArgs, path)
	_ = i.db.QueryRow(
		"SELECT COUNT(*) FROM entries WHERE "+where+" AND path <= ?", posArgs...).Scan(&pos)

	totalArgs := append([]any{}, args...)
	_ = i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE "+where, totalArgs...).Scan(&total)
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
	return out
}
