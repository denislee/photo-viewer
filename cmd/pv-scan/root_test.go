package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveRootMakesRelativeAbsolute covers S6-01: `pv-scan -root .` must
// key the index by absolute paths, as the GUI does.
func TestResolveRootMakesRelativeAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || got != wd {
		t.Fatalf("resolveRoot(.) = %q, want %q", got, wd)
	}
	if _, err := resolveRoot(""); err == nil {
		t.Fatal("resolveRoot(\"\") = nil error, want -root is required")
	}
}
