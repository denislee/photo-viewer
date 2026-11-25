package ui

import (
	"strings"

	"gioui.org/io/key"
)

// handlePeopleKey routes keys for the People views before the grid sees
// them: the overview pane takes navigation; person/group grids add Ctrl+Z
// (undo the last people edit) and R (rename the person). Returns true when
// the key was consumed.
func handlePeopleKey(ke key.Event, ctrl *Controller, views *appViews, sidebarFocus *bool) bool {
	_, dir, _, _ := ctrl.Snapshot()
	if !isPeopleDir(dir) {
		return false
	}
	if dir == PeopleView {
		if views.people.Key(ke) {
			return true
		}
		// h/left at the first card hands focus to the sidebar, like the grid.
		if ke.Name == "H" || ke.Name == key.NameLeftArrow {
			*sidebarFocus = true
			return true
		}
		return false
	}
	switch {
	case ke.Name == "Z" && ke.Modifiers.Contain(key.ModCtrl):
		ctrl.UndoPeople()
		return true
	case ke.Name == "R" && strings.HasPrefix(dir, PersonViewPrefix):
		views.header.StartRename(dir)
		return true
	}
	return false
}
