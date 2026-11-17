package ui

// IsSelected is a test helper; production code reads SnapshotSelected or
// SelectedEntries.
func (c *Controller) IsSelected(path string) bool { return c.SnapshotSelected()[path] }
