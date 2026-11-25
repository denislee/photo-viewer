package ui

import (
	"testing"

	"gioui.org/io/key"

	"github.com/dns/photo-viewer/internal/cache"
)

// pickPeople is the person list the picker tests filter:
// names chosen so "an" matches Ana and Ana Maria, "bo" matches Bogdan.
func pickPeople() []cache.Person {
	return []cache.Person{
		{ID: 1, Name: "Ana"},
		{ID: 2, Name: "Ana Maria"},
		{ID: 3, Name: "Bogdan"},
		{ID: 4, Name: "Carlos"},
	}
}

// TestPickerMatchesFuzzy checks the picker's filtering: fuzzy score order,
// the "+ New person" row only for a genuinely new name, and the 12-item cap.
func TestPickerMatchesFuzzy(t *testing.T) {
	var p personPicker
	p.editor.SingleLine = true

	p.editor.SetText("an")
	items := p.matches(pickPeople())
	if len(items) == 0 {
		t.Fatal("matches(an) returned nothing")
	}
	if items[0].PersonID != 1 || items[0].Name != "Ana" {
		t.Fatalf("first match for %q = %+v, want Ana (exact prefix scores best)", "an", items[0])
	}
	// "an" is not an existing name, so the create row must be offered.
	last := items[len(items)-1]
	if !last.New || last.Name != "an" {
		t.Fatalf("last match for %q = %+v, want the + New person row", "an", last)
	}
	for _, it := range items[:len(items)-1] {
		if it.New {
			t.Fatalf("+ New person row not at the end: %+v", items)
		}
	}

	// Case-insensitive exact match: no create row.
	p.editor.SetText("ana")
	items = p.matches(pickPeople())
	for _, it := range items {
		if it.New {
			t.Fatalf("matches(ana) offered a new person: %+v", items)
		}
	}
	if len(items) != 2 || items[0].PersonID != 1 {
		t.Fatalf("matches(ana) = %+v, want Ana then Ana Maria", items)
	}

	// Empty query lists everybody, and still no create row.
	p.editor.SetText("")
	if got := p.matches(pickPeople()); len(got) != len(pickPeople()) {
		t.Fatalf("matches(\"\") = %d items, want %d", len(got), len(pickPeople()))
	}

	// A query that names nobody leaves just the create row.
	p.editor.SetText("Zoe")
	items = p.matches(pickPeople())
	if len(items) != 1 || !items[0].New || items[0].Name != "Zoe" {
		t.Fatalf("matches(Zoe) = %+v, want only the + New person row", items)
	}
}

// TestPickerMatchesCap checks the 12-item cap, including the create row.
func TestPickerMatchesCap(t *testing.T) {
	var p personPicker
	people := make([]cache.Person, 0, 20)
	for i := range 20 {
		people = append(people, cache.Person{ID: int64(i + 1), Name: "Person " + string(rune('A'+i))})
	}
	p.editor.SetText("person")
	if got := p.matches(people); len(got) != pickerMaxItems {
		t.Fatalf("matches(person) = %d items, want the %d cap", len(got), pickerMaxItems)
	}
	if got := p.matches(people); got[len(got)-1].New {
		t.Fatalf("cap should drop the create row when the list is already full: %+v", got)
	}
}

// reviewFaces builds n faces with distinct ids and paths.
func reviewFaces(n int) []cache.FaceInfo {
	faces := make([]cache.FaceInfo, n)
	for i := range faces {
		faces[i] = cache.FaceInfo{Face: cache.Face{ID: int64(i + 1), Path: "/photos/f" + string(rune('0'+i)) + ".jpg"}}
	}
	return faces
}

// TestReviewCursorMovement covers h/j/k/l movement with a fixed column count
// and clamping at both ends of the face list.
func TestReviewCursorMovement(t *testing.T) {
	r := NewReviewView(nil, nil)
	r.Open = true
	r.setFacesForTest(reviewFaces(9), 3)

	move := func(name key.Name) {
		t.Helper()
		if !r.Key(key.Event{Name: name}) {
			t.Fatalf("Key(%q) not handled", name)
		}
	}

	move("L")
	if r.cursor != 1 {
		t.Fatalf("after l: cursor = %d, want 1", r.cursor)
	}
	move("J")
	if r.cursor != 4 {
		t.Fatalf("after j (3 cols): cursor = %d, want 4", r.cursor)
	}
	move("H")
	if r.cursor != 3 {
		t.Fatalf("after h: cursor = %d, want 3", r.cursor)
	}
	move("K")
	if r.cursor != 0 {
		t.Fatalf("after k (3 cols): cursor = %d, want 0", r.cursor)
	}
	// Clamp at the top.
	move("K")
	move("H")
	if r.cursor != 0 {
		t.Fatalf("cursor escaped the top: %d", r.cursor)
	}
	// Arrow aliases, and clamping at the bottom.
	for range 20 {
		move(key.NameRightArrow)
	}
	if r.cursor != len(r.faces)-1 {
		t.Fatalf("cursor escaped the bottom: %d", r.cursor)
	}
}

// TestReviewTargets covers the action set: marked faces when any are marked,
// otherwise just the cursor face.
func TestReviewTargets(t *testing.T) {
	r := NewReviewView(nil, nil)
	r.Open = true
	r.setFacesForTest(reviewFaces(5), 3)

	if got := r.targets(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("targets() at cursor 0 = %v, want [1]", got)
	}

	r.Key(key.Event{Name: "L"})
	r.Key(key.Event{Name: "J"})
	if r.cursor != 4 {
		t.Fatalf("cursor = %d, want 4", r.cursor)
	}
	if got := r.targets(); len(got) != 1 || got[0] != 5 {
		t.Fatalf("targets() at cursor 4 = %v, want [5]", got)
	}

	// Mark the cursor face and another one.
	r.Key(key.Event{Name: "V"})
	r.cursor = 1
	r.Key(key.Event{Name: "V"})
	got := r.targets()
	if len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Fatalf("targets() with marks = %v, want [2 5] in face order", got)
	}

	// Unmarking everything falls back to the cursor face.
	r.cursor = 4
	r.Key(key.Event{Name: "V"})
	r.cursor = 1
	r.Key(key.Event{Name: "V"})
	if got := r.targets(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("targets() after unmarking = %v, want the cursor face [2]", got)
	}
}

// TestReviewSyncDropsVanishedFaces checks the post-reload pruning: the cursor
// is clamped and marks of faces that left the set are dropped.
func TestReviewSyncDropsVanishedFaces(t *testing.T) {
	r := NewReviewView(nil, nil)
	r.setFacesForTest(reviewFaces(5), 3)
	r.cursor = 4
	r.marked = map[int64]bool{4: true, 5: true}

	r.syncFaces(reviewFaces(2))
	if r.cursor != 1 {
		t.Fatalf("cursor = %d after reload, want 1", r.cursor)
	}
	if len(r.marked) != 0 {
		t.Fatalf("marks survived the reload: %v", r.marked)
	}
}
