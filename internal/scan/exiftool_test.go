package scan

import (
	"os"
	"path/filepath"
	"strings"
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

	// Reset package daemon state so we start clean and tear down after.
	resetExifDaemon := func() {
		exifMu.Lock()
		if exifD != nil {
			_ = exifD.close()
			exifD = nil
		}
		exifFailed = false
		exifMu.Unlock()
	}
	resetExifDaemon()
	t.Cleanup(resetExifDaemon)

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
	// The wedged daemon must have been retired, not left in place.
	exifMu.Lock()
	retired := exifD == nil
	failed := exifFailed
	exifMu.Unlock()
	if !retired {
		t.Error("daemon was not retired after a timed-out request")
	}
	if failed {
		t.Error("a timeout should be recoverable, but exifFailed was set")
	}
}
