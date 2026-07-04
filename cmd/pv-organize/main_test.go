package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFile is a tiny helper: create p with the given bytes, failing the test
// on error.
func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestMoveFileCollisionSuffixesWithoutOverwrite is the core data-safety
// guarantee: a name collision must produce a suffixed destination and must NOT
// overwrite the bytes of the file already sitting at the base name.
func TestMoveFileCollisionSuffixesWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	// An existing, precious file at the base name.
	existing := filepath.Join(destDir, "photo.jpg")
	writeFile(t, existing, "EXISTING")

	// A different source that maps to the same base name.
	src := filepath.Join(dir, "photo.jpg")
	writeFile(t, src, "NEW")

	got, err := moveFile(src, destDir, "photo.jpg")
	if err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	want := filepath.Join(destDir, "photo_1.jpg")
	if got != want {
		t.Fatalf("destination = %s, want %s", got, want)
	}
	// The pre-existing file's bytes must be untouched.
	if b := mustRead(t, existing); b != "EXISTING" {
		t.Fatalf("existing file was clobbered: %q", b)
	}
	// The moved file lands under the suffixed name with its own bytes.
	if b := mustRead(t, want); b != "NEW" {
		t.Fatalf("moved file bytes = %q, want NEW", b)
	}
	// Source is gone after a successful move.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after move: %v", err)
	}
}

// TestMoveFileNoCollision covers the happy path: a free destination is claimed
// under the bare base name and the source is removed.
func TestMoveFileNoCollision(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "clip.mp4")
	writeFile(t, src, "BYTES")

	got, err := moveFile(src, destDir, "clip.mp4")
	if err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if want := filepath.Join(destDir, "clip.mp4"); got != want {
		t.Fatalf("destination = %s, want %s", got, want)
	}
	if b := mustRead(t, got); b != "BYTES" {
		t.Fatalf("moved bytes = %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
}

// TestCrossDeviceMoveCopiesAndRemovesSource exercises the EXDEV fallback helper
// directly (t.TempDir is a single filesystem, so a real cross-device EXDEV
// can't be provoked portably — the EXDEV branch of moveFile simply routes here,
// which is covered by reasoning). It must leave the destination with the exact
// source bytes and the source removed.
func TestCrossDeviceMoveCopiesAndRemovesSource(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "movie.mov")
	writeFile(t, src, "CONTENT-1234")
	dst := filepath.Join(destDir, "movie.mov")

	got, err := crossDeviceMove(src, destDir, dst)
	if err != nil {
		t.Fatalf("crossDeviceMove: %v", err)
	}
	if got != dst {
		t.Fatalf("destination = %s, want %s", got, dst)
	}
	if b := mustRead(t, dst); b != "CONTENT-1234" {
		t.Fatalf("destination bytes = %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after cross-device move: %v", err)
	}
	// No stray temp files left behind in the destination dir.
	ents, _ := os.ReadDir(destDir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// TestCrossDeviceMoveRefusesOverwrite: if the destination already exists, the
// copy fallback must refuse (surfacing EEXIST for the caller's suffix loop),
// keep the existing bytes, and leave the source intact.
func TestCrossDeviceMoveRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(destDir, "keep.jpg")
	writeFile(t, dst, "KEEP")
	src := filepath.Join(dir, "keep.jpg")
	writeFile(t, src, "INCOMING")

	_, err := crossDeviceMove(src, destDir, dst)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected file-exists error, got %v", err)
	}
	if b := mustRead(t, dst); b != "KEEP" {
		t.Fatalf("existing destination was clobbered: %q", b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should be intact on refused overwrite: %v", err)
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

	// Dry-run planning path.
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || statTaken(p) }
	planned, err := nextFreeName(destDir, "a.jpg", taken)
	if err != nil {
		t.Fatalf("nextFreeName: %v", err)
	}
	claimed[planned] = true

	// Real move of a same-named source.
	src := filepath.Join(dir, "a.jpg")
	writeFile(t, src, "NEW")
	real, err := moveFile(src, destDir, "a.jpg")
	if err != nil {
		t.Fatalf("moveFile: %v", err)
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
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || statTaken(p) }

	first, err := nextFreeName(destDir, "img.jpg", taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true
	second, err := nextFreeName(destDir, "img.jpg", taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[second] = true

	if first == second {
		t.Fatalf("two same-named sources planned to the same destination %s", first)
	}
	if want := filepath.Join(destDir, "img.jpg"); first != want {
		t.Fatalf("first = %s, want %s", first, want)
	}
	if want := filepath.Join(destDir, "img_1.jpg"); second != want {
		t.Fatalf("second = %s, want %s", second, want)
	}
}

// TestStatTaken documents the non-hang guarantee: a missing path is free, an
// existing one is taken.
func TestStatTaken(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.jpg")
	if statTaken(missing) {
		t.Fatalf("missing path reported as taken")
	}
	present := filepath.Join(dir, "yes.jpg")
	writeFile(t, present, "x")
	if !statTaken(present) {
		t.Fatalf("existing path reported as free")
	}
}
