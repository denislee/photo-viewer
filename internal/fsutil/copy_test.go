package fsutil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// noTemps fails if a *.tmp sidecar is left in dir.
func noTemps(t *testing.T, dir string) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) > 0 {
		t.Errorf("temp files left behind: %v", m)
	}
}

// errAfterReader yields up to fail good bytes, then fails — simulating a
// truncated read (yanked SD card, ZIP corruption) partway through a copy.
type errAfterReader struct {
	data []byte
	pos  int
	fail int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.fail {
		return 0, errors.New("injected read failure")
	}
	n := copy(p, r.data[r.pos:min(r.fail, len(r.data))])
	r.pos += n
	return n, nil
}

// TestWriteUnique: the happy path publishes the full content under the bare
// name with umask-governed permissions, a second write of the same name lands
// on _1 without touching the first, and no temp is left behind.
func TestWriteUnique(t *testing.T) {
	dir := t.TempDir()
	got, err := WriteUnique(context.Background(), dir, "IMG_0001.jpg", bytes.NewReader([]byte("first")))
	if err != nil {
		t.Fatalf("WriteUnique: %v", err)
	}
	if want := filepath.Join(dir, "IMG_0001.jpg"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || info.Mode().Perm() == 0o600 {
		t.Errorf("mode = %v (%v); want the umask default, not CreateTemp's 0600", info.Mode(), err)
	}

	second, err := WriteUnique(context.Background(), dir, "IMG_0001.jpg", bytes.NewReader([]byte("second")))
	if err != nil {
		t.Fatalf("WriteUnique (collision): %v", err)
	}
	if want := filepath.Join(dir, "IMG_0001_1.jpg"); second != want {
		t.Errorf("collision path = %q, want %q", second, want)
	}
	if readFile(t, got) != "first" || readFile(t, second) != "second" {
		t.Errorf("contents = %q, %q", readFile(t, got), readFile(t, second))
	}
	noTemps(t, dir)
}

// TestWriteUniqueNoPartialOnError is the crash-safety contract behind U-01: a
// write that fails midway must never leave a truncated file under a media name
// (the next inbox walk would file it into the library as valid), and must
// clean up its temp.
func TestWriteUniqueNoPartialOnError(t *testing.T) {
	dir := t.TempDir()
	r := &errAfterReader{data: []byte("partial-then-boom-more-bytes"), fail: 8}
	if _, err := WriteUnique(context.Background(), dir, "IMG_0002.jpg", r); err == nil {
		t.Fatal("expected the injected read error, got nil")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dir not empty after failed write: %v", entries)
	}
}

// TestWriteUniqueStaleTemp: a temp orphaned by a crash doesn't block the name.
func TestWriteUniqueStaleTemp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.jpg.tmp"), "orphan")
	got, err := WriteUnique(context.Background(), dir, "a.jpg", bytes.NewReader([]byte("new")))
	if err != nil {
		t.Fatalf("WriteUnique: %v", err)
	}
	if got != filepath.Join(dir, "a.jpg") || readFile(t, got) != "new" {
		t.Errorf("got %q = %q", got, readFile(t, got))
	}
	if readFile(t, filepath.Join(dir, "a.jpg.tmp")) != "orphan" {
		t.Error("someone else's temp was touched")
	}
}

// TestCopyUniqueCancelled is the S-23 guard: an interrupt aborts the copy,
// leaves nothing in the destination, and keeps the source intact.
func TestCopyUniqueCancelled(t *testing.T) {
	srcDir, destDir := t.TempDir(), t.TempDir()
	src := filepath.Join(srcDir, "clip.mp4")
	writeFile(t, src, "video bytes")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := CopyUnique(ctx, src, destDir, "clip.mp4"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Errorf("destination not empty after cancelled copy: %v", entries)
	}
	if readFile(t, src) != "video bytes" {
		t.Error("source damaged")
	}
}

// TestMoveUniqueCollision is the core data-safety guarantee: a name collision
// produces a suffixed destination and never overwrites the occupant.
func TestMoveUniqueCollision(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "dst")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destDir, "photo.jpg"), "EXISTING")
	src := filepath.Join(dir, "photo.jpg")
	writeFile(t, src, "NEW")

	got, err := MoveUnique(context.Background(), src, destDir, "photo.jpg")
	if err != nil {
		t.Fatalf("MoveUnique: %v", err)
	}
	if want := filepath.Join(destDir, "photo_1.jpg"); got != want {
		t.Fatalf("destination = %s, want %s", got, want)
	}
	if readFile(t, filepath.Join(destDir, "photo.jpg")) != "EXISTING" {
		t.Error("occupant clobbered")
	}
	if readFile(t, got) != "NEW" {
		t.Error("moved file has the wrong bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after move: %v", err)
	}
}

// TestMoveUniqueCrossDevice drives the EXDEV branch for real by moving from
// /dev/shm to a TempDir on a different filesystem: the file lands on the next
// free name via the durable copy, the occupant is untouched, and the source is
// removed only after that.
func TestMoveUniqueCrossDevice(t *testing.T) {
	destDir := t.TempDir()
	srcDir, err := os.MkdirTemp("/dev/shm", "pv-fsutil-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(srcDir) })
	if SameDevice(srcDir, destDir) {
		t.Skip("/dev/shm and TempDir share a filesystem; EXDEV path not reachable")
	}

	writeFile(t, filepath.Join(destDir, "clip.mov"), "occupant")
	src := filepath.Join(srcDir, "clip.mov")
	writeFile(t, src, "from the card")

	dest, err := MoveUnique(context.Background(), src, destDir, "clip.mov")
	if err != nil {
		t.Fatalf("MoveUnique: %v", err)
	}
	if want := filepath.Join(destDir, "clip_1.mov"); dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	if readFile(t, dest) != "from the card" {
		t.Errorf("dest content = %q", readFile(t, dest))
	}
	if readFile(t, filepath.Join(destDir, "clip.mov")) != "occupant" {
		t.Error("occupant clobbered")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be removed after a durable copy, stat err = %v", err)
	}
	noTemps(t, destDir)
}

// TestFreeNameMatchesClaim: FreeName with Taken plans the same name a real
// claim then takes, and a claimed set keeps two plans for one name distinct.
func TestFreeNameMatchesClaim(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.jpg"), "OLD")
	cand := func(n int) string { return SuffixName(dir, "a.jpg", n) }
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || Taken(p) }

	first, err := FreeName(cand, taken)
	if err != nil {
		t.Fatal(err)
	}
	claimed[first] = true
	second, err := FreeName(cand, taken)
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(dir, "a_1.jpg") || second != filepath.Join(dir, "a_2.jpg") {
		t.Fatalf("plans = %s, %s; want a_1, a_2", first, second)
	}

	src := filepath.Join(t.TempDir(), "a.jpg")
	writeFile(t, src, "NEW")
	real, err := MoveUnique(context.Background(), src, dir, "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if real != first {
		t.Fatalf("planned %s but the move produced %s", first, real)
	}
}

// TestTaken documents the non-hang guarantee: a missing path is free, an
// existing one is taken.
func TestTaken(t *testing.T) {
	dir := t.TempDir()
	if Taken(filepath.Join(dir, "nope.jpg")) {
		t.Error("missing path reported as taken")
	}
	present := filepath.Join(dir, "yes.jpg")
	writeFile(t, present, "x")
	if !Taken(present) {
		t.Error("existing path reported as free")
	}
}

// TestSyncDir: a real directory syncs; a missing one errors, so a move never
// unlinks its source after a silently failed sync.
func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir on a real dir: %v", err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("SyncDir on a missing dir: want error, got nil")
	}
}

// TestSameContent locks the U-12 byte-compare contract: identical files are
// equal, differing content of the same size is not, differing sizes are not,
// and empty files are equal — the same true/false verdicts the old double-hash
// produced, now via an early-exiting block compare.
func TestSameContent(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	// bytesRepeating builds an n-byte slice filled with b — handy for content
	// spanning several 64 KiB blocks so the block-boundary logic is exercised.
	bytesRepeating := func(b byte, n int) []byte {
		s := make([]byte, n)
		for i := range s {
			s[i] = b
		}
		return s
	}

	t.Run("identical small files", func(t *testing.T) {
		a := write(t, "a1.bin", []byte("the exact same media bytes"))
		b := write(t, "b1.bin", []byte("the exact same media bytes"))
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if !same {
			t.Error("identical files reported as different")
		}
	})

	t.Run("identical multi-block files", func(t *testing.T) {
		// ~2.5 blocks so a short final block is compared too.
		payload := bytesRepeating('Z', 64*1024*2+123)
		a := write(t, "a2.bin", payload)
		b := write(t, "b2.bin", payload)
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if !same {
			t.Error("identical multi-block files reported as different")
		}
	})

	t.Run("same size, differ at byte 0", func(t *testing.T) {
		// A large file differing in the very first byte must be reported
		// different without depending on reading either file to EOF — the
		// early-exit path the double-hash lacked.
		n := 64 * 1024 * 4
		pa := bytesRepeating('A', n)
		pb := bytesRepeating('A', n)
		pb[0] = 'B'
		a := write(t, "a3.bin", pa)
		b := write(t, "b3.bin", pb)
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if same {
			t.Error("files differing at byte 0 reported as identical")
		}
	})

	t.Run("same size, differ in a later block", func(t *testing.T) {
		n := 64*1024*3 + 7
		pa := bytesRepeating('C', n)
		pb := bytesRepeating('C', n)
		pb[n-1] = 'D' // last byte, in the short final block
		a := write(t, "a4.bin", pa)
		b := write(t, "b4.bin", pb)
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if same {
			t.Error("files differing in the final block reported as identical")
		}
	})

	t.Run("different sizes", func(t *testing.T) {
		a := write(t, "a5.bin", []byte("short"))
		b := write(t, "b5.bin", []byte("a longer set of bytes"))
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if same {
			t.Error("different-size files reported as identical")
		}
	})

	t.Run("both empty", func(t *testing.T) {
		a := write(t, "a6.bin", nil)
		b := write(t, "b6.bin", nil)
		same, err := SameContent(a, b)
		if err != nil {
			t.Fatalf("SameContent: %v", err)
		}
		if !same {
			t.Error("two empty files reported as different")
		}
	})

	t.Run("missing operand surfaces error", func(t *testing.T) {
		a := write(t, "a7.bin", []byte("present"))
		if _, err := SameContent(a, filepath.Join(dir, "nope.bin")); err == nil {
			t.Error("SameContent with a missing file: want error, got nil")
		}
	})
}
