package ui

import (
	"image"
	"image/color"
	"slices"
	"strings"

	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// pickerMaxItems bounds the person list so the panel never outgrows the
// window on a library with thousands of people.
const pickerMaxItems = 12

// Picker panel geometry, in dp. The panel is capped on both axes so it stays
// a dialog in the middle of the window on any display.
const (
	pickerWidthDp  = 420
	pickerHeightDp = 460
)

// pickItem is one row of the person picker: either an existing person, or the
// "+ New person" entry built from the typed text when no person has it yet.
type pickItem struct {
	PersonID int64
	Name     string
	New      bool
}

// personPicker is the "move to person…" dialog shared by the People views and
// the face review overlay. It is a plain value (no Controller) so the caller
// decides what a pick means: onPick gets the chosen person id, or id 0 plus
// the new name for the "+ New person" row.
type personPicker struct {
	Open   bool
	editor widget.Editor
	list   widget.List
	sel    int
	clicks []widget.Clickable
	onPick func(personID int64, newName string)

	// focus requests keyboard focus for the editor on the next Layout. The
	// view was just shown, so the user can type immediately.
	focus bool
}

// Show opens the picker for one pick. The editor is cleared and focused on
// the next Layout; the list starts at the top.
func (p *personPicker) Show(onPick func(personID int64, newName string)) {
	p.editor.SetText("")
	p.editor.SingleLine = true
	p.editor.Submit = true
	p.sel = 0
	p.Open = true
	p.onPick = onPick
	p.focus = true
	p.list.Axis = layout.Vertical
	p.list.Position.First, p.list.Position.Offset, p.list.Position.Count = 0, 0, 0
}

// Close hides the picker. The callback is cleared so a stale closure can't
// fire against a target the user has since left.
func (p *personPicker) Close() {
	p.Open = false
	p.onPick = nil
}

// matches filters people by the editor text and appends the "+ New person"
// row when the query names nobody yet. Ranking is fuzzyScore (lower is
// better) with the name as tie-breaker, so an exact/prefix match lands first.
func (p *personPicker) matches(people []cache.Person) []pickItem {
	q := strings.TrimSpace(p.editor.Text())
	ql := strings.ToLower(q)

	type scored struct {
		it    pickItem
		score int
	}
	hits := make([]scored, 0, len(people))
	for _, person := range people {
		ok, score := fuzzyScore(ql, strings.ToLower(person.Name))
		if !ok {
			continue
		}
		hits = append(hits, scored{it: pickItem{PersonID: person.ID, Name: person.Name}, score: score})
	}
	slices.SortStableFunc(hits, func(a, b scored) int {
		if a.score != b.score {
			return a.score - b.score
		}
		return strings.Compare(strings.ToLower(a.it.Name), strings.ToLower(b.it.Name))
	})

	items := make([]pickItem, len(hits))
	for k, h := range hits {
		items[k] = h.it
	}
	// Offer to create the person only when the typed name is genuinely new;
	// an exact (case-insensitive) match must pick that person instead.
	if q != "" && !hasName(people, q) {
		items = append(items, pickItem{Name: q, New: true})
	}
	if len(items) > pickerMaxItems {
		items = items[:pickerMaxItems]
	}
	return items
}

// hasName reports whether any person is called name, case-insensitively.
func hasName(people []cache.Person, name string) bool {
	for _, p := range people {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// Key handles one key press while the picker is open. Return picks the
// highlighted row, Escape backs out. Returns true when the key was consumed.
func (p *personPicker) Key(ke key.Event, people []cache.Person) bool {
	if !p.Open {
		return false
	}
	items := p.matches(people)
	switch ke.Name {
	case key.NameUpArrow:
		p.sel = max(p.sel-1, 0)
		return true
	case key.NameDownArrow:
		p.sel = min(p.sel+1, max(len(items)-1, 0))
		return true
	case key.NameReturn, key.NameEnter:
		p.pick(items)
		return true
	case key.NameEscape:
		p.Close()
		return true
	}
	return false
}

// pick activates items[sel]: an existing person is reported by id, the
// "+ New person" row by a zero id and the typed name.
func (p *personPicker) pick(items []pickItem) {
	if p.sel < 0 || p.sel >= len(items) {
		p.Close()
		return
	}
	it := items[p.sel]
	onPick := p.onPick
	if onPick != nil {
		if it.New {
			onPick(0, it.Name)
		} else {
			onPick(it.PersonID, "")
		}
	}
	p.Close()
}

// Layout draws the picker as a centered panel over a dimmed backdrop. title
// names what the choice is for ("Move to person…").
func (p *personPicker) Layout(gtx layout.Context, th *Theme, people []cache.Person, title string) layout.Dimensions {
	if !p.Open {
		return layout.Dimensions{}
	}
	if p.focus {
		gtx.Execute(key.FocusCmd{Tag: &p.editor})
		p.focus = false
	}
	for {
		evt, ok := p.editor.Update(gtx)
		if !ok {
			break
		}
		// Enter in the editor is the same as Return on the list.
		if _, ok := evt.(widget.SubmitEvent); ok {
			p.pick(p.matches(people))
		}
	}

	items := p.matches(people)
	p.sel = min(max(p.sel, 0), max(len(items)-1, 0))
	if cap(p.clicks) < len(items) {
		p.clicks = make([]widget.Clickable, len(items), max(len(items)+4, pickerMaxItems))
	} else {
		p.clicks = p.clicks[:len(items)]
	}

	total := gtx.Constraints.Max
	// Dim whatever is beneath so the panel reads as a dialog.
	paint.FillShape(gtx.Ops, color.NRGBA{A: 0xb0}, clip.Rect(image.Rectangle{Max: total}).Op())

	// Lay the panel out into a macro so it can be centered on its measured
	// height instead of a guessed one.
	panelW := min(total.X, gtx.Dp(unit.Dp(pickerWidthDp)))
	panelH := min(total.Y, gtx.Dp(unit.Dp(pickerHeightDp)))
	macro := op.Record(gtx.Ops)
	gtx.Constraints = layout.Constraints{
		Min: image.Pt(panelW, 0),
		Max: image.Pt(panelW, panelH),
	}
	dims := p.layoutPanel(gtx, th, items, title)
	call := macro.Stop()

	off := image.Pt(max((total.X-dims.Size.X)/2, 0), max((total.Y-dims.Size.Y)/2, 0))
	defer op.Offset(off).Push(gtx.Ops).Pop()
	paint.FillShape(gtx.Ops, th.Background, clip.Rect(image.Rectangle{Max: dims.Size}).Op())
	drawBorder(gtx, image.Rectangle{Max: dims.Size}, 1, th.Muted)
	call.Add(gtx.Ops)
	return layout.Dimensions{Size: total}
}

// layoutPanel draws the title, the name editor and the match list inside the
// constraints the caller sized for the panel.
func (p *personPicker) layoutPanel(gtx layout.Context, th *Theme, items []pickItem, title string) layout.Dimensions {
	pad := layout.Inset{Top: unit.Dp(14), Bottom: unit.Dp(14), Left: unit.Dp(16), Right: unit.Dp(16)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, title)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return drawEditorBox(gtx, th.CellBG,
					layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(10), Right: unit.Dp(10)},
					func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th.Theme, &p.editor, "Person name…")
						ed.Color = th.Foreground
						ed.HintColor = th.Muted
						ed.TextSize = unit.Sp(14)
						return ed.Layout(gtx)
					})
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return p.layoutItems(gtx, th, items)
			}),
		)
	})
}

// layoutItems draws the scrollable match list.
func (p *personPicker) layoutItems(gtx layout.Context, th *Theme, items []pickItem) layout.Dimensions {
	if len(items) == 0 {
		lbl := material.Label(th.Theme, unit.Sp(12), "No matches")
		lbl.Color = th.Muted
		return lbl.Layout(gtx)
	}
	p.list.Axis = layout.Vertical
	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
		click := &p.clicks[i]
		if click.Clicked(gtx) {
			p.sel = i
			p.pick(items)
		}
		return p.layoutRow(gtx, th, items[i], i == p.sel, click)
	})
}

// layoutRow draws one match, highlighted when it is the keyboard selection.
func (p *personPicker) layoutRow(gtx layout.Context, th *Theme, it pickItem, active bool, click *widget.Clickable) layout.Dimensions {
	text := it.Name
	if it.New {
		text = `+ New person "` + it.Name + `"`
	}
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13), text)
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		})
		call := macro.Stop()

		w := gtx.Constraints.Max.X
		h := dims.Size.Y
		clipArea := clip.Rect(image.Rect(0, 0, w, h)).Push(gtx.Ops)
		bg := th.Background
		if active {
			bg = th.SelectionBG
		}
		paint.ColorOp{Color: bg}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		call.Add(gtx.Ops)
		pointer.CursorPointer.Add(gtx.Ops)
		clipArea.Pop()
		return layout.Dimensions{Size: image.Pt(w, h)}
	})
}
