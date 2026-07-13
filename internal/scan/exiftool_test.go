package scan

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunExiftoolOneShotCapturesStderr checks that the one-shot fallback (S-17)
// folds exiftool's stderr detail into the returned error instead of dropping it
// for a bare "exit status 1".
func TestRunExiftoolOneShotCapturesStderr(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed; skipping stderr-capture test")
	}

	missing := filepath.Join(t.TempDir(), "nope.jpg")
	_, err := runExiftoolOneShot("-b", "-PreviewImage", missing)
	if err == nil {
		t.Fatal("expected an error for a nonexistent file")
	}
	if !strings.Contains(err.Error(), "exiftool:") {
		t.Errorf("error should carry the exiftool prefix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "File not found") {
		t.Errorf("error should carry exiftool's stderr detail, got: %v", err)
	}
}

func TestArgsUnsafeForDaemon(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"plain", []string{"-s", "-S", "-CreateDate", "/photos/a.jpg"}, false},
		{"embedded newline", []string{"-b", "-PreviewImage", "/photos/we\nird.cr2"}, true},
		{"embedded carriage return", []string{"-b", "/photos/we\rird.cr2"}, true},
		{"leading hash path", []string{"-b", "#notacomment.jpg"}, true},
		{"hash mid-string is fine", []string{"-b", "/photos/a#b.jpg"}, false},
		{"empty args", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := argsUnsafeForDaemon(c.args); got != c.want {
				t.Errorf("argsUnsafeForDaemon(%q) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

// fakeExiftoolScript is a stand-in `exiftool` binary. In -stay_open daemon
// mode it wedges — it consumes the request but never emits the "{ready}"
// sentinel, using shell builtins only so killing the process leaves no
// orphans. In one-shot mode it prints a recognisable marker so the test can
// confirm the fallback ran.
const fakeExiftoolScript = `#!/bin/sh
if [ "$1" = "-stay_open" ]; then
	while IFS= read -r _line; do :; done
	exit 0
fi
printf 'ONESHOT-OK\n'
`

// TestRunExiftoolTimeoutRetiresAndFallsBack drives the daemon against a fake
// binary that hangs, and asserts a wedged request (1) times out instead of
// pinning exifMu forever, (2) retires the daemon, and (3) still returns a
// result via the one-shot fallback.
func TestRunExiftoolTimeoutRetiresAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "exiftool")
	if err := os.WriteFile(fake, []byte(fakeExiftoolScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// Put our fake first on PATH; keep the real dirs so the script's `sh` can
	// still resolve whatever it needs.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Shrink the round-trip deadline so the test doesn't wait 15s.
	oldTimeout := exiftoolReqTimeout
	exiftoolReqTimeout = 300 * time.Millisecond
	t.Cleanup(func() { exiftoolReqTimeout = oldTimeout })

	// Reset the package daemon pool so we start clean and tear down after.
	resetExifPool()
	t.Cleanup(resetExifPool)

	start := time.Now()
	out, err := runExiftool("-s", "-S", "-CreateDate", "/some/file.jpg")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runExiftool returned error: %v", err)
	}
	if !strings.Contains(string(out), "ONESHOT-OK") {
		t.Errorf("expected one-shot fallback output, got %q", out)
	}
	// Must have given up near the deadline, not blocked on the wedged child.
	if elapsed > 5*time.Second {
		t.Errorf("runExiftool took %s; expected it to time out near %s", elapsed, exiftoolReqTimeout)
	}
	// The wedged daemon must have been retired, not left in the pool.
	if live := livePoolDaemons(); live != 0 {
		t.Errorf("expected 0 live daemons in the pool after a timeout, got %d", live)
	}
	if exifSpawnBackedOff() {
		t.Error("a timeout should be recoverable, but the spawn backoff was armed")
	}
}

// resetExifPool tears down and forgets the package daemon pool so a test can
// start from a clean slate. Sequential-test only: it assumes no request is
// mid-flight (nothing is checked out) when it runs.
func resetExifPool() {
	exifPoolMu.Lock()
	old := exifPool
	exifPool = nil
	exifPoolMu.Unlock()
	exifFailedAt.Store(0)
	if old != nil {
		for {
			select {
			case d := <-old:
				if d != nil {
					_ = d.close()
				}
			default:
				return
			}
		}
	}
}

// livePoolDaemons drains the pool and reports how many slots held a live
// daemon, restoring every slot afterwards. Sequential-test only.
func livePoolDaemons() int {
	exifPoolMu.Lock()
	pool := exifPool
	exifPoolMu.Unlock()
	if pool == nil {
		return 0
	}
	var drained []*exiftoolDaemon
	for i := 0; i < cap(pool); i++ {
		select {
		case d := <-pool:
			drained = append(drained, d)
		default:
			i = cap(pool) // pool momentarily short a slot; stop scanning
		}
	}
	live := 0
	for _, d := range drained {
		if d != nil {
			live++
		}
		pool <- d
	}
	return live
}

// fakeExiftoolDaemonScript is a working stand-in `exiftool`. In -stay_open mode
// it echoes the request's last non-flag argument (the file path) back as a
// CreateDate line followed by the "{ready}" sentinel, so concurrent callers get
// distinct results; "-stay_open False" makes it exit. In one-shot mode it
// prints ONESHOT-OK. Uses shell builtins only, so a kill leaves no orphans.
const fakeExiftoolDaemonScript = `#!/bin/sh
if [ "$1" = "-stay_open" ]; then
	lastarg=""
	while IFS= read -r line; do
		case "$line" in
			-execute) printf 'CreateDate: %s\n{ready}\n' "$lastarg"; lastarg="" ;;
			False) exit 0 ;;
			-*) : ;;
			*) lastarg="$line" ;;
		esac
	done
	exit 0
fi
printf 'ONESHOT-OK\n'
`

// TestRunExiftoolPoolConcurrent drives many concurrent requests through the
// daemon pool against a working fake and asserts each call gets its own correct
// result — i.e. the pool serves requests in parallel without cross-talk.
func TestRunExiftoolPoolConcurrent(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "exiftool")
	if err := os.WriteFile(fake, []byte(fakeExiftoolDaemonScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resetExifPool()
	t.Cleanup(resetExifPool)

	const n = 32
	type result struct {
		path string
		out  string
		err  error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			path := fmt.Sprintf("/photos/file%02d.jpg", i)
			out, err := runExiftool("-s", "-S", "-CreateDate", path)
			results[i] = result{path: path, out: string(out), err: err}
		})
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("request %d (%s) errored: %v", i, r.path, r.err)
			continue
		}
		// Each daemon echoes back the path it was asked about; a mismatch would
		// mean two concurrent requests shared a daemon and crossed streams.
		if want := "CreateDate: " + r.path; !strings.Contains(r.out, want) {
			t.Errorf("request %d: got %q, want it to contain %q", i, r.out, want)
		}
	}
	// The pool should never grow past its cap regardless of the request burst.
	if live := livePoolDaemons(); live > exiftoolPoolSize() {
		t.Errorf("pool holds %d live daemons, exceeds cap %d", live, exiftoolPoolSize())
	}
}

// TestRunExiftoolSpawnFailureBacksOffThenRetries verifies S-05: a transient
// daemon spawn failure routes callers to the one-shot exec only for the backoff
// window, after which the daemon path is retried — rather than being latched to
// fork-per-file for the process lifetime. It fault-injects a spawn that fails
// once then succeeds (delegating to the working fake daemon on PATH).
func TestRunExiftoolSpawnFailureBacksOffThenRetries(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "exiftool")
	if err := os.WriteFile(fake, []byte(fakeExiftoolDaemonScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resetExifPool()
	t.Cleanup(resetExifPool)

	// The first spawn attempt fails (a transient fork failure); every later one
	// delegates to the real spawner, which starts the working fake daemon.
	var spawns atomic.Int32
	prevSpawn := spawnExiftool
	spawnExiftool = func() (*exiftoolDaemon, error) {
		if spawns.Add(1) == 1 {
			return nil, errors.New("simulated transient spawn failure")
		}
		return prevSpawn()
	}
	t.Cleanup(func() { spawnExiftool = prevSpawn })

	// First request: spawn fails, so it falls back to the one-shot exec and arms
	// the backoff window.
	out, err := runExiftool("-s", "-S", "-CreateDate", "/photos/first.jpg")
	if err != nil {
		t.Fatalf("first runExiftool errored: %v", err)
	}
	if !strings.Contains(string(out), "ONESHOT-OK") {
		t.Errorf("first call: expected one-shot fallback, got %q", out)
	}
	if !exifSpawnBackedOff() {
		t.Fatal("a spawn failure should arm the backoff window")
	}

	// While the window is open, callers stay on the one-shot path and no further
	// spawn is attempted.
	spawnsBefore := spawns.Load()
	out, err = runExiftool("-s", "-S", "-CreateDate", "/photos/during.jpg")
	if err != nil {
		t.Fatalf("during-backoff runExiftool errored: %v", err)
	}
	if !strings.Contains(string(out), "ONESHOT-OK") {
		t.Errorf("during backoff: expected one-shot fallback, got %q", out)
	}
	if got := spawns.Load(); got != spawnsBefore {
		t.Errorf("during backoff: spawn should not be retried, but count went %d -> %d", spawnsBefore, got)
	}

	// Simulate the backoff window elapsing by ageing the failure timestamp past
	// it (deterministic, no sleeping).
	exifFailedAt.Store(time.Now().Add(-2 * exifSpawnBackoff).UnixNano())
	if exifSpawnBackedOff() {
		t.Fatal("backoff window should have re-opened once the timestamp aged out")
	}

	// Next request retries the daemon spawn (now succeeding) and rides the
	// daemon path — the one-shot latch is gone.
	out, err = runExiftool("-s", "-S", "-CreateDate", "/photos/after.jpg")
	if err != nil {
		t.Fatalf("post-backoff runExiftool errored: %v", err)
	}
	if strings.Contains(string(out), "ONESHOT-OK") {
		t.Errorf("post-backoff: should no longer be on the one-shot path, got %q", out)
	}
	if want := "CreateDate: /photos/after.jpg"; !strings.Contains(string(out), want) {
		t.Errorf("post-backoff: expected daemon output %q, got %q", want, out)
	}
	// A successful spawn clears the failure timestamp.
	if got := exifFailedAt.Load(); got != 0 {
		t.Errorf("a successful spawn should clear the failure timestamp, got %d", got)
	}
}
