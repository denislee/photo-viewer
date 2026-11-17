package ui

import (
	"maps"

	"github.com/dns/photo-viewer/internal/cache"
)

// The selected-path set is copy-on-write (G4-02): every mutation publishes a
// fresh map under c.mu, and a published map is never written again. Readers
// (the grid, once per frame) take the current map without copying it, so a
// 5k-path selection no longer costs a 218 KB copy per frame. Mutations are
// keypresses and clicks, so the clone they pay is cheap by comparison. Same
// discipline as patchFavorite / dirCounts.

// ToggleSelection adds or removes path from the selected set.
func (c *Controller) ToggleSelection(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := maps.Clone(c.selectedPaths)
	if next == nil {
		next = make(map[string]bool, 1)
	}
	if next[path] {
		delete(next, path)
	} else {
		next[path] = true
	}
	c.selectedPaths = next
}

// Select adds path to the selected set.
func (c *Controller) Select(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selectedPaths[path] {
		return // already selected: keep the published map
	}
	next := maps.Clone(c.selectedPaths)
	if next == nil {
		next = make(map[string]bool, 1)
	}
	next[path] = true
	c.selectedPaths = next
}

// SelectionMode reports whether multi-select mode is on.
func (c *Controller) SelectionMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectionMode
}

// SetSelectionMode turns multi-select mode on or off. Turning it off does not
// clear the selected set; ClearSelection does both.
func (c *Controller) SetSelectionMode(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectionMode = on
}

// ClearSelection empties the selected set and exits selection mode.
func (c *Controller) ClearSelection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectionMode = false
	c.selectedPaths = make(map[string]bool)
}

// IsSelected returns whether path is in the selected set.
func (c *Controller) IsSelected(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedPaths[path]
}

// SnapshotSelected returns the current selected-path set. The map is the
// published copy-on-write snapshot, shared with the controller: callers must
// treat it as read-only. It never changes after being returned, so callers
// that check many paths per frame (the grid row renderer) can read it without
// the lock and without paying for a copy.
func (c *Controller) SnapshotSelected() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedPaths
}

// SelectionState returns selection mode and the selected-path snapshot under
// one lock, for the per-frame grid layout.
func (c *Controller) SelectionState() (mode bool, selected map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectionMode, c.selectedPaths
}

// SelectedEntries returns the entries of entries that are selected, in order.
// It reads one selection snapshot instead of locking once per entry (G4-08).
func (c *Controller) SelectedEntries(entries []cache.Entry) []cache.Entry {
	sel := c.SnapshotSelected()
	if len(sel) == 0 {
		return nil
	}
	var out []cache.Entry
	for _, e := range entries {
		if sel[e.Path] {
			out = append(out, e)
		}
	}
	return out
}
