package ui

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubImportDates swaps processBatch's date probe and metadata-rewrite hooks for
// the duration of the test, so the batch can be exercised without exiftool. A
// nil argument leaves that hook unchanged.
func stubImportDates(t *testing.T, prober func(string) (time.Time, time.Time, bool), setter func(string, time.Time) error) {
	t.Helper()
	prober0, setter0 := importDateProber, importSetMediaDate
	if prober != nil {
		importDateProber = prober
	}
	if setter != nil {
		importSetMediaDate = setter
	}
	t.Cleanup(func() {
		importDateProber = prober0
		importSetMediaDate = setter0
	})
}

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
	v := &ImportView{}
	v.invalidate = func() { invalidations++ }

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

	t.Run("junk only", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "._a.jpg"))
		write(t, filepath.Join(dir, ".Trashes", "x.jpg"))
		write(t, filepath.Join(dir, ".Spotlight-V100", "y.mp4"))
		if inboxHasFiles(dir) {
			t.Error("inboxHasFiles with only hidden junk = true, want false")
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
		if s, m := v.statSkipped.Load(), v.statMoved.Load(); s != 1 || m != 0 {
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

	if m := v.statMoved.Load(); m != 1 {
		t.Fatalf("statMoved = %d, want 1 (errors=%d)", m, v.statErrors.Load())
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

// TestProcessBatchProgressReachesMaxOnMkdirError is the U-19 regression:
// every file bumps the bar twice (date pass + move pass). The mkdir-failure
// branch skipped its bump, so a batch with such failures stalled below 100%.
func TestProcessBatchProgressReachesMaxOnMkdirError(t *testing.T) {
	dir := t.TempDir()
	// A regular file as the Outbox makes every date-folder MkdirAll fail.
	outbox := filepath.Join(dir, "outbox")
	if err := os.WriteFile(outbox, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, p)
	}

	v := &ImportView{}
	v.processBatch(context.Background(), outbox, entries)

	if e := v.statErrors.Load(); e != int64(len(entries)) {
		t.Fatalf("statErrors = %d, want %d", e, len(entries))
	}
	if done, max := v.progress.load(); done != max || max != int64(2*len(entries)) {
		t.Errorf("progress = %d/%d, want %d/%d", done, max, 2*len(entries), 2*len(entries))
	}
}

// TestZipJunkName pins the archive filter: entries whose path contains a
// hidden component (._a.jpg, dir/._a.jpg, .Trashes/x.jpg) or __MACOSX must be
// rejected, while ordinary relative paths — including a leading "./" — are
// kept.
func TestZipJunkName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"__MACOSX/._a.jpg", true},
		{"._a.jpg", true},
		{"dir/._a.jpg", true},
		{".Trashes/x.jpg", true},
		{"a.jpg", false},
		{"dir/a.jpg", false},
		{"./a.jpg", false},
	}
	for _, tc := range cases {
		if got := zipJunkName(tc.name); got != tc.want {
			t.Errorf("zipJunkName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRunImportSkipsHiddenCardFiles is the U4-04 regression: a card holding a
// real photo next to macOS litter (._a.jpg AppleDouble fork, .Trashes/) must
// file only the real photo — the junk used to be copied into the Outbox as if
// it were media.
func TestRunImportSkipsHiddenCardFiles(t *testing.T) {
	when := time.Date(2024, 3, 5, 12, 0, 0, 0, time.Local)
	card := t.TempDir()
	for name, content := range map[string]string{
		"a.jpg":                            "real",
		"._a.jpg":                          "fork",
		filepath.Join(".Trashes", "x.jpg"): "trash",
	} {
		p := filepath.Join(card, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	inbox := t.TempDir()
	outbox := t.TempDir()
	v := &ImportView{}
	v.runImport(context.Background(), Config{InboxDir: inbox, OutboxDir: outbox}, []string{card}, nil, false)

	var filed []string
	if err := filepath.WalkDir(outbox, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outbox, p)
		if err != nil {
			return err
		}
		filed = append(filed, rel)
		return nil
	}); err != nil {
		t.Fatalf("walk outbox: %v", err)
	}
	// The Inbox copy keeps the source mtime (fc6715a), so with no EXIF date
	// the file is filed under the card file's own date.
	want := filepath.Join(when.Format("2006-01-02"), "a.jpg")
	if len(filed) != 1 || filed[0] != want {
		t.Errorf("outbox files = %v, want exactly [%s]", filed, want)
	}
	if m := v.statMoved.Load(); m != 1 {
		t.Errorf("statMoved = %d, want 1", m)
	}
	for _, name := range []string{"a.jpg", "._a.jpg", filepath.Join(".Trashes", "x.jpg")} {
		if _, err := os.Stat(filepath.Join(card, name)); err != nil {
			t.Errorf("card file %s should still exist: %v", name, err)
		}
	}
}

// TestProcessBatchCancelledReturnsPromptly is the U3-04 regression: an
// already-cancelled context must stop processBatch at once — the old producer
// filled a len(entries)-sized channel up front, so a 10000-file batch did all
// 10000 sends (and could reach the prober) before any worker noticed the
// cancel.
func TestProcessBatchCancelledReturnsPromptly(t *testing.T) {
	var calls atomic.Int64
	stubImportDates(t, func(string) (time.Time, time.Time, bool) {
		calls.Add(1)
		return time.Time{}, time.Time{}, false
	}, nil)

	dir := t.TempDir()
	outbox := t.TempDir()
	entries := make([]string, 0, 10000)
	for i := range 10000 {
		// Nonexistent paths are fine: nothing may be probed at all.
		entries = append(entries, filepath.Join(dir, "missing"+strconv.Itoa(i)+".jpg"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := &ImportView{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.processBatch(ctx, outbox, entries)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processBatch did not return within 1s of an already-cancelled context")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("date prober called %d times, want 0", n)
	}
}

// TestProcessBatchWorkerPoolCapped pins the bounded date-probe pool: probes run
// concurrently (so a big card isn't serialized on exiftool latency) but the
// exiftool forks stay capped at the pool size (≤4), and every entry still gets
// its two progress ticks.
func TestProcessBatchWorkerPoolCapped(t *testing.T) {
	dir := t.TempDir()
	outbox := t.TempDir()
	const nFiles = 12
	entries := make([]string, 0, nFiles)
	for i := range nFiles {
		p := filepath.Join(dir, "a"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		entries = append(entries, p)
	}

	when := time.Date(2024, 3, 5, 12, 0, 0, 0, time.Local)
	var inFlight, maxInFlight atomic.Int64
	stubImportDates(t, func(string) (time.Time, time.Time, bool) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		return when, time.Time{}, false
	}, nil)

	v := &ImportView{}
	v.processBatch(context.Background(), outbox, entries)

	if got := maxInFlight.Load(); got < 2 || got > 4 {
		t.Errorf("max concurrent date probes = %d, want 2..4", got)
	}
	if m := v.statMoved.Load(); m != nFiles {
		t.Errorf("statMoved = %d, want %d (errors=%d)", m, nFiles, v.statErrors.Load())
	}
	if done, max := v.progress.load(); done != 2*nFiles || max != 2*nFiles {
		t.Errorf("progress = %d/%d, want %d/%d", done, max, 2*nFiles, 2*nFiles)
	}
}

// stubSyncDir swaps the Inbox fsync hook for the duration of the test and
// restores it on cleanup.
func stubSyncDir(t *testing.T, fn func(string) error) {
	t.Helper()
	orig := syncDir
	syncDir = fn
	t.Cleanup(func() { syncDir = orig })
}

// TestCopyToInboxSyncsOnceThenRemovesSources is the U4-05 regression: copying a
// batch with deleteSrc used to fsync the Inbox directory once per file. It must
// fsync exactly once, after every copy is in place and before any source is
// removed.
func TestCopyToInboxSyncsOnceThenRemovesSources(t *testing.T) {
	const nFiles = 5
	srcDir := t.TempDir()
	inbox := t.TempDir()
	var files []string
	for i := range nFiles {
		p := filepath.Join(srcDir, "IMG_000"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte("bytes-"+strconv.Itoa(i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		files = append(files, p)
	}

	var syncCalls atomic.Int64
	sourcesExistedAtSync := true
	stubSyncDir(t, func(dir string) error {
		syncCalls.Add(1)
		for _, p := range files {
			if _, err := os.Stat(p); err != nil {
				sourcesExistedAtSync = false
			}
		}
		return nil
	})

	v := &ImportView{}
	batch := v.copyToInbox(context.Background(), files, inbox, true)

	if len(batch) != nFiles {
		t.Fatalf("len(batch) = %d, want %d", len(batch), nFiles)
	}
	if got := syncCalls.Load(); got != 1 {
		t.Errorf("syncDir calls = %d, want 1", got)
	}
	if !sourcesExistedAtSync {
		t.Error("a source was removed before the Inbox fsync")
	}
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("source %s should be gone, stat err = %v", p, err)
		}
	}
	for _, p := range batch {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("batch copy %s should exist: %v", p, err)
		}
	}
}

// TestCopyToInboxKeepsSourcesWhenSyncFails pins the safety rule: if the Inbox
// fsync fails, no source is deleted — the copies are still returned in the
// batch, so nothing is lost either way.
func TestCopyToInboxKeepsSourcesWhenSyncFails(t *testing.T) {
	srcDir := t.TempDir()
	inbox := t.TempDir()
	var files []string
	for i := range 3 {
		p := filepath.Join(srcDir, "IMG_000"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		files = append(files, p)
	}

	stubSyncDir(t, func(string) error { return errors.New("eio") })

	v := &ImportView{}
	batch := v.copyToInbox(context.Background(), files, inbox, true)

	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("source %s should be kept after a failed fsync: %v", p, err)
		}
	}
	if e := v.statErrors.Load(); e != 1 {
		t.Errorf("statErrors = %d, want 1", e)
	}
	if len(batch) != 3 {
		t.Errorf("len(batch) = %d, want 3", len(batch))
	}
}

// TestCopyToInboxNoDeleteNoSync pins that a non-deleting copy never fsyncs the
// Inbox directory and always leaves the sources alone.
func TestCopyToInboxNoDeleteNoSync(t *testing.T) {
	srcDir := t.TempDir()
	inbox := t.TempDir()
	var files []string
	for i := range 3 {
		p := filepath.Join(srcDir, "IMG_000"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		files = append(files, p)
	}

	var syncCalls atomic.Int64
	stubSyncDir(t, func(string) error {
		syncCalls.Add(1)
		return nil
	})

	v := &ImportView{}
	batch := v.copyToInbox(context.Background(), files, inbox, false)

	if got := syncCalls.Load(); got != 0 {
		t.Errorf("syncDir calls = %d, want 0", got)
	}
	if len(batch) != len(files) {
		t.Errorf("len(batch) = %d, want %d", len(batch), len(files))
	}
	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("source %s should still exist: %v", p, err)
		}
	}
}

// TestStartClickDoesNotBlockOnSlowInboxProbe is the U3-06 regression: the Start
// click used to run the inbox walk on the frame goroutine, so a media-free tree
// or a slow mount froze the UI. The click must return at once, show
// "Checking…" (starting == true), ignore further clicks while the probe is in
// flight, and then settle on the "nothing to import" explanation.
func TestStartClickDoesNotBlockOnSlowInboxProbe(t *testing.T) {
	resetConfigForTest(t)
	inbox := t.TempDir()
	outbox := t.TempDir()
	UpdateConfig(func(c *Config) {
		c.InboxDir = inbox
		c.OutboxDir = outbox
	})

	release := make(chan struct{})
	origProbe := inboxProbe
	inboxProbe = func(string) bool {
		<-release
		return false
	}
	t.Cleanup(func() { inboxProbe = origProbe })

	v := NewImportView(func() {})

	start := time.Now()
	v.onStartClicked()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("onStartClicked blocked for %v, want < 100ms", elapsed)
	}
	v.mu.Lock()
	starting := v.starting
	v.mu.Unlock()
	if !starting {
		t.Fatal("starting = false right after a click, want true while the probe runs")
	}

	start = time.Now()
	v.onStartClicked()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("second onStartClicked blocked for %v, want it ignored", elapsed)
	}

	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		starting = v.starting
		msg := v.statusMsg
		v.mu.Unlock()
		if !starting {
			if !strings.Contains(msg, "Nothing to import") {
				t.Fatalf("statusMsg = %q, want it to mention \"Nothing to import\"", msg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("starting stayed true for 5s after the probe was released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProcessBatchMtimeFixAdvancesProgress is the U3-07 regression: the
// mtime-rewrite pass used to run with no progress movement, so a slow batch of
// fixes looked frozen. Each fix must tick the bar, and the batch must still end
// at done == max.
func TestProcessBatchMtimeFixAdvancesProgress(t *testing.T) {
	dir := t.TempDir()
	outbox := t.TempDir()
	when := time.Date(2024, 3, 5, 12, 0, 0, 0, time.Local)
	var entries []string
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		entries = append(entries, p)
	}

	v := &ImportView{}
	var mu sync.Mutex
	var seen []int64
	stubImportDates(t, func(string) (time.Time, time.Time, bool) {
		return when, when, true // every file's mtime predates its metadata date
	}, func(string, time.Time) error {
		done, _ := v.progress.load()
		mu.Lock()
		seen = append(seen, done)
		mu.Unlock()
		return nil
	})

	v.processBatch(context.Background(), outbox, entries)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(entries) {
		t.Fatalf("mtime setter called %d times, want %d", len(seen), len(entries))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("mtime-fix progress %v not strictly increasing at index %d", seen, i)
		}
	}
	if done, max := v.progress.load(); done != 9 || max != 9 {
		t.Errorf("progress = %d/%d, want 9/9", done, max)
	}
}
