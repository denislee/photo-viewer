package cache

import (
	"database/sql"
	"errors"
	"testing"
)

// TestToggleFavoriteFlipsPersistedFlag is the U-07 single-statement guard.
// Index.ToggleFavorite replaces the old IsFavorite (SELECT) + SetFavorite
// (UPDATE) pair with one `UPDATE ... RETURNING`, so this asserts the returned
// value and the persisted flag agree on every flip and that CountFavorites
// tracks the change — proving the RETURNING round-trip both writes and reports
// the new state.
func TestToggleFavoriteFlipsPersistedFlag(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	root := "/lib"
	entries := seed(t, idx, root, 3)
	path := entries[0].Path

	if idx.IsFavorite(path) {
		t.Fatalf("freshly seeded entry should not be a favorite")
	}

	// First toggle: false -> true.
	got, err := idx.ToggleFavorite(path)
	if err != nil {
		t.Fatalf("ToggleFavorite: %v", err)
	}
	if !got {
		t.Fatalf("first toggle returned %v, want true", got)
	}
	if !idx.IsFavorite(path) {
		t.Fatalf("favorite flag not persisted after first toggle")
	}
	if n := idx.CountFavorites("All", true); n != 1 {
		t.Fatalf("CountFavorites = %d after favoriting, want 1", n)
	}

	// Second toggle: true -> false.
	got, err = idx.ToggleFavorite(path)
	if err != nil {
		t.Fatalf("ToggleFavorite: %v", err)
	}
	if got {
		t.Fatalf("second toggle returned %v, want false", got)
	}
	if idx.IsFavorite(path) {
		t.Fatalf("favorite flag still set after second toggle")
	}
	if n := idx.CountFavorites("All", true); n != 0 {
		t.Fatalf("CountFavorites = %d after un-favoriting, want 0", n)
	}

	// Toggling one row must not touch its neighbours.
	for _, e := range entries[1:] {
		if idx.IsFavorite(e.Path) {
			t.Fatalf("neighbour %s became a favorite", e.Path)
		}
	}
}

// TestToggleFavoriteMissingPath asserts a toggle on a path with no entries row
// updates nothing and surfaces sql.ErrNoRows (RETURNING yields no row), so the
// controller can treat it as "nothing flipped" and skip the in-memory patch.
func TestToggleFavoriteMissingPath(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	_, err := idx.ToggleFavorite("/lib/does-not-exist.jpg")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ToggleFavorite on missing path err = %v, want sql.ErrNoRows", err)
	}
}
