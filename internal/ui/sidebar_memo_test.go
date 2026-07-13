package ui

import (
	"slices"
	"testing"
)

// subdirPaths pulls the row paths out of a buildRows result for comparison,
// skipping the synthetic Favorites/Trash sentinels the builder always prepends
// so the assertion focuses on the root + subdir rows.
func subdirPaths(rows []sidebarRow) []string {
	var out []string
	for _, r := range rows {
		switch r.path {
		case FavoritesView, TrashView:
			continue
		}
		out = append(out, r.path)
	}
	return out
}

// TestBuildRowsMemoExactSubdirs guards G-09: the buildRows memo must key on the
// full subdirs slice, not just len+first. A mid-scan rename that swaps a
// non-first subdir for an equal-count set keeps len and subdirs[0] identical, so
// the old heuristic served stale rows. Same root/treeDir/countsVer/groupByYear —
// only a middle element differs — so nothing else invalidates the memo.
func TestBuildRowsMemoExactSubdirs(t *testing.T) {
	s := NewSidebar()

	root, treeDir := "/lib", "/lib"
	first := []string{"/lib/a", "/lib/b", "/lib/c"}
	// Prime the memo with the first input.
	got1 := subdirPaths(s.buildRows(root, treeDir, first, nil, 1, false))
	if want := []string{"/lib", "/lib/a", "/lib/b", "/lib/c"}; !slices.Equal(got1, want) {
		t.Fatalf("first buildRows = %v, want %v", got1, want)
	}

	// Same length, same first element, same everything else — only the middle
	// element changes (a rename of /lib/b → /lib/x). The memo must NOT serve
	// the cached rows.
	second := []string{"/lib/a", "/lib/x", "/lib/c"}
	got2 := subdirPaths(s.buildRows(root, treeDir, second, nil, 1, false))
	if want := []string{"/lib", "/lib/a", "/lib/x", "/lib/c"}; !slices.Equal(got2, want) {
		t.Fatalf("second buildRows = %v, want %v (stale memo served the first input)", got2, want)
	}
}

// TestBuildRowsMemoHitOnIdenticalSubdirs confirms the memo still fires when the
// input is genuinely unchanged (same values, different backing array): the
// second call must return the identical cached slice, proving the stored clone
// compares equal rather than always missing.
func TestBuildRowsMemoHitOnIdenticalSubdirs(t *testing.T) {
	s := NewSidebar()
	root, treeDir := "/lib", "/lib"

	rows1 := s.buildRows(root, treeDir, []string{"/lib/a", "/lib/b"}, nil, 1, false)
	// Fresh slice with identical contents — should hit the memo.
	rows2 := s.buildRows(root, treeDir, []string{"/lib/a", "/lib/b"}, nil, 1, false)
	if &rows1[0] != &rows2[0] {
		t.Fatalf("memo missed on identical subdirs: got a rebuilt slice, want the cached one")
	}
}

// TestBuildRowsMemoNilSubdirs makes sure the nil/empty subdirs case still works:
// priming with nil, repeating nil hits the memo, then a real slice rebuilds.
func TestBuildRowsMemoNilSubdirs(t *testing.T) {
	s := NewSidebar()
	root, treeDir := "/lib", "/lib"

	rowsNil1 := s.buildRows(root, treeDir, nil, nil, 1, false)
	if got := subdirPaths(rowsNil1); !slices.Equal(got, []string{"/lib"}) {
		t.Fatalf("nil subdirs rows = %v, want [/lib]", got)
	}
	// nil again — memo hit.
	rowsNil2 := s.buildRows(root, treeDir, nil, nil, 1, false)
	if &rowsNil1[0] != &rowsNil2[0] {
		t.Fatalf("memo missed on repeated nil subdirs")
	}
	// Now a real subdir — must rebuild.
	got := subdirPaths(s.buildRows(root, treeDir, []string{"/lib/a"}, nil, 1, false))
	if !slices.Equal(got, []string{"/lib", "/lib/a"}) {
		t.Fatalf("after nil, real subdir rows = %v, want [/lib /lib/a]", got)
	}
}
