package webserver

import (
	"strings"
	"testing"
)

// TestKeyHandlersSkipModifiers pins the modifier guard in both page scripts:
// without it the viewer hijacks Ctrl+L, Ctrl+K, Ctrl+F and Cmd+[ / ] (W6-B3).
func TestKeyHandlersSkipModifiers(t *testing.T) {
	const guard = "if (e.ctrlKey || e.metaKey || e.altKey) return;"
	for name, js := range map[string]string{"grid.js": gridJS, "viewer.js": viewerJS} {
		if !strings.Contains(js, guard) {
			t.Errorf("%s keydown handler has no modifier guard", name)
		}
	}
}
