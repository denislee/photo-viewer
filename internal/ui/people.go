package ui

import (
	"fmt"
	"image"
	"strings"

	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
)

// People overview geometry, in dp. The card width includes its padding so a
// row's cards can be laid out edge to edge.
const (
	peopleCardDp   = 120
	peopleAvatarDp = 96
	peopleSelWidth = 3
)

// peopleCardKind says which list a PeoplePane card came from. The overview
// draws them in one flat order (people, groups, small groups, hidden) so the
// keyboard selection is a single index.
type peopleCardKind uint8

const (
	cardKindPerson peopleCardKind = iota
	cardKindGroup
	cardKindHidden
)

// peopleCard is one avatar card: either a named person (visible or hidden) or
// an unnamed group.
type peopleCard struct {
	kind   peopleCardKind
	person cache.Person
	group  cache.Group
}

// peopleBlockKind is the kind of one widget.List row in the overview.
type peopleBlockKind uint8

const (
	blockKindToolbar peopleBlockKind = iota
	blockKindLabel
	blockKindSuggestion
	blockKindHeader
	blockKindCards
	blockKindToggle
)

// peopleBlock is one row of the overview's outer scroll list. Card rows carry
// a [start, end) range into the flat card list.
type peopleBlock struct {
	kind    peopleBlockKind
	text    string
	start   int
	end     int
	suggIdx int
	toggle  *widget.Clickable
}

// PeoplePane is the People overview: the toolbar (regroup / strictness), the
// "might be" suggestion strip, and wrapping rows of avatar cards for people,
// unnamed groups, small groups and hidden people.
//
// It is drawn in the grid area when the current directory is PeopleView, so it
// owns its own scroll list and keyboard cursor; the window only forwards keys
// and asks it to lay out.
type PeoplePane struct {
	ctrl       *Controller
	openReview func(ReviewTarget)

	snap   PeopleSnapshot
	loaded bool

	showSmall  bool
	showHidden bool

	selected int
	cols     int
	// cardBlock maps a flat card index to the list row it was drawn in, so the
	// cursor can be scrolled into view.
	cardBlock     []int
	pendingScroll bool

	list widget.List

	regroupBtn widget.Clickable
	strictBtn  widget.Clickable
	smallBtn   widget.Clickable
	hiddenBtn  widget.Clickable

	cardClicks []*widget.Clickable
	suggClicks []*widget.Clickable
}

// NewPeoplePane wires the overview to the controller and to the window's
// review-overlay opener.
func NewPeoplePane(ctrl *Controller, openReview func(ReviewTarget)) *PeoplePane {
	p := &PeoplePane{ctrl: ctrl, openReview: openReview}
	p.list.Axis = layout.Vertical
	return p
}

// setSnapshotForTest seeds the snapshot and the grid geometry without a
// controller or a Layout pass. Unit tests use it to exercise card ordering and
// cursor movement.
func (p *PeoplePane) setSnapshotForTest(snap PeopleSnapshot, cols int) {
	p.snap = snap
	p.loaded = true
	p.cols = cols
	p.selected = 0
	p.showSmall = false
	p.showHidden = false
}

// cards builds the flat, keyboard-ordered card list for the current snapshot
// and toggles: visible people, unnamed groups, the small groups when shown,
// then the hidden people when shown.
func (p *PeoplePane) cards() []peopleCard {
	if !p.loaded {
		return nil
	}
	n := len(p.snap.People) + len(p.snap.Groups)
	if p.showSmall {
		n += len(p.snap.SmallGroups)
	}
	if p.showHidden {
		n += len(p.snap.Hidden)
	}
	out := make([]peopleCard, 0, n)
	for _, person := range p.snap.People {
		out = append(out, peopleCard{kind: cardKindPerson, person: person})
	}
	for _, g := range p.snap.Groups {
		out = append(out, peopleCard{kind: cardKindGroup, group: g})
	}
	if p.showSmall {
		for _, g := range p.snap.SmallGroups {
			out = append(out, peopleCard{kind: cardKindGroup, group: g})
		}
	}
	if p.showHidden {
		for _, person := range p.snap.Hidden {
			out = append(out, peopleCard{kind: cardKindHidden, person: person})
		}
	}
	return out
}

// empty reports whether the library has no people or groups at all.
func (p *PeoplePane) empty() bool {
	return len(p.snap.People) == 0 && len(p.snap.Groups) == 0 &&
		len(p.snap.SmallGroups) == 0 && len(p.snap.Hidden) == 0
}

// colsOr1 is the column count of the last Layout, clamped for the row moves.
func (p *PeoplePane) colsOr1() int {
	if p.cols < 1 {
		return 1
	}
	return p.cols
}

// ensureClicks sizes the card and suggestion clickables, reusing their
// backing arrays across frames.
func (p *PeoplePane) ensureClicks(cards, sugg int) {
	ensure := func(list []*widget.Clickable, n int) []*widget.Clickable {
		if cap(list) < n {
			grown := make([]*widget.Clickable, n, max(n+n/5, n+8))
			copy(grown, list)
			list = grown
		} else {
			list = list[:n]
		}
		for i := range list {
			if list[i] == nil {
				list[i] = &widget.Clickable{}
			}
		}
		return list
	}
	p.cardClicks = ensure(p.cardClicks, cards)
	p.suggClicks = ensure(p.suggClicks, sugg)
}

// statusText is the muted toolbar status: the in-flight detection pass, else
// the helper-probe failure, else nothing.
func (p *PeoplePane) statusText() string {
	if p.ctrl == nil {
		return ""
	}
	if p.ctrl.FaceDetecting() {
		return "Detecting faces…"
	}
	if status, probed := p.ctrl.FaceStatus(); probed && !status.Working {
		return "Face helper unavailable — see Settings"
	}
	return ""
}

// strictnessLabel renders a preset for the cycle button.
func strictnessLabel(s string) string {
	switch s {
	case StrictnessStrict:
		return "Strict"
	case StrictnessLoose:
		return "Loose"
	default:
		return "Normal"
	}
}

// nextStrictness is the cycle normal → strict → loose → normal.
func nextStrictness(cur string) string {
	switch cur {
	case StrictnessStrict:
		return StrictnessLoose
	case StrictnessLoose:
		return StrictnessNormal
	default:
		return StrictnessStrict
	}
}

// distinctPhotoCount counts the files behind a suggestion, so "N photos" is
// never inflated by two faces in the same picture.
func distinctPhotoCount(faces []cache.FaceInfo) int {
	if len(faces) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(faces))
	for _, f := range faces {
		seen[f.Path] = struct{}{}
	}
	return len(seen)
}

// photoCountLabel is "1 photo" / "N photos".
func photoCountLabel(n int) string {
	if n == 1 {
		return "1 photo"
	}
	return fmt.Sprintf("%d photos", n)
}

// activate opens the directory of one card.
func (p *PeoplePane) activate(c peopleCard) {
	if p.ctrl == nil {
		return
	}
	if c.kind == cardKindGroup {
		p.ctrl.SelectDir(GroupDir(c.group.ClusterID))
		return
	}
	p.ctrl.SelectDir(PersonDir(c.person.ID))
}

// activateIndex is the Return action: open the card under the cursor.
func (p *PeoplePane) activateIndex(i int) {
	cards := p.cards()
	if i < 0 || i >= len(cards) {
		return
	}
	p.selected = i
	p.activate(cards[i])
}

// moveSelection shifts the cursor by delta flat cards, clamped to the list.
func (p *PeoplePane) moveSelection(delta int) bool {
	n := len(p.cards())
	if n == 0 {
		return true
	}
	next := min(max(p.selected+delta, 0), n-1)
	if next != p.selected {
		p.selected = next
		p.pendingScroll = true
	}
	return true
}

// Key handles one key press while the overview is showing. It reports whether
// the key was consumed.
func (p *PeoplePane) Key(ke key.Event) bool {
	switch ke.Name {
	case "H", key.NameLeftArrow:
		return p.moveSelection(-1)
	case "L", key.NameRightArrow:
		return p.moveSelection(1)
	case "J", key.NameDownArrow:
		return p.moveSelection(p.colsOr1())
	case "K", key.NameUpArrow:
		return p.moveSelection(-p.colsOr1())
	case key.NameReturn, key.NameEnter:
		p.activateIndex(p.selected)
		return true
	case "R":
		if p.ctrl != nil {
			p.ctrl.RegroupFaces()
		}
		return true
	case "Z":
		if !ke.Modifiers.Contain(key.ModCtrl) {
			return false
		}
		if p.ctrl != nil {
			p.ctrl.UndoPeople()
		}
		return true
	}
	return false
}

// Layout draws the overview: the toolbar, the suggestion strip and the
// wrapping card sections, all in one vertically scrolling list.
func (p *PeoplePane) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	drawBackground(gtx, th.Background)

	// Buttons are polled before they lay out, so a click is seen this frame.
	if p.regroupBtn.Clicked(gtx) && p.ctrl != nil {
		p.ctrl.RegroupFaces()
	}
	if p.strictBtn.Clicked(gtx) && p.ctrl != nil {
		p.ctrl.SetFaceStrictness(nextStrictness(GetConfig().FaceStrictness))
	}
	if p.smallBtn.Clicked(gtx) {
		p.showSmall = !p.showSmall
		p.clampSelection()
	}
	if p.hiddenBtn.Clicked(gtx) {
		p.showHidden = !p.showHidden
		p.clampSelection()
	}

	if p.ctrl != nil {
		p.snap, p.loaded = p.ctrl.People()
	}
	cards := p.cards()
	p.clampSelection()
	p.ensureClicks(len(cards), len(p.snap.Suggestions))
	for i := range p.suggClicks {
		if p.suggClicks[i].Clicked(gtx) {
			p.openSuggestion(i)
		}
	}

	p.list.Axis = layout.Vertical
	listStyle := material.List(th.Theme, &p.list)
	width := max(gtx.Constraints.Max.X-gtx.Dp(listStyle.Width()), 0)
	p.cols = max(width/gtx.Dp(unit.Dp(peopleCardDp)), 1)

	blocks, cardBlock := p.buildBlocks(cards)
	p.cardBlock = cardBlock

	if p.pendingScroll && len(cardBlock) > 0 && p.selected < len(cardBlock) {
		block := cardBlock[p.selected]
		first, count := p.list.Position.First, p.list.Position.Count
		if count > 0 && (block < first || block >= first+count) {
			p.list.ScrollTo(block)
		}
		p.pendingScroll = false
	}

	return listStyle.Layout(gtx, len(blocks), func(gtx layout.Context, i int) layout.Dimensions {
		return p.layoutBlock(gtx, th, blocks[i], cards)
	})
}

// clampSelection keeps the cursor inside the current flat card list.
func (p *PeoplePane) clampSelection() {
	p.selected = min(max(p.selected, 0), max(len(p.cards())-1, 0))
}

// openSuggestion reviews the suggestion at index i.
func (p *PeoplePane) openSuggestion(i int) {
	if p.openReview == nil || i < 0 || i >= len(p.snap.Suggestions) {
		return
	}
	s := p.snap.Suggestions[i]
	p.openReview(ReviewTarget{PersonID: s.Person.ID, Suggest: true, Title: s.Person.Name})
}

// buildBlocks turns the snapshot into list rows. Card rows map back to their
// flat indices so scrolling the cursor into view stays cheap.
func (p *PeoplePane) buildBlocks(cards []peopleCard) ([]peopleBlock, []int) {
	blocks := []peopleBlock{{kind: blockKindToolbar}}
	cardBlock := make([]int, len(cards))
	if !p.loaded {
		blocks = append(blocks, peopleBlock{kind: blockKindLabel, text: "Loading…"})
		return blocks, cardBlock
	}

	addCards := func(start, end int) {
		cols := p.colsOr1()
		for rowStart := start; rowStart < end; rowStart += cols {
			rowEnd := min(rowStart+cols, end)
			bi := len(blocks)
			blocks = append(blocks, peopleBlock{kind: blockKindCards, start: rowStart, end: rowEnd})
			for k := rowStart; k < rowEnd; k++ {
				cardBlock[k] = bi
			}
		}
	}

	for i, s := range p.snap.Suggestions {
		blocks = append(blocks, peopleBlock{kind: blockKindSuggestion, suggIdx: i, text: s.Person.Name})
	}

	idx := 0
	if n := len(p.snap.People); n > 0 {
		blocks = append(blocks, peopleBlock{kind: blockKindHeader, text: fmt.Sprintf("People (%d)", n)})
		addCards(idx, idx+n)
	}
	idx += len(p.snap.People)

	if n := len(p.snap.Groups); n > 0 {
		blocks = append(blocks, peopleBlock{kind: blockKindHeader, text: fmt.Sprintf("Unnamed groups (%d)", n)})
		addCards(idx, idx+n)
	}
	idx += len(p.snap.Groups)

	if n := len(p.snap.SmallGroups); n > 0 {
		text := fmt.Sprintf("Show small groups (%d)", n)
		if p.showSmall {
			text = "Hide small groups"
		}
		blocks = append(blocks, peopleBlock{kind: blockKindToggle, text: text, toggle: &p.smallBtn})
		if p.showSmall {
			addCards(idx, idx+n)
		}
	}
	idx += len(p.snap.SmallGroups)

	if n := len(p.snap.Hidden); n > 0 {
		blocks = append(blocks, peopleBlock{
			kind:   blockKindToggle,
			text:   fmt.Sprintf("Hidden (%d)", n),
			toggle: &p.hiddenBtn,
		})
		if p.showHidden {
			addCards(idx, idx+n)
		}
	}

	if p.empty() {
		blocks = append(blocks, peopleBlock{
			kind: blockKindLabel,
			text: "No faces yet. Faces are detected in the background after indexing " +
				"(needs pv-face-detect: pipx install face_recognition).",
		})
	}
	return blocks, cardBlock
}

// layoutBlock draws one row of the outer list.
func (p *PeoplePane) layoutBlock(gtx layout.Context, th *Theme, b peopleBlock, cards []peopleCard) layout.Dimensions {
	switch b.kind {
	case blockKindToolbar:
		return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(8)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return p.layoutToolbar(gtx, th)
			})
	case blockKindLabel:
		return layout.Inset{Top: unit.Dp(32), Bottom: unit.Dp(32), Left: unit.Dp(16), Right: unit.Dp(16)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), b.text)
				lbl.Color = th.Muted
				lbl.Alignment = text.Middle
				return lbl.Layout(gtx)
			})
	case blockKindSuggestion:
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(8)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return p.layoutSuggestion(gtx, th, b)
			})
	case blockKindHeader:
		return layout.Inset{Top: unit.Dp(14), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(15), b.text)
				lbl.Color = th.Foreground
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			})
	case blockKindCards:
		return layout.Inset{Left: unit.Dp(4), Right: unit.Dp(4)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return p.layoutCardRow(gtx, th, cards, b.start, b.end)
			})
	case blockKindToggle:
		return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}.
			Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return material.Button(th.Theme, b.toggle, b.text).Layout(gtx)
			})
	}
	return layout.Dimensions{}
}

// layoutToolbar draws Regroup, the strictness cycle and the muted status.
func (p *PeoplePane) layoutToolbar(gtx layout.Context, th *Theme) layout.Dimensions {
	strictness := "Strictness: " + strictnessLabel(GetConfig().FaceStrictness)
	status := p.statusText()
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(material.Button(th.Theme, &p.regroupBtn, "Regroup faces").Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(material.Button(th.Theme, &p.strictBtn, strictness).Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), status)
			lbl.Color = th.Muted
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		}),
	)
}

// layoutSuggestion draws one "N photos might be <Name>" strip with its
// Review button. Its click was already drained by Layout.
func (p *PeoplePane) layoutSuggestion(gtx layout.Context, th *Theme, b peopleBlock) layout.Dimensions {
	if b.suggIdx < 0 || b.suggIdx >= len(p.snap.Suggestions) {
		return layout.Dimensions{}
	}
	s := p.snap.Suggestions[b.suggIdx]
	text := fmt.Sprintf("%d photos might be %s", distinctPhotoCount(s.Faces), s.Person.Name)
	click := p.suggClicks[b.suggIdx]
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13), text)
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(material.Button(th.Theme, click, "Review").Layout),
	)
}

// layoutCardRow draws one row of avatar cards.
func (p *PeoplePane) layoutCardRow(gtx layout.Context, th *Theme, cards []peopleCard, start, end int) layout.Dimensions {
	kids := make([]layout.FlexChild, 0, end-start)
	for i := start; i < end; i++ {
		c := cards[i]
		idx := i
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return p.layoutCard(gtx, th, c, idx)
		}))
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx, kids...)
}

// layoutCard draws one person/group card: round avatar, name and muted count,
// with the cursor's accent border when selected.
func (p *PeoplePane) layoutCard(gtx layout.Context, th *Theme, c peopleCard, idx int) layout.Dimensions {
	if idx >= 0 && idx < len(p.cardClicks) && p.cardClicks[idx].Clicked(gtx) {
		p.selected = idx
		p.activate(c)
	}

	name := "Who is this?"
	sub := ""
	face := c.group.Sample
	switch c.kind {
	case cardKindGroup:
		sub = faceCountLabel(c.group.Faces)
	case cardKindHidden:
		name = c.person.Name
		sub = photoCountLabel(c.person.Photos)
		face = c.person.Cover
	default:
		name = c.person.Name
		sub = photoCountLabel(c.person.Photos)
		face = c.person.Cover
	}

	var avatars *avatarCache
	if p.ctrl != nil {
		avatars = p.ctrl.Avatars()
	}
	cardW := gtx.Dp(unit.Dp(peopleCardDp))
	avatarPx := gtx.Dp(unit.Dp(peopleAvatarDp))
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(8), Left: unit.Dp(6), Right: unit.Dp(6)}

	click := p.cardClicks[idx]
	return click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = cardW
		gtx.Constraints.Max.X = cardW
		gtx.Constraints.Min.Y = 0
		dims := pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints = layout.Exact(image.Pt(avatarPx, avatarPx))
					return drawAvatar(gtx, th, avatars, face, avatarPx, true)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(5)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(12), name)
					lbl.Color = th.Foreground
					lbl.MaxLines = 1
					lbl.Alignment = text.Middle
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Label(th.Theme, unit.Sp(10), sub)
					lbl.Color = th.Muted
					lbl.MaxLines = 1
					lbl.Alignment = text.Middle
					return lbl.Layout(gtx)
				}),
			)
		})
		pointer.CursorPointer.Add(gtx.Ops)
		if idx == p.selected {
			drawBorder(gtx, image.Rectangle{Max: dims.Size}, gtx.Dp(unit.Dp(peopleSelWidth)), th.Accent)
		}
		return dims
	})
}

// groupSuggestions returns up to groupSuggestionMax existing person names
// fuzzy-matching query, best first. The group header uses it for its
// "this might already be <name>" chips.
func groupSuggestions(people []cache.Person, query string) []string {
	ql := strings.ToLower(strings.TrimSpace(query))
	type hit struct {
		name  string
		score int
	}
	hits := make([]hit, 0, len(people))
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		if ok, score := fuzzyScore(ql, strings.ToLower(p.Name)); ok {
			hits = append(hits, hit{name: p.Name, score: score})
		}
	}
	sortHits := func(a, b hit) int {
		if a.score != b.score {
			return a.score - b.score
		}
		return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	}
	// A tiny insertion sort keeps the top-N without pulling in slices just for
	// five names per frame.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && sortHits(hits[j], hits[j-1]) < 0; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	n := min(len(hits), groupSuggestionMax)
	out := make([]string, n)
	for i := range n {
		out[i] = hits[i].name
	}
	return out
}
