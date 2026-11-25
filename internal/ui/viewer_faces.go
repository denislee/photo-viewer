package ui

import (
	"image"
	"image/color"
	"slices"
	"strings"
	"sync"

	"gioui.org/f32"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// viewerFaces is the viewer's face layer (plan §4.3): `p` toggles boxes on
// the current photo, `t` cycles through them, clicking a box or Enter on the
// selected one opens the name picker, and Y / N / X confirm the shown name,
// reject it ("Not <name>") or mark it "Not a face". Faces are loaded off the
// UI goroutine per (path, PeopleVersion).
type viewerFaces struct {
	ctrl       *Controller
	openPerson func(id int64)

	Show bool
	sel  int // selected box, -1 none

	mu      sync.Mutex
	path    string
	ver     uint64
	faces   []cache.FaceInfo
	loading bool

	picker      personPicker
	clicks      []widget.Clickable
	personLinks []widget.Clickable
	people      []cache.Person // distinct people in the photo (for the info panel)
}

func newViewerFaces(ctrl *Controller, openPerson func(id int64)) *viewerFaces {
	return &viewerFaces{ctrl: ctrl, openPerson: openPerson, sel: -1}
}

// forPath returns the faces of path, kicking an async load when the cached
// set is for another path or an older people version.
func (vf *viewerFaces) forPath(path string) []cache.FaceInfo {
	if vf == nil || vf.ctrl == nil {
		return nil
	}
	ver := vf.ctrl.PeopleVersion()
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if (vf.path != path || vf.ver != ver) && !vf.loading {
		vf.loading = true
		go func() {
			fs, _ := vf.ctrl.Index().FacesInPhoto(path)
			var ppl []cache.Person
			for _, f := range fs {
				if f.PersonID.Valid && !slices.ContainsFunc(ppl, func(p cache.Person) bool { return p.ID == f.PersonID.Int64 }) {
					ppl = append(ppl, cache.Person{ID: f.PersonID.Int64, Name: f.PersonName})
				}
			}
			vf.mu.Lock()
			if vf.path != path {
				vf.sel = -1
			}
			vf.path, vf.ver, vf.faces, vf.people, vf.loading = path, ver, fs, ppl, false
			vf.mu.Unlock()
			if vf.ctrl.invalidate != nil {
				vf.ctrl.invalidate()
			}
		}()
	}
	if vf.path != path {
		return nil
	}
	return vf.faces
}

// peopleIn returns the named people of path (empty until loaded).
func (vf *viewerFaces) peopleIn(path string) []cache.Person {
	vf.forPath(path)
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.path != path {
		return nil
	}
	return vf.people
}

// boxRect maps a face's normalised box into the drawn image rectangle.
func boxRect(nb [4]float64, img f32Rect) image.Rectangle {
	w, h := img.max.X-img.min.X, img.max.Y-img.min.Y
	x0 := img.min.X + float32(nb[0])*w
	y0 := img.min.Y + float32(nb[1])*h
	return image.Rect(int(x0), int(y0), int(x0+float32(nb[2])*w), int(y0+float32(nb[3])*h))
}

// f32Rect is the drawn image's on-screen rectangle (fractional pixels).
type f32Rect struct{ min, max f32.Point }

// draw paints the boxes over the photo. img is where drawZoomed put the
// image; area clips the boxes to the viewer's image area.
func (vf *viewerFaces) draw(gtx layout.Context, th *Theme, path string, img f32Rect, area image.Rectangle) {
	if vf == nil || (!vf.Show && vf.sel < 0) {
		return
	}
	faces := vf.forPath(path)
	vf.mu.Lock()
	pollWhileLoading(gtx, vf.loading)
	vf.mu.Unlock()
	if len(vf.clicks) < len(faces) {
		vf.clicks = make([]widget.Clickable, len(faces))
	}
	defer clip.Rect(area).Push(gtx.Ops).Pop()
	stroke := float32(gtx.Dp(unit.Dp(2)))
	for k, f := range faces {
		if !cache.HasNBox(f.NBox) {
			continue
		}
		r := boxRect(f.NBox, img)
		col := th.Muted
		label := "?"
		switch {
		case f.Assign == cache.AssignIgnored:
			col, label = color.NRGBA{R: 0x70, G: 0x70, B: 0x70, A: 0xa0}, "not a face"
		case f.PersonID.Valid:
			col, label = th.Accent, f.PersonName
			if f.Assign != cache.AssignUser {
				label += " ?" // attributed by its group, not confirmed
			}
		}
		if k == vf.sel {
			col = color.NRGBA{R: 0xff, G: 0xd0, B: 0x40, A: 0xff}
		}
		paint.FillShape(gtx.Ops, col, clip.Stroke{Path: clip.Rect(r).Path(), Width: stroke}.Op())
		// Label chip above the box (inside it when the box touches the top).
		lbl := material.Label(th.Theme, unit.Sp(12), label)
		lbl.Color = th.Foreground
		m := op.Record(gtx.Ops)
		d := layout.Inset{Left: unit.Dp(4), Right: unit.Dp(4), Top: unit.Dp(1), Bottom: unit.Dp(1)}.Layout(gtx, lbl.Layout)
		call := m.Stop()
		y := r.Min.Y - d.Size.Y
		if y < area.Min.Y {
			y = r.Min.Y
		}
		st := op.Offset(image.Pt(r.Min.X, y)).Push(gtx.Ops)
		paint.FillShape(gtx.Ops, color.NRGBA{A: 0xb0}, clip.Rect{Max: d.Size}.Op())
		call.Add(gtx.Ops)
		st.Pop()
		// Click target over the box.
		st = op.Offset(r.Min).Push(gtx.Ops)
		g := gtx
		g.Constraints = layout.Exact(r.Size())
		if vf.clicks[k].Clicked(g) {
			vf.sel = k
			vf.openPicker()
		}
		vf.clicks[k].Layout(g, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} })
		st.Pop()
	}
}

// selected returns the selected face, if any.
func (vf *viewerFaces) selected() (cache.FaceInfo, bool) {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.sel < 0 || vf.sel >= len(vf.faces) {
		return cache.FaceInfo{}, false
	}
	return vf.faces[vf.sel], true
}

func (vf *viewerFaces) openPicker() {
	f, ok := vf.selected()
	if !ok {
		return
	}
	vf.picker.Show(func(personID int64, newName string) {
		if personID > 0 {
			name, _ := vf.ctrl.Index().PersonName(personID)
			vf.ctrl.Retag([]int64{f.ID}, cache.VerdictConfirmed, personID, "Tagged "+name)
		} else if strings.TrimSpace(newName) != "" {
			vf.ctrl.ConfirmAsName([]int64{f.ID}, newName)
		}
	})
}

// handleKey runs the viewer's face keys; reports whether it consumed ke.
func (vf *viewerFaces) handleKey(ke key.Event, path string) bool {
	if vf == nil {
		return false
	}
	switch ke.Name {
	case "P":
		vf.Show = !vf.Show
		if !vf.Show {
			vf.sel = -1
		}
		return true
	case "T":
		faces := vf.forPath(path)
		if len(faces) == 0 {
			vf.ctrl.setToast("No faces detected in this photo")
			return true
		}
		vf.Show = true
		vf.sel = (vf.sel + 1) % len(faces)
		return true
	}
	if vf.sel < 0 {
		return false
	}
	f, ok := vf.selected()
	if !ok {
		vf.sel = -1
		return false
	}
	switch ke.Name {
	case key.NameEscape:
		vf.sel = -1
	case key.NameReturn, key.NameEnter:
		vf.openPicker()
	case "Y":
		if f.PersonID.Valid {
			vf.ctrl.Retag([]int64{f.ID}, cache.VerdictConfirmed, f.PersonID.Int64, "Confirmed "+f.PersonName)
		} else {
			vf.openPicker()
		}
	case "N":
		if f.PersonID.Valid {
			vf.ctrl.Retag([]int64{f.ID}, cache.VerdictRejected, f.PersonID.Int64, "Not "+f.PersonName)
		}
	case "X":
		vf.ctrl.Retag([]int64{f.ID}, cache.VerdictIgnored, 0, "Marked as not a face")
	default:
		return false
	}
	return true
}

// layoutPeopleLinks draws the info panel's People line as clickable names
// (each opens that person's view) at the current offset, width w. Returns
// the height used.
func (vf *viewerFaces) layoutPeopleLinks(gtx layout.Context, th *Theme, path string, w int) int {
	ppl := vf.peopleIn(path)
	vf.mu.Lock()
	pollWhileLoading(gtx, vf.loading)
	vf.mu.Unlock()
	if len(ppl) == 0 {
		return 0
	}
	if len(vf.personLinks) < len(ppl) {
		vf.personLinks = make([]widget.Clickable, len(ppl))
	}
	g := gtx
	g.Constraints = layout.Constraints{Max: image.Pt(w, gtx.Constraints.Max.Y)}
	y := 0
	head := material.Label(th.Theme, unit.Sp(11), "People")
	head.Color = th.Muted
	head.Font.Weight = 700
	y += head.Layout(g).Size.Y
	for k, p := range ppl {
		st := op.Offset(image.Pt(0, y)).Push(gtx.Ops)
		if vf.personLinks[k].Clicked(g) && vf.openPerson != nil {
			vf.openPerson(p.ID)
		}
		d := vf.personLinks[k].Layout(g, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(th.Theme, unit.Sp(12), p.Name)
			l.Color = th.Accent
			return l.Layout(gtx)
		})
		st.Pop()
		y += d.Size.Y
	}
	return y
}

// pickerPeople lists every person (visible and hidden) for the tag picker.
func (vf *viewerFaces) pickerPeople() []cache.Person {
	snap, _ := vf.ctrl.People()
	return append(append([]cache.Person(nil), snap.People...), snap.Hidden...)
}
