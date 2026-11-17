package cache

import (
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestFindDuplicatesOrder pins FindDuplicates' ordering now that it happens in
// memory instead of via SQL's ORDER BY: groups come back in content-hash order,
// and each group is oldest-first with path as the tie-breaker.
func TestFindDuplicatesOrder(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	type row struct {
		path  string
		mtime int64
		hash  string
	}
	for _, r := range []row{
		{"/lib/z.jpg", 100, "hh"},
		{"/lib/a.jpg", 300, "hh"},
		{"/lib/m.jpg", 200, "hh"},
		{"/lib/b.jpg", 200, "hh"},
		{"/lib/g1.jpg", 50, "aa"},
		{"/lib/g2.jpg", 50, "aa"},
	} {
		res := idx.ReconcileBatch([]scan.Result{{
			Path:    r.path,
			Type:    scan.TypePhoto,
			Size:    10,
			ModTime: time.Unix(r.mtime, 0),
		}})
		if len(res) != 1 {
			t.Fatalf("ReconcileBatch(%s) = %d entries, want 1", r.path, len(res))
		}
		if _, err := idx.db.Exec("UPDATE entries SET content_hash = ? WHERE path = ?", r.hash, r.path); err != nil {
			t.Fatalf("set content_hash for %s: %v", r.path, err)
		}
	}

	groups := idx.FindDuplicates()
	if len(groups) != 2 {
		t.Fatalf("FindDuplicates returned %d groups, want 2: %+v", len(groups), groups)
	}

	if groups[0].Hash != "aa" {
		t.Errorf("groups[0].Hash = %q, want %q", groups[0].Hash, "aa")
	}
	if got, want := duplicatePathsOf(groups[0]), []string{"/lib/g1.jpg", "/lib/g2.jpg"}; !equalStrings(got, want) {
		t.Errorf("group aa paths = %v, want %v", got, want)
	}

	if groups[1].Hash != "hh" {
		t.Errorf("groups[1].Hash = %q, want %q", groups[1].Hash, "hh")
	}
	if got, want := duplicatePathsOf(groups[1]), []string{"/lib/z.jpg", "/lib/b.jpg", "/lib/m.jpg", "/lib/a.jpg"}; !equalStrings(got, want) {
		t.Errorf("group hh paths = %v, want %v", got, want)
	}
}

func duplicatePathsOf(g DuplicateGroup) []string {
	out := make([]string, len(g.Entries))
	for i, e := range g.Entries {
		out[i] = e.Path
	}
	return out
}

// TestPlanFindDuplicatesNoTempSort proves findDuplicatesSQL no longer makes
// SQLite materialize and sort the whole duplicate result set — the ordering is
// done in Go after the rows are grouped.
func TestPlanFindDuplicatesNoTempSort(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	mustNotContain(t, "FindDuplicates", explainPlan(t, idx, findDuplicatesSQL), "FOR ORDER BY")
}
