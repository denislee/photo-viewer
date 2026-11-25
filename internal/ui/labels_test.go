package ui

import "testing"

// TestToolbarItemsTextMemoized checks the G4-12 memo: the first call formats,
// repeat calls with the same count return the cached string without allocating,
// and a changed count reformats.
func TestToolbarItemsTextMemoized(t *testing.T) {
	var tb Toolbar
	if got := tb.itemsText(3); got != "3 items" {
		t.Fatalf("itemsText(3) = %q, want %q", got, "3 items")
	}
	if n := testing.AllocsPerRun(100, func() { _ = tb.itemsText(3) }); n != 0 {
		t.Errorf("itemsText allocations on memo hit = %v, want 0", n)
	}
	if got := tb.itemsText(4); got != "4 items" {
		t.Errorf("itemsText(4) = %q, want %q", got, "4 items")
	}
}

// TestSearchCandidateTitle checks the precomputed display title and lowercase
// match keys built by newSearchCandidate.
func TestSearchCandidateTitle(t *testing.T) {
	if got := newSearchCandidate("/lib/a", "a", true).title; got != "📁 a" {
		t.Errorf("dir title = %q, want %q", got, "📁 a")
	}
	c := newSearchCandidate("/lib/a/B.jpg", "a/B.jpg", false)
	if c.title != "  B.jpg" {
		t.Errorf("file title = %q, want %q", c.title, "  B.jpg")
	}
	if c.baseLower != "b.jpg" {
		t.Errorf("baseLower = %q, want %q", c.baseLower, "b.jpg")
	}
	if c.relLower != "a/b.jpg" {
		t.Errorf("relLower = %q, want %q", c.relLower, "a/b.jpg")
	}
}

// TestSidebarCountLabels checks that buildRows precomputes the per-row count
// label, and leaves it empty for rows without a count.
func TestSidebarCountLabels(t *testing.T) {
	s := NewSidebar()
	rows := s.buildRows("/lib", "/lib", []string{"/lib/x"}, map[string]int{"/lib/x": 7}, 1, false)

	found := false
	for _, r := range rows {
		switch {
		case r.path == "/lib/x":
			found = true
			if r.countLabel != "7" {
				t.Errorf("row %q countLabel = %q, want %q", r.path, r.countLabel, "7")
			}
		case r.countLabel != "":
			t.Errorf("row %q countLabel = %q, want empty", r.path, r.countLabel)
		}
	}
	if !found {
		t.Fatalf("no row with path %q in %d rows", "/lib/x", len(rows))
	}
}
