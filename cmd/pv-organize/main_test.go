package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestOrganizeDryRunOutputIsStable pins organize's dry-run plan to a fixed
// order: the walk emits from parallel workers in no fixed order, so the sort
// (and the collision sequence that follows it) must make a 1-worker run and a
// 4-worker run print exactly the same plan.
func TestOrganizeDryRunOutputIsStable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, p := range []string{
		filepath.Join("src", "a", "img.jpg"),
		filepath.Join("src", "b", "img.jpg"),
		filepath.Join("src", "c", "IMG.JPG"),
		filepath.Join("src", "clip.mp4"),
		filepath.Join("src", "notes.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, "x")
	}
	if err := os.MkdirAll(filepath.Join("dst", "2024", "01", "02"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join("dst", "2024", "01", "02", "img.jpg"), "x")

	orig := mediaDate
	t.Cleanup(func() { mediaDate = orig })
	mediaDate = func(path string) time.Time {
		if filepath.Ext(path) == ".mp4" {
			return time.Date(2023, 12, 31, 10, 0, 0, 0, time.Local)
		}
		return time.Date(2024, 1, 2, 10, 0, 0, 0, time.Local)
	}

	absDst := filepath.Join(dir, "dst")
	want := "[Dry Run] " + filepath.Join("src", "a", "img.jpg") + " -> " +
		filepath.Join(absDst, "2024", "01", "02", "img_1.jpg") + "\n" +
		"[Dry Run] " + filepath.Join("src", "b", "img.jpg") + " -> " +
		filepath.Join(absDst, "2024", "01", "02", "img_2.jpg") + "\n" +
		"[Dry Run] " + filepath.Join("src", "c", "IMG.JPG") + " -> " +
		filepath.Join(absDst, "2024", "01", "02", "IMG.JPG") + "\n" +
		"[Dry Run] " + filepath.Join("src", "clip.mp4") + " -> " +
		filepath.Join(absDst, "2023", "12", "31", "clip.mp4") + "\n"

	for _, workers := range []int{1, 4} {
		var buf bytes.Buffer
		count, failures := organize(context.Background(), "src", absDst, true, workers, &buf)
		if count != 4 || failures != 0 {
			t.Fatalf("workers=%d: count=%d failures=%d, want 4 and 0", workers, count, failures)
		}
		if got := buf.String(); got != want {
			t.Fatalf("workers=%d: plan =\n%s\nwant\n%s", workers, got, want)
		}
	}

	if _, err := os.Stat(filepath.Join("src", "a", "img.jpg")); err != nil {
		t.Fatalf("dry run moved src/a/img.jpg: %v", err)
	}
	if _, err := os.Stat(filepath.Join("dst", "2023")); !os.IsNotExist(err) {
		t.Fatalf("dry run created dst/2023 (stat err = %v)", err)
	}
}
