package ui

import (
	"testing"

	"gioui.org/io/key"
)

func testAppViews() *appViews {
	v := &appViews{
		dups: &DuplicatesView{}, imports: &ImportView{}, organize: &OrganizeView{},
		settings: &SettingsView{}, search: &FuzzySearchView{},
		sdPrompt: &SDPromptView{}, webView: &WebServerView{},
	}
	v.modals = newModals(v, func() string { return "/lib" })
	return v
}

// TestTopModalPriority covers G4-09: for every combination of open flags the
// winner is the first open modal in the order the old hand-written chains in
// drawRoot and handleKeys used.
func TestTopModalPriority(t *testing.T) {
	v := testAppViews()
	order := []string{"search", "duplicates", "imports", "organize", "settings", "sdPrompt", "webServer"}
	flags := []*bool{&v.search.Open, &v.dups.Open, &v.imports.Open, &v.organize.Open,
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
		{"imports", key.Event{Name: "Q"}, true},
		{"imports", ctrlBracket, true},
		{"imports", key.Event{Name: "["}, false},
		{"organize", ctrlBracket, true},
		{"settings", key.Event{Name: ","}, true},
		{"settings", ctrlBracket, false},
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
