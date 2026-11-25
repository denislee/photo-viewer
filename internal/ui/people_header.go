package ui

import (
	"fmt"
	"strings"
	"sync"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// Person header geometry, in dp. HeightDp tells the window how much vertical
// space to reserve above a person/group grid; the merge prompt needs an extra
// row.
const (
	personHeaderDp      = 56
	personHeaderMergeDp = 96
	groupSuggestionMax  = 5
)

// PersonHeader is the strip above a person's or unnamed group's photo grid: the
// editable name, the review / merge / hide actions, and, for a group, the
// always-visible "name this person" editor with fuzzy chips of existing names.
//
// The header is not a modal. While it has a focused editor (or the merge picker
// is open) the window asks Editing() and routes printable keys here instead of
// the app-wide hotkeys; the picker itself is drawn last, over the whole window,
// with LayoutPicker.
type PersonHeader struct {
	ctrl       *Controller
	openReview func(ReviewTarget)

	nameEditor  widget.Editor
	editorID    int64
	editorOpen  bool
	editorFocus bool

	groupEditor widget.Editor
	groupID     int64
	groupFocus  bool
	// blur asks the next Layout to drop keyboard focus (Escape in the person
	// rename editor or the group editor).
	blur bool

	picker     personPicker
	pickerSelf int64

	// mu guards the merge-prompt state, which the RenamePerson callback writes
	// from the people-op goroutine. mergeFor scopes the prompt to the person
	// it came from, so navigating to someone else cannot merge the wrong pair.
	mu        sync.Mutex
	mergeFor  int64
	mergeID   int64
	mergeName string

	// name is cached per person id + PeopleVersion so a frame never hits the
	// database for a name that hasn't changed.
	nameID     int64
	nameVer    uint64
	name       string
	nameCached bool

	nameBtn        widget.Clickable
	reviewBtn      widget.Clickable
	mergeBtn       widget.Clickable
	cancelMergeBtn widget.Clickable
	pickerBtn      widget.Clickable
	hideBtn        widget.Clickable
	ignoreBtn      widget.Clickable

	chips []*widget.Clickable
}

// NewPersonHeader wires the header to the controller and to the window's
// review-overlay opener.
func NewPersonHeader(ctrl *Controller, openReview func(ReviewTarget)) *PersonHeader {
	h := &PersonHeader{ctrl: ctrl, openReview: openReview}
	h.nameEditor.SingleLine = true
	h.nameEditor.Submit = true
	h.groupEditor.SingleLine = true
	h.groupEditor.Submit = true
	return h
}

// HeightDp is the strip height for dir: 0 for ordinary directories, 56 for a
// person/group sentinel, 96 while the merge prompt is showing.
func (h *PersonHeader) HeightDp(dir string) int {
	if id, ok := parseSentinelID(dir, PersonViewPrefix); ok {
		if _, _, pending := h.mergeState(id); pending {
			return personHeaderMergeDp
		}
		return personHeaderDp
	}
	if _, ok := parseSentinelID(dir, GroupViewPrefix); ok {
		return personHeaderDp
	}
	return 0
}

// mergeState returns the stored merge target if the prompt belongs to person
// id.
func (h *PersonHeader) mergeState(id int64) (int64, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mergeID == 0 || h.mergeFor != id {
		return 0, "", false
	}
	return h.mergeID, h.mergeName, true
}

// clearMerge dismisses the merge prompt.
func (h *PersonHeader) clearMerge() {
	h.mu.Lock()
	h.mergeFor, h.mergeID, h.mergeName = 0, 0, ""
	h.mu.Unlock()
}

// setMergeForTest seeds the merge prompt without a rename collision. Unit
// tests use it for HeightDp.
func (h *PersonHeader) setMergeForTest(forID, otherID int64, otherName string) {
	h.mu.Lock()
	h.mergeFor, h.mergeID, h.mergeName = forID, otherID, otherName
	h.mu.Unlock()
}

// Editing reports whether a text editor (or the picker) currently owns the
// keyboard, so the window must route printable keys here.
func (h *PersonHeader) Editing() bool {
	return h.editorOpen || h.picker.Open || h.groupFocus
}

// PickerOpen reports whether the merge picker is showing.
func (h *PersonHeader) PickerOpen() bool { return h.picker.Open }

// StartRename opens the person's inline name editor prefilled with the current
// name.
func (h *PersonHeader) StartRename(dir string) {
	id, ok := parseSentinelID(dir, PersonViewPrefix)
	if !ok {
		return
	}
	h.beginRename(id, h.cachedName(id))
}

// beginRename focuses the inline editor on person id with name prefilled.
func (h *PersonHeader) beginRename(id int64, name string) {
	h.editorID = id
	h.editorOpen = true
	h.editorFocus = true
	h.nameEditor.SetText(name)
}

// Key handles one key press while Editing() is true. The picker gets every key
// while it is open; otherwise Escape cancels the inline editor. It reports
// whether the key was consumed.
func (h *PersonHeader) Key(ke key.Event) bool {
	if !h.Editing() {
		return false
	}
	if h.picker.Open {
		h.picker.Key(ke, h.pickerPeople())
		return true
	}
	if ke.Name == key.NameEscape {
		h.editorOpen = false
		h.groupFocus = false
		h.blur = true
		return true
	}
	return false
}

// Layout draws the strip for dir. photos is the grid's entry count, shown as a
// muted "N photos".
func (h *PersonHeader) Layout(gtx layout.Context, th *Theme, dir string, photos int) layout.Dimensions {
	if id, ok := parseSentinelID(dir, PersonViewPrefix); ok {
		return h.layoutPerson(gtx, th, id, photos)
	}
	if id, ok := parseSentinelID(dir, GroupViewPrefix); ok {
		return h.layoutGroup(gtx, th, id, photos)
	}
	return layout.Dimensions{}
}

// cachedName returns person id's name, re-reading it only when the id or the
// people version moved.
func (h *PersonHeader) cachedName(id int64) string {
	if h.ctrl == nil || h.ctrl.Index() == nil {
		return h.name
	}
	ver := h.ctrl.PeopleVersion()
	if h.nameID == id && h.nameVer == ver && h.nameCached {
		return h.name
	}
	h.nameID, h.nameVer = id, ver
	h.name, h.nameCached = "", false
	if n, ok := h.ctrl.Index().PersonName(id); ok {
		h.name, h.nameCached = n, true
	}
	return h.name
}

// personHidden reads the person's hidden flag from the People snapshot, which
// is already loaded off the UI goroutine.
func (h *PersonHeader) personHidden(id int64) bool {
	if h.ctrl == nil {
		return false
	}
	snap, _ := h.ctrl.People()
	for _, p := range snap.Hidden {
		if p.ID == id {
			return true
		}
	}
	return false
}

// allPeople returns visible and hidden people for the group name chips.
func (h *PersonHeader) allPeople() []cache.Person {
	if h.ctrl == nil {
		return nil
	}
	snap, _ := h.ctrl.People()
	out := make([]cache.Person, 0, len(snap.People)+len(snap.Hidden))
	out = append(out, snap.People...)
	out = append(out, snap.Hidden...)
	return out
}

// pickerPeople is the merge picker's list: every person except the one being
// merged (and renamed).
func (h *PersonHeader) pickerPeople() []cache.Person {
	if h.ctrl == nil {
		return nil
	}
	snap, _ := h.ctrl.People()
	out := make([]cache.Person, 0, len(snap.People)+len(snap.Hidden))
	for _, p := range snap.People {
		if p.ID != h.pickerSelf {
			out = append(out, p)
		}
	}
	for _, p := range snap.Hidden {
		if p.ID != h.pickerSelf {
			out = append(out, p)
		}
	}
	return out
}

// rename asks the controller to rename id, remembering a name collision so the
// merge prompt can appear once the background op reports it.
func (h *PersonHeader) rename(id int64, name string) {
	if h.ctrl == nil {
		return
	}
	h.ctrl.RenamePerson(id, name, func(otherID int64, otherName string) {
		h.mu.Lock()
		h.mergeFor, h.mergeID, h.mergeName = id, otherID, otherName
		h.mu.Unlock()
		// The rename op's own invalidate has already run; bumping again wakes
		// the frame loop for the newly stored merge prompt.
		h.ctrl.bumpPeople()
	})
}

// submitRename closes the inline editor and renames when the text actually
// changed.
func (h *PersonHeader) submitRename(gtx layout.Context, id int64, current string) {
	name := strings.TrimSpace(h.nameEditor.Text())
	h.editorOpen = false
	gtx.Execute(key.FocusCmd{})
	if name == "" || name == current || h.ctrl == nil {
		return
	}
	h.rename(id, name)
}

// confirmMerge folds this person into the remembered target.
func (h *PersonHeader) confirmMerge(id int64) {
	other, _, pending := h.mergeState(id)
	h.clearMerge()
	if pending && other > 0 && h.ctrl != nil {
		h.ctrl.MergePeople(id, other)
	}
}

// openPicker shows the person picker for a merge; its "+ New person" row
// renames this person instead.
func (h *PersonHeader) openPicker(id int64) {
	h.pickerSelf = id
	h.picker.Show(func(personID int64, newName string) {
		if h.ctrl == nil {
			return
		}
		if personID > 0 {
			h.ctrl.MergePeople(id, personID)
			return
		}
		if name := strings.TrimSpace(newName); name != "" {
			h.rename(id, name)
		}
	})
}

// layoutPerson draws the named-person strip.
func (h *PersonHeader) layoutPerson(gtx layout.Context, th *Theme, id int64, photos int) layout.Dimensions {
	name := h.cachedName(id)
	hidden := h.personHidden(id)
	if h.editorOpen && h.editorID != id {
		h.editorOpen = false
	}
	if h.blur {
		gtx.Execute(key.FocusCmd{})
		h.blur = false
	}

	if h.nameBtn.Clicked(gtx) && !h.editorOpen {
		h.beginRename(id, name)
	}
	if h.reviewBtn.Clicked(gtx) && h.openReview != nil {
		h.openReview(ReviewTarget{PersonID: id, Title: name})
	}
	if h.pickerBtn.Clicked(gtx) {
		h.openPicker(id)
	}
	if h.hideBtn.Clicked(gtx) && h.ctrl != nil {
		h.ctrl.HidePerson(id, !hidden)
	}
	if h.mergeBtn.Clicked(gtx) {
		h.confirmMerge(id)
	}
	if h.cancelMergeBtn.Clicked(gtx) {
		h.clearMerge()
	}

	if h.editorOpen {
		if h.editorFocus {
			gtx.Execute(key.FocusCmd{Tag: &h.nameEditor})
			h.editorFocus = false
		}
		for {
			evt, ok := h.nameEditor.Update(gtx)
			if !ok {
				break
			}
			if _, ok := evt.(widget.SubmitEvent); ok {
				h.submitRename(gtx, id, name)
			}
		}
	}

	hideText := "Hide"
	if hidden {
		hideText = "Unhide"
	}

	main := func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if h.editorOpen {
							return h.layoutNameEditor(gtx, th)
						}
						return h.nameBtn.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Label(th.Theme, unit.Sp(16), name)
							lbl.Color = th.Foreground
							lbl.Font.Weight = 700
							lbl.MaxLines = 1
							return lbl.Layout(gtx)
						})
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(th.Theme, unit.Sp(12), photoCountLabel(photos))
						lbl.Color = th.Muted
						lbl.MaxLines = 1
						return lbl.Layout(gtx)
					}),
					layout.Flexed(1, layout.Spacer{}.Layout),
					layout.Rigid(material.Button(th.Theme, &h.reviewBtn, "Review faces").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &h.pickerBtn, "Merge into…").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(material.Button(th.Theme, &h.hideBtn, hideText).Layout),
				)
			})
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(main),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return h.layoutMergeRow(gtx, th, id)
		}),
	)
}

// layoutNameEditor draws the inline, single-line rename editor at a fixed
// width so the action buttons keep their place.
func (h *PersonHeader) layoutNameEditor(gtx layout.Context, th *Theme) layout.Dimensions {
	w := gtx.Dp(unit.Dp(240))
	gtx.Constraints.Min.X = w
	gtx.Constraints.Max.X = w
	ed := material.Editor(th.Theme, &h.nameEditor, "Person name")
	ed.Color = th.Foreground
	ed.HintColor = th.Muted
	ed.TextSize = unit.Sp(15)
	return drawEditorBox(gtx, th.CellBG,
		layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)},
		ed.Layout)
}

// layoutMergeRow draws the "name already exists" prompt under the person row.
func (h *PersonHeader) layoutMergeRow(gtx layout.Context, th *Theme, id int64) layout.Dimensions {
	_, otherName, pending := h.mergeState(id)
	if !pending {
		return layout.Dimensions{}
	}
	text := fmt.Sprintf("A person named %s already exists. Merge into %s?", otherName, otherName)
	return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(6), Left: unit.Dp(12), Right: unit.Dp(12)}.
		Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(13), text)
					lbl.Color = th.Foreground
					lbl.Font.Weight = 600
					lbl.MaxLines = 1
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
				layout.Rigid(material.Button(th.Theme, &h.mergeBtn, "Merge").Layout),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(material.Button(th.Theme, &h.cancelMergeBtn, "Cancel").Layout),
			)
		})
}

// ensureChips sizes the group suggestion clickables.
func (h *PersonHeader) ensureChips(n int) {
	if cap(h.chips) < n {
		grown := make([]*widget.Clickable, n, max(n+4, 8))
		copy(grown, h.chips)
		h.chips = grown
	} else {
		h.chips = h.chips[:n]
	}
	for i := range h.chips {
		if h.chips[i] == nil {
			h.chips[i] = &widget.Clickable{}
		}
	}
}

// submitGroup names the unnamed group after the editor's current text.
func (h *PersonHeader) submitGroup(id int64) {
	name := strings.TrimSpace(h.groupEditor.Text())
	if name == "" || h.ctrl == nil {
		return
	}
	h.ctrl.NameGroup(id, name)
}

// layoutGroup draws the unnamed-group strip: the "Who is this?" editor with
// fuzzy chips plus the review / ignore actions.
func (h *PersonHeader) layoutGroup(gtx layout.Context, th *Theme, id int64, photos int) layout.Dimensions {
	// A different group means the in-progress name belongs to someone else.
	if h.groupID != id {
		h.groupID = id
		h.groupEditor.SetText("")
		h.groupFocus = false
	}
	for {
		evt, ok := h.groupEditor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := evt.(widget.SubmitEvent); ok {
			h.submitGroup(id)
		}
	}
	blurred := false
	if h.blur {
		gtx.Execute(key.FocusCmd{})
		h.blur = false
		blurred = true
	}
	h.groupFocus = !blurred && gtx.Focused(&h.groupEditor)

	if h.reviewBtn.Clicked(gtx) && h.openReview != nil {
		h.openReview(ReviewTarget{ClusterID: id, Title: "Unnamed group"})
	}
	if h.ignoreBtn.Clicked(gtx) && h.ctrl != nil {
		h.ctrl.IgnoreGroup(id)
	}

	chips := groupSuggestions(h.allPeople(), h.groupEditor.Text())
	h.ensureChips(len(chips))
	for i, name := range chips {
		if h.chips[i].Clicked(gtx) {
			h.groupEditor.SetText(name)
			h.groupEditor.SetCaret(len(name), len(name))
			h.submitGroup(id)
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(10), Left: unit.Dp(12), Right: unit.Dp(12)}.
				Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Label(th.Theme, unit.Sp(15), "Who is this?")
							lbl.Color = th.Foreground
							lbl.Font.Weight = 700
							return lbl.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							ed := material.Editor(th.Theme, &h.groupEditor, "Name this person")
							ed.Color = th.Foreground
							ed.HintColor = th.Muted
							ed.TextSize = unit.Sp(14)
							return drawEditorBox(gtx, th.CellBG,
								layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)},
								ed.Layout)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lbl := material.Label(th.Theme, unit.Sp(12), photoCountLabel(photos))
							lbl.Color = th.Muted
							lbl.MaxLines = 1
							return lbl.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(material.Button(th.Theme, &h.reviewBtn, "Review faces").Layout),
						layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
						layout.Rigid(material.Button(th.Theme, &h.ignoreBtn, "Not a person I care about").Layout),
					)
				})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if len(chips) == 0 {
				return layout.Dimensions{}
			}
			kids := make([]layout.FlexChild, 0, len(chips)*2)
			for i := range chips {
				if i > 0 {
					kids = append(kids, layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout))
				}
				kids = append(kids, layout.Rigid(material.Button(th.Theme, h.chips[i], chips[i]).Layout))
			}
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(8), Left: unit.Dp(12), Right: unit.Dp(12)}.
				Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, kids...)
				})
		}),
	)
}

// LayoutPicker draws the merge picker over the whole window when it is open.
// The window calls it last, after the body and the header.
func (h *PersonHeader) LayoutPicker(gtx layout.Context, th *Theme) layout.Dimensions {
	if !h.picker.Open {
		return layout.Dimensions{}
	}
	return h.picker.Layout(gtx, th, h.pickerPeople(), "Merge into…")
}
