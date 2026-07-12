package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	if exifFailed.Load() {
		t.Error("a timeout should be recoverable, but exifFailed was set")
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
	exifFailed.Store(false)
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
