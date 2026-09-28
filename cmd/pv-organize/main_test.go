package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/fsutil"
)

// The move itself (collisions, cross-device copy, cancellation) is
// fsutil.MoveUnique and is tested there; these tests pin the dry-run planner
// to it.

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// TestDryRunPlanMatchesRealRun: the dry-run planner and a real move must resolve
// a collision to the same destination name, so `-dry-run` never lies about what
// a real run will do.
func TestDryRunPlanMatchesRealRun(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destDir, "a.jpg"), "OLD")

	planned, err := planDest(destDir, "a.jpg", map[string]bool{})
	if err != nil {
		t.Fatalf("planDest: %v", err)
	}

	src := filepath.Join(dir, "a.jpg")
	writeFile(t, src, "NEW")
	real, err := fsutil.MoveUnique(context.Background(), src, destDir, "a.jpg")
	if err != nil {
		t.Fatalf("MoveUnique: %v", err)
	}
	if planned != real {
		t.Fatalf("dry-run planned %s but real run produced %s", planned, real)
	}
	if want := filepath.Join(destDir, "a_1.jpg"); real != want {
		t.Fatalf("destination = %s, want %s", real, want)
	}
}

// TestDryRunPlanDistinctForSameNamedSources: two sources with the same base
// name must plan to distinct destinations (base, then _1) via the claimed set,
// matching what the atomic real run would do.
func TestDryRunPlanDistinctForSameNamedSources(t *testing.T) {
	destDir := t.TempDir()
	claimed := map[string]bool{}

	first, err := planDest(destDir, "img.jpg", claimed)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true
	second, err := planDest(destDir, "img.jpg", claimed)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(destDir, "img.jpg"); first != want {
		t.Fatalf("first = %s, want %s", first, want)
	}
	if want := filepath.Join(destDir, "img_1.jpg"); second != want {
		t.Fatalf("second = %s, want %s", second, want)
	}
}
