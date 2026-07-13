package ui

import (
	"reflect"
	"testing"

	"gioui.org/io/event"
	"gioui.org/io/key"
)

// TestKeyFilterSets guards initiative G-08: the two static key-event filter
// sets were hoisted out of handleKeys (which runs every frame) to package-level
// vars built once. This asserts the hoisted sets are byte-for-byte identical to
// what the old inline construction produced — same members, same order — so key
// handling is unchanged.
func TestKeyFilterSets(t *testing.T) {
	// wantBase mirrors the original inline base literal.
	wantBase := []event.Filter{
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: key.NameUpArrow},
		key.Filter{Name: key.NameDownArrow},
		key.Filter{Name: key.NameReturn},
		key.Filter{Name: key.NamePageUp},
		key.Filter{Name: key.NamePageDown},
		key.Filter{Name: "K", Required: key.ModCtrl},
		key.Filter{Name: "[", Required: key.ModCtrl},
	}
	// wantFull mirrors the original: base literal followed by the extras that
	// were appended when the fuzzy-search palette was closed.
	wantFull := append(append([]event.Filter(nil), wantBase...),
		key.Filter{Name: key.NameLeftArrow},
		key.Filter{Name: key.NameRightArrow},
		key.Filter{Name: key.NameSpace},
		key.Filter{Name: "D"},
		key.Filter{Name: "E"},
		key.Filter{Name: "F"},
		key.Filter{Name: "G"},
		key.Filter{Name: "G", Required: key.ModShift},
		key.Filter{Name: "H"},
		key.Filter{Name: "I"},
		key.Filter{Name: "J"},
		key.Filter{Name: "K"},
		key.Filter{Name: "L"},
		key.Filter{Name: "M"},
		key.Filter{Name: "O"},
		key.Filter{Name: "o"},
		key.Filter{Name: "Q"},
		key.Filter{Name: "V"},
		key.Filter{Name: "["},
		key.Filter{Name: "]"},
		key.Filter{Name: "F", Required: key.ModCtrl},
		key.Filter{Name: "B", Required: key.ModCtrl},
		key.Filter{Name: "I", Required: key.ModCtrl},
		key.Filter{Name: "D", Required: key.ModCtrl},
		key.Filter{Name: "E", Required: key.ModCtrl},
		key.Filter{Name: "+", Required: key.ModCtrl},
		key.Filter{Name: "=", Required: key.ModCtrl},
		key.Filter{Name: "-", Required: key.ModCtrl},
		key.Filter{Name: ","},
	)

	if !reflect.DeepEqual(baseKeyFilters, wantBase) {
		t.Errorf("baseKeyFilters mismatch:\n got  %v\n want %v", baseKeyFilters, wantBase)
	}
	if !reflect.DeepEqual(fullKeyFilters, wantFull) {
		t.Errorf("fullKeyFilters mismatch:\n got  %v\n want %v", fullKeyFilters, wantFull)
	}

	// fullKeyFilters must start with exactly baseKeyFilters (in order) so the
	// palette-open subset is a prefix of the palette-closed set.
	if !reflect.DeepEqual(fullKeyFilters[:len(baseKeyFilters)], baseKeyFilters) {
		t.Errorf("fullKeyFilters does not start with baseKeyFilters:\n got %v", fullKeyFilters[:len(baseKeyFilters)])
	}

	// Building fullKeyFilters must not have mutated baseKeyFilters.
	if len(baseKeyFilters) != len(wantBase) {
		t.Errorf("baseKeyFilters length changed to %d, want %d", len(baseKeyFilters), len(wantBase))
	}
}
