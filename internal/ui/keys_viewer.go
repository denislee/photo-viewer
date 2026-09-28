package ui

import (
	"gioui.org/app"
	"gioui.org/io/key"

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
			e := viewer.entries[viewer.Index]
			_, _, currentEntries, _ := ctrl.Snapshot()
			for i, cur := range currentEntries {
				if cur.Path == e.Path {
					grid.Selected = i
					grid.pendingScroll = true
					break
				}
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
			if p := viewer.Player(); p != nil {
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
			e := &viewer.entries[viewer.Index]
			e.Favorite = ctrl.ToggleFavorite(e.Path, e.Favorite)
			w.Invalidate()
		}
	case key.NameSpace:
		// Pause/resume the embedded player when on a video. Space on
		// anything else is a no-op so it doesn't accidentally pause a
		// player that isn't loaded.
		if isViewerVideo(viewer) {
			if p := viewer.Player(); p != nil {
				p.TogglePause()
				w.Invalidate()
			}
		}
	case "M":
		if isViewerVideo(viewer) {
			if p := viewer.Player(); p != nil {
				p.ToggleMute()
				w.Invalidate()
			}
		}
	case "]":
		if isViewerVideo(viewer) {
			if p := viewer.Player(); p != nil {
				p.SeekRelative(5)
				w.Invalidate()
			}
		}
	}
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
