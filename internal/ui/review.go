package ui

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"path/filepath"
	"sync"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// Review grid geometry, in dp. 96dp avatars keep a face readable while still
// fitting most of a group's outliers on one screen.
const (
	reviewCellDp = 96
	reviewGapDp  = 8
)

// ReviewTarget says what a ReviewView is reviewing:
//
//   - PersonID > 0 && !Suggest: that person's faces (outlier first);
//   - ClusterID > 0: one unnamed group (outlier first);
//   - PersonID > 0 && Suggest: the unattributed faces suggested for that
//     person, i.e. "might be <Title>".
//
// Title is the name shown in the header (the person, or "Suggestions").
type ReviewTarget struct {
	PersonID  int64
	ClusterID int64
	Suggest   bool
	Title     string
}

// ReviewView is the full-window "review faces" overlay: a grid of face
// avatars, farthest from the group centroid first, with keyboard verdicts
// (confirm / not this person / move / ignore / cover / open photo).
//
// Face loading happens off the UI goroutine and is republished whenever the
// controller's people version moves; the cursor and the mark set are pruned
// to whatever still exists after a reload.
type ReviewView struct {
	Open bool

	ctrl      *Controller
	openPhoto func(path string)

	// mu guards the published load (loaded/ver/loading/gen) and the target,
	// which the load goroutine reads.
	mu      sync.Mutex
	loaded  []cache.FaceInfo // last published face list
	ver     uint64           // PeopleVersion the list was loaded at
	loading bool
	gen     int
	target  ReviewTarget

	// UI-goroutine-only state, synced from loaded once per frame.
	faces  []cache.FaceInfo
	cursor int
	marked map[int64]bool
	cols   int
	list   widget.List
	clicks []*widget.Clickable
	// pendingScroll asks the next Layout to bring the cursor row into view.
	pendingScroll bool

	picker personPicker

	invalidate func()
}

// NewReviewView wires the overlay to the controller (face queries, avatars,
// verdict ops) and to the viewer's open-photo action.
func NewReviewView(ctrl *Controller, openPhoto func(path string)) *ReviewView {
	r := &ReviewView{ctrl: ctrl, openPhoto: openPhoto}
	r.list.Axis = layout.Vertical
	return r
}

// SetInvalidate registers the callback that wakes the Gio frame loop after a
// background face load.
func (r *ReviewView) SetInvalidate(f func()) { r.invalidate = f }

// Show opens the overlay on a new target, resetting the cursor, marks and
// scroll position, and starts the asynchronous face load.
func (r *ReviewView) Show(t ReviewTarget) {
	r.Open = true
	r.picker.Close()
	r.cursor = 0
	r.marked = nil
	r.pendingScroll = true
	r.faces = nil
	r.list.Position.First, r.list.Position.Offset, r.list.Position.Count = 0, 0, 0

	ver := uint64(0)
	if r.ctrl != nil {
		ver = r.ctrl.PeopleVersion()
	}
	r.mu.Lock()
	r.target = t
	r.gen++
	r.loaded = nil
	r.ver = ver
	r.loading = r.ctrl != nil
	r.mu.Unlock()
	if r.ctrl != nil {
		r.startLoad(ver)
	}
}

// Close hides the overlay.
func (r *ReviewView) Close() {
	r.Open = false
	r.picker.Close()
	r.marked = nil
}

// TextInput reports whether a text editor (the person picker) is on top, so
// the window keeps printable keys away from the app-wide hotkeys.
func (r *ReviewView) TextInput() bool { return r.picker.Open }

// startLoad fetches the target's faces in the background and publishes them
// with the people version they were read at. A load whose generation is stale
// (the target changed under it) publishes nothing.
func (r *ReviewView) startLoad(ver uint64) {
	r.mu.Lock()
	gen := r.gen
	t := r.target
	r.loading = true
	r.mu.Unlock()

	go func() {
		var (
			fs  []cache.FaceInfo
			err error
		)
		switch {
		case t.ClusterID > 0:
			fs, err = r.ctrl.index.FacesOfCluster(t.ClusterID, cache.OrderOutlierFirst)
		case t.Suggest:
			fs, err = r.ctrl.index.SuggestionsFor(t.PersonID)
		case t.PersonID > 0:
			fs, err = r.ctrl.index.FacesOfPerson(t.PersonID, cache.OrderOutlierFirst)
		}
		if err != nil {
			log.Printf("review: load faces: %v", err)
		}
		r.mu.Lock()
		if r.gen != gen {
			r.mu.Unlock()
			return
		}
		r.loaded = fs
		r.ver = ver
		r.loading = false
		r.mu.Unlock()
		if r.invalidate != nil {
			r.invalidate()
		}
	}()
}

// targetCopy returns the current target under the load mutex.
func (r *ReviewView) targetCopy() ReviewTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target
}

// syncFaces publishes a new face list to the UI-goroutine state and drops the
// cursor/marks that no longer refer to a face (a reload after a verdict can
// shrink the review set).
func (r *ReviewView) syncFaces(faces []cache.FaceInfo) {
	r.faces = faces
	r.cursor = min(max(r.cursor, 0), max(len(faces)-1, 0))
	if len(r.marked) == 0 {
		r.marked = nil
		return
	}
	keep := make(map[int64]bool, len(r.marked))
	for _, f := range faces {
		if r.marked[f.ID] {
			keep[f.ID] = true
		}
	}
	if len(keep) == 0 {
		r.marked = nil
		return
	}
	r.marked = keep
}

// sameFaceSlice reports whether a and b are the same published list. Loads
// always allocate a fresh slice, so first-element identity is exact.
func sameFaceSlice(a, b []cache.FaceInfo) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// setFacesForTest seeds the UI-side face list and column count without a
// load. Unit tests use it to exercise cursor movement and targets().
func (r *ReviewView) setFacesForTest(faces []cache.FaceInfo, cols int) {
	r.faces = faces
	r.cols = cols
	r.cursor = 0
	r.marked = nil
}

// people returns the visible people for the picker.
func (r *ReviewView) people() []cache.Person {
	if r.ctrl == nil {
		return nil
	}
	snap, _ := r.ctrl.People()
	return snap.People
}

// personName resolves a person's name for the toast messages.
func (r *ReviewView) personName(id int64) string {
	if r.ctrl != nil {
		if n, ok := r.ctrl.index.PersonName(id); ok {
			return n
		}
	}
	return r.targetCopy().Title
}

// cursorFace returns the face under the cursor, if any.
func (r *ReviewView) cursorFace() (cache.FaceInfo, bool) {
	if r.cursor < 0 || r.cursor >= len(r.faces) {
		return cache.FaceInfo{}, false
	}
	return r.faces[r.cursor], true
}

// allIDs is every face of the review set, for the suggest-target "Y all" /
// "N all" shortcuts.
func (r *ReviewView) allIDs() []int64 {
	ids := make([]int64, len(r.faces))
	for i, f := range r.faces {
		ids[i] = f.ID
	}
	return ids
}

// targets is the face set an action applies to: the marked faces when any are
// marked, otherwise just the cursor face.
func (r *ReviewView) targets() []int64 {
	if len(r.marked) > 0 {
		ids := make([]int64, 0, len(r.marked))
		for _, f := range r.faces {
			if r.marked[f.ID] {
				ids = append(ids, f.ID)
			}
		}
		return ids
	}
	if f, ok := r.cursorFace(); ok {
		return []int64{f.ID}
	}
	return nil
}

// moveCursor moves the selection by delta cells, clamped to the face list,
// and asks the next Layout to keep it in view. J/K pass the column count so a
// row jump lands on the same column, the way the photo grid behaves.
func (r *ReviewView) moveCursor(delta int) bool {
	if len(r.faces) == 0 {
		return true
	}
	next := min(max(r.cursor+delta, 0), len(r.faces)-1)
	if next != r.cursor {
		r.cursor = next
		r.pendingScroll = true
	}
	return true
}

// toggleMark flips the mark on the cursor face.
func (r *ReviewView) toggleMark() bool {
	f, ok := r.cursorFace()
	if !ok {
		return true
	}
	if r.marked == nil {
		r.marked = map[int64]bool{}
	}
	if r.marked[f.ID] {
		delete(r.marked, f.ID)
	} else {
		r.marked[f.ID] = true
	}
	return true
}

// confirm is the Y / Return action. In an unnamed group there is nobody to
// confirm (the group has no name yet), so it is a no-op; on a suggest target
// Shift confirms every suggestion at once.
func (r *ReviewView) confirm(all bool) bool {
	t := r.targetCopy()
	ids := r.targets()
	if t.ClusterID > 0 || len(ids) == 0 || r.ctrl == nil {
		return true
	}
	if t.Suggest && all {
		ids = r.allIDs()
	}
	r.ctrl.Retag(ids, cache.VerdictConfirmed, t.PersonID, "Confirmed "+faceCountLabel(len(ids)))
	r.marked = nil
	return true
}

// reject is the N / X action: "not this person" for a person (or a
// suggestion), "remove from group" for an unnamed cluster. Shift rejects
// every suggestion at once.
func (r *ReviewView) reject(all bool) bool {
	t := r.targetCopy()
	ids := r.targets()
	if len(ids) == 0 || r.ctrl == nil {
		return true
	}
	if t.Suggest && all {
		ids = r.allIDs()
	}
	switch {
	case t.ClusterID > 0:
		r.ctrl.SplitFaces(ids)
	case t.PersonID > 0:
		r.ctrl.Retag(ids, cache.VerdictRejected, t.PersonID,
			"Removed "+faceCountLabel(len(ids))+" from "+r.personName(t.PersonID))
	default:
		return true
	}
	r.marked = nil
	return true
}

// ignore is the I action: "not a face / don't care".
func (r *ReviewView) ignore() bool {
	ids := r.targets()
	if len(ids) == 0 || r.ctrl == nil {
		return true
	}
	r.ctrl.Retag(ids, cache.VerdictIgnored, 0, "Ignored "+faceCountLabel(len(ids)))
	r.marked = nil
	return true
}

// Key handles one key press. While the picker is open every key belongs to it
// (so a letter typed into the name editor can't also fire a verdict).
func (r *ReviewView) Key(ke key.Event) bool {
	if r.picker.Open {
		r.picker.Key(ke, r.people())
		return true
	}
	if !r.Open {
		return false
	}
	if isCtrlBracket(ke) {
		r.Close()
		return true
	}
	if ke.Name == "Z" && ke.Modifiers.Contain(key.ModCtrl) {
		if r.ctrl != nil {
			r.ctrl.UndoPeople()
		}
		return true
	}

	cols := max(r.cols, 1)
	switch ke.Name {
	case "H", key.NameLeftArrow:
		return r.moveCursor(-1)
	case "L", key.NameRightArrow:
		return r.moveCursor(1)
	case "J", key.NameDownArrow:
		return r.moveCursor(cols)
	case "K", key.NameUpArrow:
		return r.moveCursor(-cols)
	case key.NameEscape, "Q":
		r.Close()
		return true
	case "V":
		return r.toggleMark()
	case "Y", key.NameReturn:
		return r.confirm(ke.Modifiers.Contain(key.ModShift))
	case "N", "X":
		return r.reject(ke.Modifiers.Contain(key.ModShift))
	case "S":
		if !r.targetCopy().Suggest {
			return false
		}
		return r.moveCursor(1)
	case "M":
		r.picker.Show(func(personID int64, newName string) {
			ids := r.targets()
			if len(ids) == 0 || r.ctrl == nil {
				return
			}
			label := faceCountLabel(len(ids))
			if personID > 0 {
				r.ctrl.Retag(ids, cache.VerdictConfirmed, personID,
					"Moved "+label+" to "+r.personName(personID))
			} else if newName != "" {
				r.ctrl.ConfirmAsName(ids, newName)
			}
			r.marked = nil
		})
		return true
	case "I":
		return r.ignore()
	case "C":
		t := r.targetCopy()
		f, ok := r.cursorFace()
		if ok && r.ctrl != nil && t.PersonID > 0 && !t.Suggest {
			r.ctrl.SetCover(t.PersonID, f.ID)
		}
		return true
	case "O":
		f, ok := r.cursorFace()
		if ok {
			if r.openPhoto != nil {
				r.openPhoto(f.Path)
			}
			r.Close()
		}
		return true
	}
	return false
}

// helpLine is the keyboard legend under the header; the cluster and suggest
// targets have their own verbs (no person to reject / confirm, or a skip).
func (r *ReviewView) helpLine() string {
	t := r.targetCopy()
	switch {
	case t.ClusterID > 0:
		return "n remove from group · m move to… (name it) · i not a face · v mark · o open photo · ctrl+z undo · esc close"
	case t.Suggest:
		return "y yes · n no · s skip · Y all yes · N all no · esc close"
	}
	return "y confirm · n not this person · m move to… · i not a face · c set cover · v mark · o open photo · ctrl+z undo · esc close"
}

// headerText is the title line: what is being reviewed and how much of it.
func (r *ReviewView) headerText() string {
	t := r.targetCopy()
	title := t.Title
	if title == "" {
		switch {
		case t.ClusterID > 0:
			title = "Unnamed group"
		case t.Suggest:
			title = "Suggestions"
		}
	}
	return fmt.Sprintf("%s — %s, farthest from the group first", title, faceCountLabel(len(r.faces)))
}

// Layout draws the overlay: header, keyboard legend and the face grid. It
// also kicks off a reload when the people version has moved since the faces
// were read, so a verdict made elsewhere shows up here.
func (r *ReviewView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if r.ctrl != nil {
		r.mu.Lock()
		loading, ver := r.loading, r.ver
		r.mu.Unlock()
		if !loading {
			if cur := r.ctrl.PeopleVersion(); cur != ver {
				r.startLoad(cur)
			}
		}
	}
	r.mu.Lock()
	loaded, loading := r.loaded, r.loading
	r.mu.Unlock()
	pollWhileLoading(gtx, loading)
	if !sameFaceSlice(loaded, r.faces) {
		r.syncFaces(loaded)
	}

	drawBackground(gtx, th.Background)
	pad := layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12)}
	dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, r.headerText())
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(11), r.helpLine())
				lbl.Color = th.Muted
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(r.faces) == 0 {
					lbl := material.Label(th.Theme, unit.Sp(14), "No faces to review")
					lbl.Color = th.Muted
					if loading {
						lbl.Text = "Loading…"
					}
					return layout.Center.Layout(gtx, lbl.Layout)
				}
				return r.layoutGrid(gtx, th)
			}),
		)
	})

	if r.picker.Open {
		r.picker.Layout(gtx, th, r.people(), "Move to person…")
	}
	return dims
}

// layoutGrid lays the faces out in rows of as many 96dp cells as fit across
// the window, mirroring the photo grid's column fitting.
func (r *ReviewView) layoutGrid(gtx layout.Context, th *Theme) layout.Dimensions {
	cellPx := gtx.Dp(unit.Dp(reviewCellDp))
	gapPx := gtx.Dp(unit.Dp(reviewGapDp))
	listStyle := material.List(th.Theme, &r.list)
	width := max(gtx.Constraints.Max.X-gtx.Dp(listStyle.Width()), 0)
	cols := max((width+gapPx)/(cellPx+gapPx), 1)
	r.cols = cols

	if cap(r.clicks) < len(r.faces) {
		grown := max(len(r.faces)+len(r.faces)/5, len(r.faces)+8)
		next := make([]*widget.Clickable, len(r.faces), grown)
		copy(next, r.clicks)
		r.clicks = next
	} else {
		r.clicks = r.clicks[:len(r.faces)]
	}

	rowCount := (len(r.faces) + cols - 1) / cols

	// Keep the cursor row on screen; only the rows around the cursor need
	// pointer events drained, so this mirrors the grid's windowed sweep.
	if r.pendingScroll && rowCount > 0 {
		selRow := r.cursor / cols
		first, count := r.list.Position.First, r.list.Position.Count
		switch {
		case count == 0, selRow < first:
			r.list.ScrollTo(selRow)
		case selRow >= first+count:
			r.list.ScrollTo(max(selRow-count+1, 0))
		}
		r.pendingScroll = false
	}

	first := max(r.list.Position.First, 0)
	last := min((first+r.list.Position.Count+1)*cols, len(r.faces))
	for i := min(first*cols, last); i < last; i++ {
		if r.clicks[i] == nil {
			continue
		}
		if r.clicks[i].Clicked(gtx) {
			if r.cursor != i {
				r.cursor = i
				r.pendingScroll = true
			}
		}
	}

	return listStyle.Layout(gtx, rowCount, func(gtx layout.Context, row int) layout.Dimensions {
		children := make([]layout.FlexChild, 0, cols*2)
		for c := 0; c < cols; c++ {
			i := row*cols + c
			if c > 0 {
				children = append(children, layout.Rigid(layout.Spacer{Width: unit.Dp(reviewGapDp)}.Layout))
			}
			if i >= len(r.faces) {
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Dimensions{Size: image.Pt(cellPx, cellPx)}
				}))
				continue
			}
			fi := r.faces[i]
			if r.clicks[i] == nil {
				r.clicks[i] = &widget.Clickable{}
			}
			click := r.clicks[i]
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return r.layoutCell(gtx, th, fi, cellPx, i == r.cursor, r.marked[fi.ID], click)
			}))
		}
		rowDims := layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
		return layout.Dimensions{Size: rowDims.Size.Add(image.Pt(0, gapPx))}
	})
}

// layoutCell draws one face: the avatar, its file name, the selection border
// on the cursor and a check overlay on marked cells.
func (r *ReviewView) layoutCell(gtx layout.Context, th *Theme, fi cache.FaceInfo, sizePx int, cursor, marked bool, click *widget.Clickable) layout.Dimensions {
	var avatars *avatarCache
	if r.ctrl != nil {
		avatars = r.ctrl.Avatars()
	}
	labelPx := gtx.Dp(unit.Dp(16))
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		cgtx := gtx
		cgtx.Constraints = layout.Exact(image.Pt(sizePx, sizePx+labelPx))
		dims := layout.Flex{Axis: layout.Vertical}.Layout(cgtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return drawAvatar(gtx, th, avatars, fi.Face, sizePx, false)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(10), filepath.Base(fi.Path))
				lbl.Color = th.Muted
				lbl.MaxLines = 1
				return lbl.Layout(gtx)
			}),
		)
		sq := image.Rect(0, 0, sizePx, sizePx)
		if marked {
			// Tint the avatar rather than hiding it: the user still needs to
			// see which face the mark is on.
			tint := th.SelectionBG
			tint.A = 0xa0
			paint.FillShape(gtx.Ops, tint, clip.Rect(sq).Op())
			over := gtx
			over.Constraints = layout.Exact(sq.Max)
			layout.Center.Layout(over, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(28), "✓")
				lbl.Color = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
				return lbl.Layout(gtx)
			})
		}
		if cursor {
			drawBorder(gtx, sq, gtx.Dp(unit.Dp(3)), th.Accent)
		}
		return dims
	})
}
