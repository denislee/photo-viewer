package ui

import (
	"gioui.org/app"
	"gioui.org/io/key"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

func handleViewerKey(ke key.Event, viewer *Viewer, grid *Grid, ctrl *Controller, w *app.Window) {
	// While the delete-confirmation modal is up, only Enter / Esc / Ctrl+[
	// are meaningful — every other key is a no-op so a stray hjkl can't
	// silently advance past a confirmation the user hasn't dismissed.
	if viewer.Confirming {
		switch ke.Name {
		case key.NameReturn:
			viewer.ConfirmDelete(ctrl.DeletePath)
			w.Invalidate()
		case key.NameEscape:
			viewer.CancelDelete()
			w.Invalidate()
		case "[":
			if ke.Modifiers.Contain(key.ModCtrl) {
				viewer.CancelDelete()
				w.Invalidate()
			}
		}
		return
	}

	syncGrid := func() {
		if viewer.Index >= 0 && viewer.Index < len(viewer.entries) {
			_, _, currentEntries, _ := ctrl.Snapshot()
			if i := gridIndexFor(currentEntries, viewer.Index, viewer.entries[viewer.Index].Path); i >= 0 {
				grid.Selected = i
				grid.pendingScroll = true
			}
		}
	}

	switch ke.Name {
	case key.NameEscape, "Q":
		syncGrid()
		viewer.Close()
		w.Invalidate()
	case "[":
		if ke.Modifiers.Contain(key.ModCtrl) {
			syncGrid()
			viewer.Close()
			w.Invalidate()
		} else if isViewerVideo(viewer) {
			if p := viewer.player.Load(); p != nil {
				p.SeekRelative(-5)
				w.Invalidate()
			}
		}
	case key.NameLeftArrow, "H":
		viewer.Prev()
		w.Invalidate()
	case key.NameRightArrow, "L":
		viewer.Next()
		w.Invalidate()
	case key.NameDownArrow, "J":
		viewer.Next()
		w.Invalidate()
	case key.NameUpArrow, "K":
		viewer.Prev()
		w.Invalidate()
	case "D":
		if !ke.Modifiers.Contain(key.ModCtrl) {
			viewer.RequestDelete()
			w.Invalidate()
		}
	case "I":
		viewer.ShowInfo = !viewer.ShowInfo
		w.Invalidate()
	case "O", "o":
		if viewer.Index >= 0 && viewer.Index < len(viewer.entries) {
			openExternally(viewer.entries[viewer.Index : viewer.Index+1])
		}
	case "F":
		if viewer.Index >= 0 && viewer.Index < len(viewer.entries) {
			// Write through the viewer's private copy, never the slice it
			// shares with the controller and grid (G4-03).
			e := &viewer.mutableEntries()[viewer.Index]
			e.Favorite = ctrl.ToggleFavorite(e.Path, e.Favorite)
			w.Invalidate()
		}
	case key.NameSpace:
		// Pause/resume the embedded player when on a video. Space on
		// anything else is a no-op so it doesn't accidentally pause a
		// player that isn't loaded.
		if isViewerVideo(viewer) {
			if p := viewer.player.Load(); p != nil {
				p.TogglePause()
				w.Invalidate()
			}
		}
	case "M":
		if isViewerVideo(viewer) {
			if p := viewer.player.Load(); p != nil {
				p.ToggleMute()
				w.Invalidate()
			}
		}
	case "]":
		if isViewerVideo(viewer) {
			if p := viewer.player.Load(); p != nil {
				p.SeekRelative(5)
				w.Invalidate()
			}
		}
	}
}

// gridIndexFor returns the index of path in entries, or -1. hint is where it
// probably is: the viewer's index, which matches the grid's whenever nothing
// changed the list while the viewer was open, so the common close costs one
// comparison instead of an O(N) scan (G3-04).
func gridIndexFor(entries []cache.Entry, hint int, path string) int {
	if hint >= 0 && hint < len(entries) && entries[hint].Path == path {
		return hint
	}
	for i := range entries {
		if entries[i].Path == path {
			return i
		}
	}
	return -1
}

// isViewerVideo reports whether the viewer's current entry is a video.
// Hoisted so the video-control key cases don't have to repeat the index /
// type checks.
func isViewerVideo(viewer *Viewer) bool {
	if viewer.Index < 0 || viewer.Index >= len(viewer.entries) {
		return false
	}
	return viewer.entries[viewer.Index].Type == scan.TypeVideo
}
