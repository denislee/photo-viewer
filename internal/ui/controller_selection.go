package ui

import (
	"maps"
)

// ToggleSelection adds or removes path from the selected set.
func (c *Controller) ToggleSelection(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selectedPaths[path] {
		delete(c.selectedPaths, path)
	} else {
		c.selectedPaths[path] = true
	}
}

// Select adds path to the selected set.
func (c *Controller) Select(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectedPaths[path] = true
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

// SnapshotSelected returns a copy of the current selected-path set. Callers
// that need to check multiple paths per frame (e.g. the grid row renderer)
// should snapshot once and iterate the copy rather than calling IsSelected
// per entry, which would acquire the mutex once per cell per frame.
func (c *Controller) SnapshotSelected() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := make(map[string]bool, len(c.selectedPaths))
	maps.Copy(snap, c.selectedPaths)
	return snap
}
