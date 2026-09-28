package ui

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestSelectionModeLocked is the G-13 guard: selection mode is only reached
// through locked accessors, so toggling it while another goroutine reads it
// is race-free (meaningful under -race), and ClearSelection exits the mode.
func TestSelectionModeLocked(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, filepath.Join(dir, "lib"), filepath.Join(dir, "cache"))

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 1000 {
			c.SetSelectionMode(i%2 == 0)
		}
	})
	wg.Go(func() {
		for range 1000 {
			_ = c.SelectionMode()
		}
	})
	wg.Wait()

	c.SetSelectionMode(true)
	c.Select("/lib/a.jpg")
	if !c.SelectionMode() || !c.IsSelected("/lib/a.jpg") {
		t.Fatal("selection mode / selected path not recorded")
	}
	c.ClearSelection()
	if c.SelectionMode() || c.IsSelected("/lib/a.jpg") {
		t.Error("ClearSelection left selection mode or a selected path behind")
	}
}
