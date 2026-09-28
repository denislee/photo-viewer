package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestImportCopyProgressInstrumentation locks the U-09 contract for the
// phase-3 Inbox-copy loop: it drives the progress bar with the exact helper
// sequence the loop uses — setProgress(0, len(files)) up front, then one
// bumpProgress per file (whether the copy succeeds or errors) — so the bar
// climbs to len(files) instead of holding the previous batch's stale terminal
// value. The loop itself lives inside runImport (goroutine + exiftool via
// processBatch), so it isn't hermetically unit-testable; this asserts the
// instrumentation invariant it depends on. No process is registered, so the
// helpers fall back to scheduleInvalidate → invalidate.
func TestImportCopyProgressInstrumentation(t *testing.T) {
	const nFiles = 7
	var invalidations int
	v := &ImportView{invalidate: func() { invalidations++ }}

	v.setProgress(0, int64(nFiles))
	if done, max := v.progress.load(); max != nFiles {
		t.Fatalf("progressMax after setProgress = %d, want %d", max, nFiles)
	} else if done != 0 {
		t.Fatalf("progressDone after setProgress = %d, want 0", done)
	}

	// One bump per file, mirroring the loop's success and copy-error paths.
	for range nFiles {
		v.bumpProgress()
	}

	if done, max := v.progress.load(); done != nFiles {
		t.Errorf("progressDone after %d bumps = %d, want %d", nFiles, done, nFiles)
	} else if max != nFiles {
		t.Errorf("progressMax drifted to %d, want %d", max, nFiles)
	}
	// Each helper wakes the frame loop (no registry wired ⇒ direct invalidate);
	// the bar can't visibly advance without at least one redraw per update.
	if invalidations < nFiles {
		t.Errorf("invalidate called %d times, want ≥ %d (bar would look frozen)", invalidations, nFiles)
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

// TestWriteFileDurableSuccess verifies the happy path publishes the full
// content atomically and leaves no .tmp sidecar behind.
func TestWriteFileDurableSuccess(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0001.jpg")
	want := []byte("the full media file contents")

	if err := writeFileDurable(dst, bytes.NewReader(want)); err != nil {
		t.Fatalf("writeFileDurable: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("dst content = %q, want %q", got, want)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp sidecar was left behind: %v", err)
	}
}

// TestWriteFileDurableNoPartialOnError is the crash-safety contract behind U-01:
// a write that fails midway must never leave a truncated file at the final name
// (which the next inbox walk would file into the library as valid media), and
// must clean up its .tmp too.
func TestWriteFileDurableNoPartialOnError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0002.jpg")

	r := &errAfterReader{data: []byte("partial-then-boom-more-bytes"), fail: 8}
	err := writeFileDurable(dst, r)
	if err == nil {
		t.Fatal("expected writeFileDurable to return the injected error, got nil")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("truncated file left at final name %s (stat err: %v)", dst, statErr)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("partial .tmp left behind (stat err: %v)", statErr)
	}
}

// TestInboxHasFiles locks the U-10 cheap existence check: inboxHasFiles reports
// true when the inbox holds at least one importable media file (only DetectType
// != TypeUnknown counts), false for an empty inbox or one holding only
// non-media/hidden leftovers. It replaces the full recursive inboxFileCount at
// the Start-click decision sites, so it must agree with them on the ≥1-file
// question.
func TestInboxHasFiles(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("empty dir", func(t *testing.T) {
		if inboxHasFiles(t.TempDir()) {
			t.Error("inboxHasFiles on an empty dir = true, want false")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if inboxHasFiles(filepath.Join(t.TempDir(), "nope")) {
			t.Error("inboxHasFiles on a missing dir = true, want false")
		}
	})

	t.Run("only non-media and hidden files", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".DS_Store"))
		write(t, filepath.Join(dir, "notes.txt"))
		write(t, filepath.Join(dir, "sub", "readme.md"))
		if inboxHasFiles(dir) {
			t.Error("inboxHasFiles with only non-media files = true, want false")
		}
	})

	t.Run("one media file at root", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "IMG_0001.jpg"))
		if !inboxHasFiles(dir) {
			t.Error("inboxHasFiles with a media file = false, want true")
		}
	})

	t.Run("media file nested among many non-media files", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".DS_Store"))
		for i := range 50 {
			write(t, filepath.Join(dir, "junk", "f"+strconv.Itoa(i)+".txt"))
		}
		write(t, filepath.Join(dir, "deep", "sub", "clip.mp4"))
		if !inboxHasFiles(dir) {
			t.Error("inboxHasFiles with a nested media file = false, want true")
		}
	})
}

// TestSyncDir confirms the U-13 helper opens and fsyncs a real directory and
// surfaces an error for a missing one (so a move never unlinks its source on a
// silently-failed dir sync). The durability itself is a crash-consistency
// property, verified by review against internal/export's moveFile.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("syncDir on a missing dir: want error, got nil")
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
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
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
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
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
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
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
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
		}
		if same {
			t.Error("files differing in the final block reported as identical")
		}
	})

	t.Run("different sizes", func(t *testing.T) {
		a := write(t, "a5.bin", []byte("short"))
		b := write(t, "b5.bin", []byte("a longer set of bytes"))
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
		}
		if same {
			t.Error("different-size files reported as identical")
		}
	})

	t.Run("both empty", func(t *testing.T) {
		a := write(t, "a6.bin", nil)
		b := write(t, "b6.bin", nil)
		same, err := sameContent(a, b)
		if err != nil {
			t.Fatalf("sameContent: %v", err)
		}
		if !same {
			t.Error("two empty files reported as different")
		}
	})

	t.Run("missing operand surfaces error", func(t *testing.T) {
		a := write(t, "a7.bin", []byte("present"))
		if _, err := sameContent(a, filepath.Join(dir, "nope.bin")); err == nil {
			t.Error("sameContent with a missing file: want error, got nil")
		}
	})
}

// TestImportDirsProblem pins the U-17 config guard: an Inbox that is, or
// contains, the Outbox is rejected (import would re-file the library into
// itself); an Inbox inside the Outbox, siblings, and name-prefix siblings are
// all fine.
func TestImportDirsProblem(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "photos")
	if err := os.MkdirAll(filepath.Join(lib, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "photos-link")
	if err := os.Symlink(lib, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		inbox, outbox string
		wantProblem   bool
	}{
		{"unset", "", lib, false},
		{"same dir", lib, lib, true},
		{"same dir, trailing slash", lib + "/", lib, true},
		{"same dir via symlink", link, lib, true},
		{"outbox inside inbox", root, lib, true},
		{"inbox inside outbox", filepath.Join(lib, "inbox"), lib, false},
		{"siblings", filepath.Join(root, "in"), filepath.Join(root, "out"), false},
		{"name-prefix sibling", filepath.Join(root, "photos-in"), lib, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := importDirsProblem(tc.inbox, tc.outbox)
			if (got != "") != tc.wantProblem {
				t.Errorf("importDirsProblem(%q, %q) = %q, wantProblem %v", tc.inbox, tc.outbox, got, tc.wantProblem)
			}
		})
	}
}

// TestProcessBatchNeverDeletesAlreadyFiled is the U-17 regression: a file that
// already sits at its Outbox/YYYY-MM-DD destination (inbox == outbox, or
// reached through a symlinked inbox) used to be compared with itself, judged a
// duplicate, and removed — deleting the only copy. It must be skipped instead,
// while a genuinely separate identical copy is still de-duplicated.
func TestProcessBatchNeverDeletesAlreadyFiled(t *testing.T) {
	when := time.Date(2024, 3, 5, 12, 0, 0, 0, time.Local)
	content := []byte("already-filed media bytes")

	setup := func(t *testing.T) (outbox, filed string) {
		t.Helper()
		outbox = t.TempDir()
		dateDir := filepath.Join(outbox, when.Format("2006-01-02"))
		if err := os.MkdirAll(dateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		filed = filepath.Join(dateDir, "IMG_0001.jpg")
		if err := os.WriteFile(filed, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filed, when, when); err != nil {
			t.Fatal(err)
		}
		return outbox, filed
	}
	assertIntact := func(t *testing.T, v *ImportView, filed string) {
		t.Helper()
		got, err := os.ReadFile(filed)
		if err != nil {
			t.Fatalf("filed copy is gone: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("filed copy changed: %q", got)
		}
		if s, m := atomic.LoadInt64(&v.statSkipped), atomic.LoadInt64(&v.statMoved); s != 1 || m != 0 {
			t.Errorf("skipped=%d moved=%d, want skipped=1 moved=0", s, m)
		}
	}

	t.Run("inbox is outbox", func(t *testing.T) {
		outbox, filed := setup(t)
		v := &ImportView{}
		v.processBatch(context.Background(), outbox, []string{filed})
		assertIntact(t, v, filed)
	})

	t.Run("inbox symlinked to outbox", func(t *testing.T) {
		outbox, filed := setup(t)
		link := filepath.Join(t.TempDir(), "inbox")
		if err := os.Symlink(outbox, link); err != nil {
			t.Fatal(err)
		}
		viaLink := filepath.Join(link, when.Format("2006-01-02"), "IMG_0001.jpg")
		v := &ImportView{}
		v.processBatch(context.Background(), outbox, []string{viaLink})
		assertIntact(t, v, filed)
	})

	t.Run("separate identical copy is still de-duplicated", func(t *testing.T) {
		outbox, filed := setup(t)
		dup := filepath.Join(t.TempDir(), "IMG_0001.jpg")
		if err := os.WriteFile(dup, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dup, when, when); err != nil {
			t.Fatal(err)
		}
		v := &ImportView{}
		v.processBatch(context.Background(), outbox, []string{dup})
		assertIntact(t, v, filed)
		if _, err := os.Stat(dup); !os.IsNotExist(err) {
			t.Errorf("inbox duplicate should have been removed, stat err = %v", err)
		}
	})
}

// TestProcessBatchCollisionNeverOverwrites is the U-18 regression: when the
// destination name is taken by different content, the file must land on the
// next free _N name — the old fallback renamed onto an unchecked
// UnixNano-suffixed name — and no existing file may be touched.
func TestProcessBatchCollisionNeverOverwrites(t *testing.T) {
	when := time.Date(2024, 3, 5, 12, 0, 0, 0, time.Local)
	outbox := t.TempDir()
	dateDir := filepath.Join(outbox, when.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	occupants := map[string]string{"IMG_0001.jpg": "first", "IMG_0001_1.jpg": "second"}
	for name, content := range occupants {
		if err := os.WriteFile(filepath.Join(dateDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(t.TempDir(), "IMG_0001.jpg")
	if err := os.WriteFile(src, []byte("third, different"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}

	v := &ImportView{}
	v.processBatch(context.Background(), outbox, []string{src})

	if m := atomic.LoadInt64(&v.statMoved); m != 1 {
		t.Fatalf("statMoved = %d, want 1 (errors=%d)", m, atomic.LoadInt64(&v.statErrors))
	}
	for name, want := range occupants {
		if got, _ := os.ReadFile(filepath.Join(dateDir, name)); string(got) != want {
			t.Errorf("%s = %q, want %q (clobbered)", name, got, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dateDir, "IMG_0001_2.jpg")); err != nil || string(got) != "third, different" {
		t.Errorf("IMG_0001_2.jpg = %q, %v; want the imported file", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be moved away, stat err = %v", err)
	}
}

// TestCopyFileNoReplace: the durable copy used for cross-device filing must
// refuse to publish over an existing file and must not leave its temp behind.
func TestCopyFileNoReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "dst.jpg")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("occupant"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFileNoReplace(src, dst); !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy onto existing file: err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "occupant" {
		t.Errorf("occupant clobbered: %q", got)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp left behind: %v", err)
	}
}

// TestFileIntoCrossDevice drives fileInto's EXDEV branch for real by moving
// from /dev/shm to a TempDir on a different filesystem: the file must land on
// the next free name via the durable copy, the occupant must be untouched, and
// the source must be removed only after that.
func TestFileIntoCrossDevice(t *testing.T) {
	destDir := t.TempDir()
	srcDir, err := os.MkdirTemp("/dev/shm", "pv-fileinto-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(srcDir) })
	if sameDevice(srcDir, destDir) {
		t.Skip("/dev/shm and TempDir share a filesystem; EXDEV path not reachable")
	}

	if err := os.WriteFile(filepath.Join(destDir, "clip.mov"), []byte("occupant"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "clip.mov")
	if err := os.WriteFile(src, []byte("from the card"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest, err := fileInto(src, destDir, "clip.mov")
	if err != nil {
		t.Fatalf("fileInto: %v", err)
	}
	if want := filepath.Join(destDir, "clip_1.mov"); dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	if got, _ := os.ReadFile(dest); string(got) != "from the card" {
		t.Errorf("dest content = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(destDir, "clip.mov")); string(got) != "occupant" {
		t.Errorf("occupant clobbered: %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be removed after a durable copy, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(destDir, "*.tmp")); len(matches) > 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}
