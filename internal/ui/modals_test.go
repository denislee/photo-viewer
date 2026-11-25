package ui

import (
	"testing"

	"gioui.org/io/event"
	"gioui.org/io/key"
)

func testAppViews() *appViews {
	v := &appViews{
		dups: &DuplicatesView{}, imports: &ImportView{}, organize: &OrganizeView{},
		settings: &SettingsView{}, search: &FuzzySearchView{},
		sdPrompt: &SDPromptView{}, webView: &WebServerView{},
		header: &PersonHeader{}, review: &ReviewView{},
		viewer: &Viewer{Open: true, faces: &viewerFaces{}},
	}
	v.modals = newModals(v, func() string { return "/lib" })
	return v
}

// TestTopModalPriority covers G4-09: for every combination of open flags the
// winner is the first open modal in the order the old hand-written chains in
// drawRoot and handleKeys used.
func TestTopModalPriority(t *testing.T) {
	v := testAppViews()
	order := []string{"search", "personPicker", "viewerPicker", "review", "duplicates", "imports", "organize", "settings", "sdPrompt", "webServer"}
	flags := []*bool{&v.search.Open, &v.header.picker.Open, &v.viewer.faces.picker.Open, &v.review.Open, &v.dups.Open, &v.imports.Open, &v.organize.Open,
		&v.settings.Open, &v.sdPrompt.Open, &v.webView.Open}
	if len(v.modals) != len(order) {
		t.Fatalf("%d modals, want %d", len(v.modals), len(order))
	}
	for mask := range 1 << len(flags) {
		want := ""
		for i, f := range flags {
			*f = mask&(1<<i) != 0
			if *f && want == "" {
				want = order[i]
			}
		}
		got := ""
		if m := v.topModal(); m != nil {
			got = m.name
		}
		if got != want {
			t.Fatalf("open mask %08b: top modal %q, want %q", mask, got, want)
		}
	}
}

// TestModalCloseKeys checks each modal's close keys, including the ones it
// must ignore (Q in the web-server modal would eat a password character).
func TestModalCloseKeys(t *testing.T) {
	ctrlBracket := key.Event{Name: "[", Modifiers: key.ModCtrl}
	cases := []struct {
		modal  string
		ev     key.Event
		closes bool
	}{
		{"imports", key.Event{Name: key.NameEscape}, true},
		{"imports", key.Event{Name: "Q"}, false},
		{"imports", ctrlBracket, true},
		{"imports", key.Event{Name: "["}, false},
		{"organize", ctrlBracket, true},
		{"organize", key.Event{Name: "Q"}, true},
		{"settings", key.Event{Name: ","}, false},
		{"settings", key.Event{Name: "Q"}, false},
		{"settings", key.Event{Name: key.NameEscape}, true},
		{"settings", ctrlBracket, true},
		{"sdPrompt", key.Event{Name: key.NameEscape}, true},
		{"webServer", key.Event{Name: "Q"}, false},
		{"webServer", ctrlBracket, true},
		{"search", key.Event{Name: "K", Modifiers: key.ModCtrl}, true},
		{"search", key.Event{Name: key.NameEscape}, true},
	}
	for _, c := range cases {
		v := testAppViews()
		var m *modal
		for i := range v.modals {
			if v.modals[i].name == c.modal {
				m = &v.modals[i]
			}
		}
		open := map[string]*bool{"imports": &v.imports.Open, "organize": &v.organize.Open,
			"settings": &v.settings.Open, "sdPrompt": &v.sdPrompt.Open,
			"webServer": &v.webView.Open, "search": &v.search.Open}[c.modal]
		*open = true
		m.keys(c.ev)
		if closed := !*open; closed != c.closes {
			t.Errorf("%s + %v: closed = %v, want %v", c.modal, c.ev.Name, closed, c.closes)
		}
	}
}

// TestTextInputModalKeyFilters covers G6-B1: while a modal with text editors is
// on top, only its close keys are registered, so typing q or "," into a
// Settings or Import editor reaches the editor instead of closing the modal.
func TestTextInputModalKeyFilters(t *testing.T) {
	has := func(fs []event.Filter, name key.Name) bool {
		for _, f := range fs {
			if kf, ok := f.(key.Filter); ok && kf.Name == name && kf.Required == 0 {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		open      func(v *appViews)
		textInput bool
	}{
		{func(v *appViews) { v.settings.Open = true }, true},
		{func(v *appViews) { v.imports.Open = true }, true},
		{func(v *appViews) { v.webView.Open = true }, true},
		{func(v *appViews) { v.organize.Open = true }, false},
		{func(v *appViews) {}, false},
	} {
		v := testAppViews()
		c.open(v)
		fs := keyFiltersFor(v)
		name := "none"
		if m := v.topModal(); m != nil {
			name = m.name
		}
		for _, k := range []key.Name{"Q", ",", key.NameLeftArrow, key.NameReturn, key.NameSpace} {
			if got := has(fs, k); got == c.textInput {
				t.Errorf("%s open: %q registered = %v, want %v", name, k, got, !c.textInput)
			}
		}
		if !has(fs, key.NameEscape) {
			t.Errorf("%s open: Esc not registered", name)
		}
	}
	v := testAppViews()
	v.search.Open = true
	if got := keyFiltersFor(v); len(got) != len(baseKeyFilters) {
		t.Errorf("search open: %d filters, want the base set", len(got))
	}
}
