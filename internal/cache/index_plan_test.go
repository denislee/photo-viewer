package cache

import (
	"path/filepath"
	"strings"
	"testing"
)

// explainPlan runs EXPLAIN QUERY PLAN for the given statement and returns the
// concatenated plan-node detail text (one node per line). Tests assert against
// this to prove the hot queries use an index and avoid full scans / temp
// b-trees. Lives in package cache so it can reach the unexported *sql.DB.
func explainPlan(t *testing.T, idx *Index, query string, args ...any) string {
	t.Helper()
	rows, err := idx.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	return b.String()
}

// mustContain / mustNotContain give readable failures that print the full plan.
func mustContain(t *testing.T, label, plan, want string) {
	t.Helper()
	if !strings.Contains(plan, want) {
		t.Errorf("%s: plan should contain %q\nplan:\n%s", label, want, plan)
	}
}

func mustNotContain(t *testing.T, label, plan, bad string) {
	t.Helper()
	if strings.Contains(plan, bad) {
		t.Errorf("%s: plan should NOT contain %q\nplan:\n%s", label, bad, plan)
	}
}

// TestPlanThumbIDLookup proves GetEntryByThumbID's query hits idx_entries_thumb
// (SEARCH) instead of the pre-I-08 full table SCAN it did per HTTP request.
func TestPlanThumbIDLookup(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	// The exact statement issued by GetEntryByThumbID (faces.go).
	const q = "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE thumb_id = ? LIMIT 1"
	plan := explainPlan(t, idx, q, "deadbeef")

	mustContain(t, "thumb", plan, "SEARCH entries USING INDEX idx_entries_thumb")
	mustNotContain(t, "thumb", plan, "SCAN entries")
}

// TestPlanNeighborsDirKind proves the dir-view neighbor probe streams off the
// PK range index — no MULTI-INDEX OR and no temp b-tree, which the old
// "OR path = ?" arm forced on every viewer keystroke.
func TestPlanNeighborsDirKind(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	v := View{Kind: "dir", Dir: "/lib"}
	where, args := v.whereClause()
	// The exact "prev neighbor" statement issued by Neighbors (paginate.go).
	q := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE " +
		where + " AND path < ? ORDER BY path DESC LIMIT 1"
	args = append(args, filepath.Join("/lib", "000100.jpg"))
	plan := explainPlan(t, idx, q, args...)

	mustContain(t, "neighbors-dir", plan, "SEARCH entries USING")
	mustNotContain(t, "neighbors-dir", plan, "SCAN entries")
	mustNotContain(t, "neighbors-dir", plan, "MULTI-INDEX OR")
	mustNotContain(t, "neighbors-dir", plan, "TEMP B-TREE")
}

// TestPlanYears proves the sidebar's Years() grouping streams off the ordered
// expression index idx_entries_yearexpr instead of scanning + building a temp
// b-tree for the GROUP BY.
func TestPlanYears(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	// The exact statement issued by Years (index.go) for filter "All"/showRAW,
	// where the type clause is empty.
	q := "SELECT " + yearExpr + " AS y, COUNT(*) FROM entries WHERE 1=1 GROUP BY y ORDER BY y DESC"
	plan := explainPlan(t, idx, q)

	mustContain(t, "years", plan, "idx_entries_yearexpr")
	mustNotContain(t, "years", plan, "TEMP B-TREE")
}

// TestPlanListByYear proves the year filter uses the expression index (SEARCH)
// instead of full-scanning the table to evaluate yearExpr per row. Since v6
// appended path to idx_entries_yearexpr, a path-only projection is now served
// entirely from the index (COVERING) with no table fetch.
func TestPlanListByYear(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	q := "SELECT path FROM entries WHERE " + yearExpr + " = ?"
	plan := explainPlan(t, idx, q, 2024)

	mustContain(t, "list-by-year", plan, "SEARCH entries USING COVERING INDEX idx_entries_yearexpr")
}

// TestPlanListDirAndListPage proves both the ListDir range query and the
// paginated ListPage query stream off the covering PK index with no temp
// b-tree — the pre-I-08 "OR path = ?" arm forced a MULTI-INDEX OR + sort on
// every directory listing.
func TestPlanListDirAndListPage(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	// ListDir's range statement (index.go).
	lower, upper := dirRange("/lib")
	listDirQ := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE path >= ? AND path < ? ORDER BY path"
	plan := explainPlan(t, idx, listDirQ, lower, upper)
	mustContain(t, "listdir", plan, "SEARCH entries USING")
	mustContain(t, "listdir", plan, "sqlite_autoindex_entries_1")
	mustNotContain(t, "listdir", plan, "SCAN entries")
	mustNotContain(t, "listdir", plan, "MULTI-INDEX OR")
	mustNotContain(t, "listdir", plan, "TEMP B-TREE")

	// ListPage's paginated statement (paginate.go) for a dir view.
	v := View{Kind: "dir", Dir: "/lib"}
	where, args := v.whereClause()
	listPageQ := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE " +
		where + " ORDER BY path LIMIT ? OFFSET ?"
	args = append(args, 100, 100)
	plan = explainPlan(t, idx, listPageQ, args...)
	mustContain(t, "listpage", plan, "SEARCH entries USING")
	mustNotContain(t, "listpage", plan, "SCAN entries")
	mustNotContain(t, "listpage", plan, "MULTI-INDEX OR")
	mustNotContain(t, "listpage", plan, "TEMP B-TREE")
}

// TestDeadIndexesDropped proves the migrations removed the write-only/superseded
// indexes and created the functional ones: v5 dropped idx_entries_year and
// idx_entries_duration; v6 replaced the single-column idx_entries_favorite with
// the covering idx_entries_favorite_path.
func TestDeadIndexesDropped(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	have := map[string]bool{}
	rows, err := idx.db.Query("SELECT name FROM sqlite_master WHERE type='index'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			have[n] = true
		}
	}
	rows.Close()

	for _, dead := range []string{"idx_entries_year", "idx_entries_duration", "idx_entries_favorite"} {
		if have[dead] {
			t.Errorf("dead index %s should have been dropped", dead)
		}
	}
	for _, want := range []string{"idx_entries_thumb", "idx_entries_yearexpr", "idx_entries_favorite_path"} {
		if !have[want] {
			t.Errorf("expected index %s to exist", want)
		}
	}

	var uv int
	_ = idx.db.QueryRow("PRAGMA user_version").Scan(&uv)
	if uv != 7 {
		t.Errorf("user_version = %d, want 7", uv)
	}
}

// TestPlanFavoritesCovering proves the v6 (favorite, path) index makes the
// favorites page and viewer neighbor probes stream in path order — no temp
// b-tree materializing and sorting every favorite to return one page/one row.
func TestPlanFavoritesCovering(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	v := View{Kind: "favorites"}
	where, args := v.whereClause()

	// ListPage's paginated favorites statement (paginate.go).
	pageArgs := append(append([]any{}, args...), 60, 0)
	pagePlan := explainPlan(t, idx,
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" ORDER BY path LIMIT ? OFFSET ?", pageArgs...)
	mustContain(t, "favorites-page", pagePlan, "SEARCH entries USING INDEX idx_entries_favorite_path")
	mustNotContain(t, "favorites-page", pagePlan, "TEMP B-TREE")

	// Neighbors' "prev" probe (paginate.go).
	prevArgs := append(append([]any{}, args...), filepath.Join("/lib", "000100.jpg"))
	prevPlan := explainPlan(t, idx,
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" AND path < ? ORDER BY path DESC LIMIT 1", prevArgs...)
	mustContain(t, "favorites-neighbor", prevPlan, "SEARCH entries USING INDEX idx_entries_favorite_path")
	mustNotContain(t, "favorites-neighbor", prevPlan, "TEMP B-TREE")
}

// TestPlanYearCovering proves the v6 (yearExpr, path) index makes the year
// gallery page and viewer neighbor probes stream in path order instead of
// sorting the whole year via a temp b-tree.
func TestPlanYearCovering(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	v := View{Kind: "year", Year: 2024}
	where, args := v.whereClause()

	pageArgs := append(append([]any{}, args...), 60, 0)
	pagePlan := explainPlan(t, idx,
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" ORDER BY path LIMIT ? OFFSET ?", pageArgs...)
	mustContain(t, "year-page", pagePlan, "SEARCH entries USING INDEX idx_entries_yearexpr")
	mustNotContain(t, "year-page", pagePlan, "TEMP B-TREE")

	prevArgs := append(append([]any{}, args...), filepath.Join("/lib", "000100.jpg"))
	prevPlan := explainPlan(t, idx,
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" AND path < ? ORDER BY path DESC LIMIT 1", prevArgs...)
	mustContain(t, "year-neighbor", prevPlan, "SEARCH entries USING INDEX idx_entries_yearexpr")
	mustNotContain(t, "year-neighbor", prevPlan, "TEMP B-TREE")
}

// TestDirViewFilePath preserves the pre-I-08 semantics of the dropped
// "OR path = ?" arm: when a *file* path is passed where a directory is
// expected (reachable via the web /dir?path= handler, which only checks
// withinRoot), the file itself is the single member of the view. This exercises
// the dirExactEntry fold-in across CountView, ListPage and Neighbors.
func TestDirViewFilePath(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 10)

	filePath := filepath.Join("/lib", "000003.jpg")
	v := View{Kind: "dir", Dir: filePath} // Dir is actually a file

	if got := idx.CountView(v); got != 1 {
		t.Errorf("CountView(file-as-dir) = %d, want 1 (the file itself)", got)
	}

	page := idx.ListPage(v, 0, 10)
	if len(page) != 1 || page[0].Path != filePath {
		t.Errorf("ListPage(file-as-dir) = %v, want [%s]", page, filePath)
	}

	prev, next, pos, total := idx.Neighbors(v, filePath)
	if prev != nil {
		t.Errorf("prev = %v, want nil (file is sole/first member)", prev)
	}
	if next != nil {
		t.Errorf("next = %v, want nil (file is sole/last member)", next)
	}
	if pos != 1 || total != 1 {
		t.Errorf("pos/total = %d/%d, want 1/1", pos, total)
	}
}

// TestDirViewExactPrependOrder proves the exact-dir row is prepended before the
// range in a synthetic case where both exist: an entry whose path equals the
// directory string sorts before that directory's children. We can't create such
// a pair on a real filesystem, but the index is path-keyed, so we insert both
// directly and confirm ordering + pagination fold-in.
func TestDirViewExactPrependOrder(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	// Children under "/lib/sub".
	seed(t, idx, "/lib/sub", 5)
	// A row whose path == the directory "/lib/sub" itself.
	if _, err := idx.db.Exec(
		"INSERT INTO entries (path, type, size, mtime, thumb_id, favorite, duration_ms) VALUES (?,?,?,?,?,0,0)",
		"/lib/sub", 1, 1, 0, ThumbIDFor("/lib/sub"),
	); err != nil {
		t.Fatal(err)
	}

	v := View{Kind: "dir", Dir: "/lib/sub"}
	if got := idx.CountView(v); got != 6 {
		t.Fatalf("CountView = %d, want 6 (exact + 5 children)", got)
	}

	full := idx.ListPage(v, 0, 0) // no limit
	if len(full) != 6 || full[0].Path != "/lib/sub" {
		t.Fatalf("ListPage full: len=%d first=%q, want 6 and /lib/sub first", len(full), full[0].Path)
	}

	// Pagination fold-in: page of 2 from offset 0 -> [exact, first child].
	p := idx.ListPage(v, 0, 2)
	if len(p) != 2 || p[0].Path != "/lib/sub" || filepath.Base(p[1].Path) != "000000.jpg" {
		t.Fatalf("ListPage(0,2) = %v, want [/lib/sub, .../000000.jpg]", pathsOf(p))
	}
	// Offset 1 skips the exact row -> starts at first child.
	p = idx.ListPage(v, 1, 2)
	if len(p) != 2 || filepath.Base(p[0].Path) != "000000.jpg" || filepath.Base(p[1].Path) != "000001.jpg" {
		t.Fatalf("ListPage(1,2) = %v, want children 0,1", pathsOf(p))
	}

	// Neighbors of the first child: prev should be the exact row.
	prev, _, pos, total := idx.Neighbors(v, filepath.Join("/lib/sub", "000000.jpg"))
	if prev == nil || prev.Path != "/lib/sub" {
		t.Errorf("prev of first child = %v, want /lib/sub", prev)
	}
	if pos != 2 || total != 6 {
		t.Errorf("pos/total = %d/%d, want 2/6", pos, total)
	}
	// Neighbors of the exact row itself: prev nil, next first child, pos 1.
	prev, next, pos, total := idx.Neighbors(v, "/lib/sub")
	if prev != nil {
		t.Errorf("prev of exact = %v, want nil", prev)
	}
	if next == nil || filepath.Base(next.Path) != "000000.jpg" {
		t.Errorf("next of exact = %v, want first child", next)
	}
	if pos != 1 || total != 6 {
		t.Errorf("pos/total = %d/%d, want 1/6", pos, total)
	}
}

func pathsOf(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Path
	}
	return out
}
