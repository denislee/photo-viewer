package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestShortPathCutsOnRuneBoundary covers U6-B5: the byte cut must never split
// a multi-byte rune.
func TestShortPathCutsOnRuneBoundary(t *testing.T) {
	if got := shortPath("/a/b.jpg"); got != "/a/b.jpg" {
		t.Fatalf("short path changed: %q", got)
	}
	// Offset the é so the plain len-47 cut lands on its second byte.
	for pad := range 4 {
		p := "/lib/" + strings.Repeat("x", 30+pad) + "/Café-" + strings.Repeat("é", 20) + ".jpg"
		got := shortPath(p)
		if !utf8.ValidString(got) {
			t.Fatalf("pad %d: shortPath produced invalid UTF-8: %q", pad, got)
		}
		if !strings.HasPrefix(got, "…") || !strings.HasSuffix(p, strings.TrimPrefix(got, "…")) {
			t.Fatalf("pad %d: %q is not an ellipsised suffix of %q", pad, got, p)
		}
	}
}
