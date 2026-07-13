package export

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
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

// TestCopyFileDurableAndAtomic covers the happy path of the crash-safe copy:
// the destination gets the exact source bytes, no ".tmp" is left behind, and
// the source is untouched (copyFile never removes it — moveFile does).
func TestCopyFileDurableAndAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "photo.jpg")
	writeFile(t, src, "PIXELS")
	dst := filepath.Join(dir, "out", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b := mustRead(t, dst); b != "PIXELS" {
		t.Fatalf("copied bytes = %q, want PIXELS", b)
	}
	if b := mustRead(t, src); b != "PIXELS" {
		t.Fatalf("source mutated by copy: %q", b)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("leftover temp after copy: %v", err)
	}
}

// TestMoveFileSameFilesystem exercises the fast path: within one filesystem the
// rename succeeds, the destination holds the bytes, and the source is gone.
func TestMoveFileSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	writeFile(t, src, "BYTES")
	dst := filepath.Join(dir, "dst", "clip.mp4")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if b := mustRead(t, dst); b != "BYTES" {
		t.Fatalf("moved bytes = %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present after move: %v", err)
	}
}

// TestStatTaken documents the non-hang guarantee: a missing path is free, an
// existing one is taken.
func TestStatTaken(t *testing.T) {
	dir := t.TempDir()
	if statTaken(filepath.Join(dir, "nope.jpg")) {
		t.Fatalf("missing path reported as taken")
	}
	present := filepath.Join(dir, "yes.jpg")
	writeFile(t, present, "x")
	if !statTaken(present) {
		t.Fatalf("existing path reported as free")
	}
}

// TestAvoidCollisionSuffixes: an occupied base name must resolve to the "_1"
// variant, and a free name must be returned unchanged.
func TestAvoidCollisionSuffixes(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "a.jpg")
	writeFile(t, base, "OLD")

	got, err := avoidCollision(base, statTaken)
	if err != nil {
		t.Fatalf("avoidCollision: %v", err)
	}
	if want := filepath.Join(dir, "a_1.jpg"); got != want {
		t.Fatalf("collision resolved to %s, want %s", got, want)
	}

	free := filepath.Join(dir, "b.jpg")
	got, err = avoidCollision(free, statTaken)
	if err != nil {
		t.Fatalf("avoidCollision(free): %v", err)
	}
	if got != free {
		t.Fatalf("free path changed: got %s want %s", got, free)
	}
}

// TestAvoidCollisionTerminates is the anti-hang guarantee: a predicate that
// always reports "taken" (the stand-in for an unreadable destination directory
// whose stat keeps erroring) must make avoidCollision give up with an error
// instead of looping forever.
func TestAvoidCollisionTerminates(t *testing.T) {
	alwaysTaken := func(string) bool { return true }
	if _, err := avoidCollision("/nowhere/x.jpg", alwaysTaken); err == nil {
		t.Fatalf("expected avoidCollision to fail (not hang) when every name is taken")
	}
}

// TestDryRunPlanDistinctForSameNamedSources: the dry-run planner (claimed map
// OR on-disk stat) must resolve two identically-named favorites to distinct
// destinations — base, then _1 — exactly as a real run's on-disk collision
// check would. This is what makes -dry-run's printed plan match a real run
// instead of printing the same path twice.
func TestDryRunPlanDistinctForSameNamedSources(t *testing.T) {
	dir := t.TempDir()
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || statTaken(p) }

	first, err := avoidCollision(filepath.Join(dir, "img.jpg"), taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true
	second, err := avoidCollision(filepath.Join(dir, "img.jpg"), taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[second] = true

	if first == second {
		t.Fatalf("two same-named sources planned to the same destination %s", first)
	}
	if want := filepath.Join(dir, "img.jpg"); first != want {
		t.Fatalf("first = %s, want %s", first, want)
	}
	if want := filepath.Join(dir, "img_1.jpg"); second != want {
		t.Fatalf("second = %s, want %s", second, want)
	}
}

// TestDryRunPlanMatchesRealCopy: the destination the dry-run planner picks for a
// collision must be the one a real copy actually lands on, so -dry-run never
// lies about the outcome.
func TestDryRunPlanMatchesRealCopy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.jpg"), "OLD")

	// Dry-run planning path.
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || statTaken(p) }
	planned, err := avoidCollision(filepath.Join(dir, "a.jpg"), taken)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	claimed[planned] = true

	// Real run: collision-resolve against the disk, then copy there.
	src := filepath.Join(dir, "src.jpg")
	writeFile(t, src, "NEW")
	real, err := avoidCollision(filepath.Join(dir, "a.jpg"), statTaken)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if planned != real {
		t.Fatalf("dry-run planned %s but real run resolved %s", planned, real)
	}
	if err := copyFile(src, real); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if want := filepath.Join(dir, "a_1.jpg"); real != want {
		t.Fatalf("destination = %s, want %s", real, want)
	}
	if b := mustRead(t, real); b != "NEW" {
		t.Fatalf("copied bytes = %q", b)
	}
	if b := mustRead(t, filepath.Join(dir, "a.jpg")); b != "OLD" {
		t.Fatalf("pre-existing file clobbered: %q", b)
	}
}

// writePNG writes a small solid-colour PNG to p, giving recompressImage a real
// decodable source that needs no external tools.
func writePNG(t *testing.T, p string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", p, err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode %s: %v", p, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", p, err)
	}
}

// TestRecompressImagePreservesMtime is the S-09 guarantee: a recompressed image
// export carries the source's mtime (capture time), not the encode time, so it
// sorts correctly in date-ordered browsers. Uses the pure-Go image path (PNG →
// JPEG), so it needs no external tools and runs unconditionally.
func TestRecompressImagePreservesMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	writePNG(t, src, 64, 48)

	// A distinctive, whole-second past mtime: whole seconds dodge sub-second
	// truncation on coarser filesystems, and being years in the past makes the
	// "stamped with now" regression impossible to pass the tolerance below.
	want := time.Date(2019, 3, 14, 9, 26, 53, 0, time.UTC)
	if err := os.Chtimes(src, want, want); err != nil {
		t.Fatalf("chtimes src: %v", err)
	}

	dst := filepath.Join(dir, "out.jpg")
	if err := recompressImage(src, dst, 32, 82); err != nil {
		t.Fatalf("recompressImage: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	// Tolerance covers coarse mtime resolution (e.g. FAT's 2 s) without admitting
	// an encode-time (~now) mtime, which would be years off.
	if d := info.ModTime().Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("recompressed mtime = %v, want ~%v (delta %v)", info.ModTime(), want, d)
	}
}
