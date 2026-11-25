package ui

import (
	"testing"

	"gioui.org/io/key"

	"github.com/dns/photo-viewer/internal/cache"
)

// peoplePaneTestSnapshot is a library with two named people, two unnamed
// groups, two small groups and one hidden person, i.e. one card of every kind
// the overview can draw.
func peoplePaneTestSnapshot() PeopleSnapshot {
	return PeopleSnapshot{
		People: []cache.Person{
			{ID: 10, Name: "Ana", Photos: 7},
			{ID: 11, Name: "Bob", Photos: 3},
		},
		Hidden: []cache.Person{
			{ID: 12, Name: "Carla", Photos: 1, Hidden: true},
		},
		Groups: []cache.Group{
			{ClusterID: 20, Faces: 5},
			{ClusterID: 21, Faces: 3},
		},
		SmallGroups: []cache.Group{
			{ClusterID: 30, Faces: 2},
			{ClusterID: 31, Faces: 1},
		},
	}
}

// cardRef identifies a card in the flat selection order.
type cardRef struct {
	kind peopleCardKind
	id   int64
}

func cardRefs(cards []peopleCard) []cardRef {
	out := make([]cardRef, len(cards))
	for i, c := range cards {
		if c.kind == cardKindGroup {
			out[i] = cardRef{kind: c.kind, id: c.group.ClusterID}
		} else {
			out[i] = cardRef{kind: c.kind, id: c.person.ID}
		}
	}
	return out
}

func assertCards(t *testing.T, got, want []cardRef) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("cards = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("card %d = %+v, want %+v (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestPeoplePaneCardsOrder checks the flat card order follows the drawn
// sections: people, groups, then small groups and hidden people only while
// their toggles are on.
func TestPeoplePaneCardsOrder(t *testing.T) {
	p := &PeoplePane{}
	p.setSnapshotForTest(peoplePaneTestSnapshot(), 2)

	assertCards(t, cardRefs(p.cards()), []cardRef{
		{cardKindPerson, 10},
		{cardKindPerson, 11},
		{cardKindGroup, 20},
		{cardKindGroup, 21},
	})

	p.showSmall = true
	assertCards(t, cardRefs(p.cards()), []cardRef{
		{cardKindPerson, 10},
		{cardKindPerson, 11},
		{cardKindGroup, 20},
		{cardKindGroup, 21},
		{cardKindGroup, 30},
		{cardKindGroup, 31},
	})

	p.showHidden = true
	assertCards(t, cardRefs(p.cards()), []cardRef{
		{cardKindPerson, 10},
		{cardKindPerson, 11},
		{cardKindGroup, 20},
		{cardKindGroup, 21},
		{cardKindGroup, 30},
		{cardKindGroup, 31},
		{cardKindHidden, 12},
	})

	// Turning small groups back off leaves the hidden card last again.
	p.showSmall = false
	assertCards(t, cardRefs(p.cards()), []cardRef{
		{cardKindPerson, 10},
		{cardKindPerson, 11},
		{cardKindGroup, 20},
		{cardKindGroup, 21},
		{cardKindHidden, 12},
	})
}

// TestPeoplePaneSelection checks H/J/K/L move the cursor one card, one row,
// clamped to the visible cards, using the column count from the last Layout.
func TestPeoplePaneSelection(t *testing.T) {
	p := &PeoplePane{}
	p.setSnapshotForTest(peoplePaneTestSnapshot(), 3)
	p.showSmall = true
	p.showHidden = true

	selectTo := func(keyName key.Name) int {
		t.Helper()
		if !p.Key(key.Event{Name: keyName}) {
			t.Fatalf("%q was not handled", keyName)
		}
		return p.selected
	}

	if got := selectTo("L"); got != 1 {
		t.Fatalf("L moved to %d, want 1", got)
	}
	if got := selectTo(key.NameLeftArrow); got != 0 {
		t.Fatalf("Left moved to %d, want 0", got)
	}
	if got := selectTo("J"); got != 3 {
		t.Fatalf("J moved to %d, want 3 (one row of 3)", got)
	}
	if got := selectTo(key.NameUpArrow); got != 0 {
		t.Fatalf("Up moved to %d, want 0", got)
	}
	selectTo("L")
	selectTo("L")
	if p.selected != 2 {
		t.Fatalf("L,L landed at %d, want 2", p.selected)
	}
	// 7 visible cards: 2 people + 2 groups + 2 small groups + 1 hidden.
	if got := selectTo("J"); got != 5 {
		t.Fatalf("J from 2 moved to %d, want 5", got)
	}
	// Down at the bottom row clamps to the last card.
	if got := selectTo(key.NameDownArrow); got != 6 {
		t.Fatalf("J past the end moved to %d, want 6", got)
	}
	if got := selectTo("K"); got != 3 {
		t.Fatalf("K moved to %d, want 3", got)
	}

	// Return is consumed even though no controller is wired to navigate.
	if !p.Key(key.Event{Name: key.NameReturn}) {
		t.Fatal("Return was not handled")
	}
	if !p.Key(key.Event{Name: "R"}) {
		t.Fatal("R was not handled")
	}
	if !p.Key(key.Event{Name: "Z", Modifiers: key.ModCtrl}) {
		t.Fatal("Ctrl+Z was not handled")
	}
}

// TestPersonHeaderHeightDp checks the strip only reserves space over a
// person/group sentinel and grows for the merge prompt.
func TestPersonHeaderHeightDp(t *testing.T) {
	h := NewPersonHeader(nil, nil)

	cases := []struct {
		dir  string
		want int
	}{
		{"/home/photos/2024", 0},
		{PeopleView, 0},
		{PersonDir(7), personHeaderDp},
		{GroupDir(3), personHeaderDp},
	}
	for _, c := range cases {
		if got := h.HeightDp(c.dir); got != c.want {
			t.Errorf("HeightDp(%q) = %d, want %d", c.dir, got, c.want)
		}
	}

	h.setMergeForTest(7, 9, "Ana")
	if got := h.HeightDp(PersonDir(7)); got != personHeaderMergeDp {
		t.Errorf("HeightDp(person with merge prompt) = %d, want %d", got, personHeaderMergeDp)
	}
	// The prompt belongs to person 7 only.
	if got := h.HeightDp(PersonDir(8)); got != personHeaderDp {
		t.Errorf("HeightDp(other person) = %d, want %d", got, personHeaderDp)
	}
	if got := h.HeightDp(GroupDir(7)); got != personHeaderDp {
		t.Errorf("HeightDp(group with the same id) = %d, want %d", got, personHeaderDp)
	}
}

// TestPeoplePaneGroupSuggestions checks the group name chips: fuzzy matches
// only, best first, never more than groupSuggestionMax.
func TestPeoplePaneGroupSuggestions(t *testing.T) {
	people := []cache.Person{
		{Name: "Ana"},
		{Name: "Ana Maria"},
		{Name: "Bogdan"},
		{Name: "Carlos"},
		{Name: "Daniela"},
		{Name: "Fabiana"},
		{Name: "Gabriel"},
		{Name: "Helena"},
	}

	// "ana" matches the two Anas by prefix and Daniela/Fabiana as a
	// subsequence; the chip list is capped at groupSuggestionMax.
	got := groupSuggestions(people, "ana")
	if len(got) != 4 {
		t.Fatalf("groupSuggestions(ana) = %v, want the 4 matching names", got)
	}
	if got[0] != "Ana" || got[1] != "Ana Maria" {
		t.Fatalf("groupSuggestions(ana) = %v, want Ana then Ana Maria first", got)
	}

	// The query is lowercased before matching, so an uppercase query behaves
	// the same.
	upper := groupSuggestions(people, "ANA")
	for i := range got {
		if upper[i] != got[i] {
			t.Fatalf("groupSuggestions(ANA) = %v, want %v", upper, got)
		}
	}

	// An empty query still caps the chips.
	if got := groupSuggestions(people, ""); len(got) != groupSuggestionMax {
		t.Fatalf("groupSuggestions(\"\") returned %d chips, want %d", len(got), groupSuggestionMax)
	}
	// A broad query matches everybody but is still capped.
	if got := groupSuggestions(people, "a"); len(got) != groupSuggestionMax {
		t.Fatalf("groupSuggestions(a) returned %d chips, want at most %d", len(got), groupSuggestionMax)
	}

	// A query no name contains yields no chips.
	if got := groupSuggestions(people, "zzz"); len(got) != 0 {
		t.Fatalf("groupSuggestions(zzz) = %v, want none", got)
	}
}
