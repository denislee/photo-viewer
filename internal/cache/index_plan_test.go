package cache

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dns/photo-viewer/internal/scan"
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
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
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

// TestPlanNeighborsPosBounded proves every statement NeighborsWithTotal and
// listPageAfterQuery issue is a bounded index SEARCH: no full SCAN, no temp
// b-tree, and — for a dir view — both a lower and an upper bound on path in the
// same SEARCH, which is what keeps the statement O(log N)/O(pos) instead of a
// per-row filter over the whole subtree (C5-01).
//
// The statements are built through whereWithCursor, i.e. exactly the SQL those
// callers run. Asserting on whereClause() + a hand-appended "AND path < ?" (the
// old form) was vacuous for dir views: the `path<?` it found was the directory's
// own upper bound, so a degraded third path term went unnoticed.
func TestPlanNeighborsPosBounded(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	probe := filepath.Join("/lib", "000100.jpg")
	// The three bounds the neighbour/keyset callers pass (prev, next, pos/after).
	ops := []string{"<", ">", "<="}
	for _, tc := range []struct {
		label string
		v     View
	}{
		{"all", View{Kind: "all"}},
		{"favorites", View{Kind: "favorites"}},
		{"year", View{Kind: "year", Year: 2024}},
		{"dir", View{Kind: "dir", Dir: "/lib"}},
	} {
		for _, op := range ops {
			where, args := tc.v.whereWithCursor(op, probe)
			label := "pos-" + tc.label + "-" + op

			// The position statement issued by NeighborsWithTotal (paginate.go).
			plan := explainPlan(t, idx, "SELECT COUNT(*) FROM entries WHERE "+where, args...)
			mustContain(t, label, plan, "SEARCH entries USING")
			mustNotContain(t, label, plan, "SCAN entries")
			mustNotContain(t, label, plan, "TEMP B-TREE")

			// The prev/next statements issued by NeighborsWithTotal and the
			// page statement issued by listPageAfterQuery. Same WHERE, so the
			// plan must show the same bounded search for the ordered row reads.
			for _, tail := range []string{
				" ORDER BY path DESC LIMIT 1", // prev
				" ORDER BY path ASC LIMIT 1",  // next
				" ORDER BY path LIMIT 7",      // listPageAfterQuery
			} {
				plan = explainPlan(t, idx,
					"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
						where+tail, args...)
				mustContain(t, label, plan, "SEARCH entries USING")
				mustNotContain(t, label, plan, "SCAN entries")
				mustNotContain(t, label, plan, "TEMP B-TREE")
			}

			if tc.v.Kind != "dir" {
				continue
			}
			// C5-01: one lower AND one upper bound on path in a single SEARCH.
			// SQLite renders both `>` and `>=` as path>? and both `<` and `<=`
			// as path<?, so this is the plan's two-sided range, whichever
			// operators whereWithCursor picked.
			bounded := false
			for _, want := range []string{
				"(path>? AND path<?)",
				"(path>? AND path<=?)",
				"(path>=? AND path<?)",
				"(path>=? AND path<=?)",
			} {
				if strings.Contains(plan, want) {
					bounded = true
					break
				}
			}
			if !bounded {
				t.Errorf("%s: dir plan should bound path from both sides in one SEARCH\nplan:\n%s", label, plan)
			}
		}
	}
}

// TestWhereWithCursorDirHasOneBoundEach pins C5-01 at the SQL text level: for a
// dir view, whatever the cursor position relative to [Dir+"/", Dir+"0"), the
// rendered WHERE holds exactly one lower and one upper path bound. Two bounds is
// what SQLite's index range scan can use; a third would be evaluated per row and
// make the statement O(dir size).
//
// "path >" also matches "path >=", and "path <" also matches "path <=", so the
// counts below cover both operator spellings.
func TestWhereWithCursorDirHasOneBoundEach(t *testing.T) {
	v := View{Kind: "dir", Dir: "/lib/sub", Filter: "All", ShowRAW: true}
	lower, upper := dirRange("/lib/sub")
	if lower != "/lib/sub/" {
		t.Fatalf("dirRange lower = %q, want /lib/sub/", lower)
	}

	cursors := []struct {
		label  string
		cursor string
	}{
		{"exact-dir (before)", "/lib/sub"},
		{"before", "/lib/aaa.jpg"},
		{"at-lower", lower},
		{"inside", filepath.Join("/lib/sub", "000100.jpg")},
		{"at-upper", upper},
		{"after", upper + "/child.jpg"},
	}
	for _, op := range []string{"<", ">", "<="} {
		for _, c := range cursors {
			where, _ := v.whereWithCursor(op, c.cursor)
			label := fmt.Sprintf("dir op=%q cursor=%s", op, c.label)
			if got := strings.Count(where, "path >"); got != 1 {
				t.Errorf("%s: %d lower path bounds, want exactly 1\nwhere: %s",
					label, got, where)
			}
			if got := strings.Count(where, "path <"); got != 1 {
				t.Errorf("%s: %d upper path bounds, want exactly 1\nwhere: %s",
					label, got, where)
			}
		}
	}

	// Same guarantee under a type filter, whose fragment must trail the bounds.
	filtered := View{Kind: "dir", Dir: "/lib/sub", Filter: "Photos", ShowRAW: false}
	for _, op := range []string{"<", ">", "<="} {
		where, args := filtered.whereWithCursor(op, filepath.Join("/lib/sub", "000100.jpg"))
		if got := strings.Count(where, "path >"); got != 1 {
			t.Errorf("filtered op=%q: %d lower path bounds, want 1\nwhere: %s", op, got, where)
		}
		if got := strings.Count(where, "path <"); got != 1 {
			t.Errorf("filtered op=%q: %d upper path bounds, want 1\nwhere: %s", op, got, where)
		}
		// Two path bounds + the two type-filter args (RAW and video).
		if len(args) != 4 {
			t.Errorf("filtered op=%q: len(args) = %d, want 4\nwhere: %s", op, len(args), where)
		}
	}
}

// TestDeadIndexesDropped proves the migrations removed the write-only/superseded
// indexes and created the functional ones: v5 dropped idx_entries_year and
// idx_entries_duration; v6 replaced the single-column idx_entries_favorite with
// the covering idx_entries_favorite_path.
func TestDeadIndexesDropped(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	have := indexNames(t, idx)

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
	if uv != 8 {
		t.Errorf("user_version = %d, want 8", uv)
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

// TestPlanCountDirFiltered guards C-14: the sidebar's per-directory count
// (CountDirFiltered → CountView) must be a PK range SEARCH, not the old
// "(range) OR path = ?" MULTI-INDEX OR, under every filter combination.
func TestPlanCountDirFiltered(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	for _, filter := range []string{"All", "Photos", "Videos"} {
		for _, showRAW := range []bool{true, false} {
			v := View{Kind: "dir", Dir: "/lib", Filter: filter, ShowRAW: showRAW}
			where, args := v.whereClause()
			plan := explainPlan(t, idx, "SELECT COUNT(*) FROM entries WHERE "+where, args...)
			label := fmt.Sprintf("count %s raw=%v", filter, showRAW)
			mustContain(t, label, plan, "SEARCH entries USING")
			mustNotContain(t, label, plan, "SCAN entries")
			mustNotContain(t, label, plan, "MULTI-INDEX OR")
		}
	}
}

// TestCountDirFiltered pins CountDirFiltered's results now that it shares
// CountView: the type filters, a sibling directory sharing the name prefix
// ("/lib2") staying out of "/lib", and a file path passed as dir counting as 1.
func TestCountDirFiltered(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seedMixed(t, idx, "/lib/sub") // 2 photos, 2 videos, 1 RAW
	seedMixed(t, idx, "/lib2")

	for _, tc := range []struct {
		dir     string
		filter  string
		showRAW bool
		want    int
	}{
		{"/lib", "All", true, 5},
		{"/lib", "All", false, 4},
		{"/lib", "Photos", true, 3},
		{"/lib", "Photos", false, 2},
		{"/lib", "Videos", true, 2},
		{"/lib/sub", "All", true, 5},
		{"/lib/sub/b_video.mp4", "All", true, 1},
		{"/lib/sub/b_video.mp4", "Photos", true, 0},
		{"/nope", "All", true, 0},
	} {
		if got := idx.CountDirFiltered(tc.dir, tc.filter, tc.showRAW); got != tc.want {
			t.Errorf("CountDirFiltered(%q, %q, %v) = %d, want %d", tc.dir, tc.filter, tc.showRAW, got, tc.want)
		}
	}
	if got := idx.CountDirFiltered("/lib", "All", true); got != 5 {
		t.Errorf("CountDirFiltered(/lib) = %d, want 5", got)
	}
}

// TestPlanCountFavorites guards the favorites badge count: CountFavorites must
// stream off the covering (favorite, path) index under every filter combination
// rather than scanning the entries table.
//
// The query below is a copy of CountFavorites' SQL in index.go — keep it in
// sync if that statement changes.
func TestPlanCountFavorites(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	for _, tc := range []struct {
		filter  string
		showRAW bool
	}{
		{"All", true},
		{"Photos", false},
		{"Videos", true},
	} {
		where, args := typeFilterClause(tc.filter, tc.showRAW)
		plan := explainPlan(t, idx, "SELECT COUNT(*) FROM entries WHERE favorite = 1"+where, args...)
		label := fmt.Sprintf("count favorites %s raw=%v", tc.filter, tc.showRAW)
		mustContain(t, label, plan, "idx_entries_favorite_path")
		mustNotContain(t, label, plan, "SCAN entries")
	}
}

// TestPlanCountChildDirsFiltered guards the sidebar's per-child-directory count
// query: it must be a PK range SEARCH over the parent's subtree.
//
// The query below is a copy of CountChildDirsFiltered's SQL in index.go — keep
// it in sync if that statement changes.
func TestPlanCountChildDirsFiltered(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	parent := "/lib"
	lower, upper := dirRange(parent)
	startCol := len(lower) + 1
	where, args := typeFilterClause("All", true)
	q := `
		SELECT
			substr(path, ?, instr(substr(path, ?), '/') - 1) AS child,
			COUNT(*)
		FROM entries
		WHERE path >= ? AND path < ?
		  AND instr(substr(path, ?), '/') > 0` + where + `
		GROUP BY child`
	queryArgs := []any{startCol, startCol, lower, upper, startCol}
	queryArgs = append(queryArgs, args...)

	plan := explainPlan(t, idx, q, queryArgs...)
	mustContain(t, "child dirs", plan, "SEARCH entries")
	// The group-by b-tree is inherent to this query shape (the child name is a
	// computed expression, so no index can serve the GROUP BY). Asserted as
	// documentation: it is over the parent's subtree only, not the whole table.
	mustContain(t, "child dirs", plan, "TEMP B-TREE FOR GROUP BY")
}

// TestCountChildDirsFiltered pins the per-child-directory recursive counts and
// the type filters applied to them.
func TestCountChildDirsFiltered(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	idx.ReconcileBatch([]scan.Result{
		{Path: "/lib/a/1.jpg", Type: scan.TypePhoto, Size: 1},
		{Path: "/lib/a/b/2.jpg", Type: scan.TypePhoto, Size: 2},
		{Path: "/lib/c/3.mp4", Type: scan.TypeVideo, Size: 3},
		{Path: "/lib/top.jpg", Type: scan.TypePhoto, Size: 4},
	})

	for _, tc := range []struct {
		filter  string
		showRAW bool
		want    map[string]int
	}{
		{"All", true, map[string]int{"/lib/a": 2, "/lib/c": 1}},
		{"Photos", true, map[string]int{"/lib/a": 2}},
		{"Videos", true, map[string]int{"/lib/c": 1}},
	} {
		got := idx.CountChildDirsFiltered("/lib", tc.filter, tc.showRAW)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CountChildDirsFiltered(/lib, %q, %v) = %v, want %v", tc.filter, tc.showRAW, got, tc.want)
		}
	}
}
