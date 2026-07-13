package scan

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// exiftoolDaemon is a long-running exiftool process used by metadata readers
// instead of fork+exec-per-file. exiftool's `-stay_open True -@ -` protocol
// keeps the process alive between calls, which makes bulk metadata reads
// roughly two orders of magnitude faster than the alternative.
//
// The Python daemon pattern in internal/face/detector.go is the model.
type exiftoolDaemon struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// exiftool daemons are pooled rather than kept as a single shared instance so
// that concurrent metadata readers (the import date pass runs up to
// min(NumCPU,4) workers; pv-organize's video-date loop; an interactive viewer
// info read) don't serialize behind one daemon's request round-trip. Each pool
// slot holds either a live daemon or nil (an empty slot spawned on demand): a
// request checks a slot out, runs on it, and checks it back in, retiring just
// that daemon (returning nil) on any error so only the failing instance is torn
// down.
//
// exifFailedAt records the unix-nano time of the most recent daemon spawn
// failure, or 0 when the last spawn succeeded (or none has been attempted). A
// recent failure routes callers to the one-shot exec, but only for
// exifSpawnBackoff; once that window elapses a fresh spawn is retried. This is a
// timed backoff rather than a permanent latch (S-05) so a transient fork
// failure (memory pressure, FD exhaustion) doesn't degrade the whole process to
// fork-per-file for its lifetime — mirroring face.Recheck's re-probe-after-
// failure spirit.
var (
	exifPoolMu   sync.Mutex
	exifPool     chan *exiftoolDaemon
	exifFailedAt atomic.Int64
)

// exifSpawnBackoff is how long a failed daemon spawn suppresses further spawn
// attempts, routing callers to the one-shot exec instead. After the window a
// fresh spawn is retried. It is a var, not a const, only so tests can shrink or
// age past it — production code never reassigns it.
var exifSpawnBackoff = time.Minute

// exifSpawnBackedOff reports whether a daemon spawn failed recently enough that
// callers should skip the pool and go straight to the one-shot exec. Once the
// backoff window has elapsed the pool path re-opens and the spawn is retried.
func exifSpawnBackedOff() bool {
	at := exifFailedAt.Load()
	if at == 0 {
		return false
	}
	return time.Since(time.Unix(0, at)) < exifSpawnBackoff
}

// spawnExiftool builds a daemon. It is a var only so tests can fault-inject a
// transient spawn failure; production always uses spawnExiftoolDaemon.
var spawnExiftool = spawnExiftoolDaemon

// exiftoolPoolSize mirrors the face pipeline's worker sizing: at least 2 (so an
// interactive read isn't stuck behind a single in-flight bulk request) and at
// most 4, which is where the import date pass caps its own worker count.
func exiftoolPoolSize() int {
	return min(max(runtime.NumCPU(), 2), 4)
}

// exifPoolChan lazily builds the daemon pool as a buffered channel pre-filled
// with empty (nil) slots, each spawned into a live daemon on first use.
func exifPoolChan() chan *exiftoolDaemon {
	exifPoolMu.Lock()
	defer exifPoolMu.Unlock()
	if exifPool == nil {
		n := exiftoolPoolSize()
		exifPool = make(chan *exiftoolDaemon, n)
		for range n {
			exifPool <- nil
		}
	}
	return exifPool
}

// exiftoolReqTimeout bounds a single -stay_open round-trip. Each daemon is a
// pooled, long-lived process; before this bound existed a request that never
// produced "{ready}" (a hung child, a pathological file the tool stalls on)
// would hold its pool slot forever, and a run of them could drain the pool and
// stall every metadata feature until the app was restarted. It is a var, not a
// const, only so tests can shrink it — production code never reassigns it.
var exiftoolReqTimeout = 15 * time.Second

// errExiftoolTimeout marks a request that blew exiftoolReqTimeout. It flows
// through runExiftool's existing "any error retires the daemon" path, so a
// timed-out request tears the daemon down (killing the wedged child) and falls
// back to the one-shot exec, and the next caller transparently respawns it.
var errExiftoolTimeout = errors.New("exiftool: request timed out")

// haveExiftool caches whether exiftool is on PATH. runExiftool gates the daemon
// pool on it so a never-installed tool short-circuits straight to the one-shot
// path instead of retrying a doomed spawn every backoff window (S-05), and
// SetMediaDate — which execs exiftool directly, not through the pooled daemon —
// reuses it rather than re-walking $PATH per file in the "fix dates" loop.
// Presence can't change meaningfully mid-run; a process restart re-probes.
// Mirrors haveFFprobe in duration.go.
var haveExiftool = sync.OnceValue(func() bool {
	_, err := exec.LookPath("exiftool")
	return err == nil
})

// RunExiftool runs an exiftool request through this package's shared
// -stay_open daemon and returns its stdout, so callers outside the package
// (e.g. internal/thumb's RAW preview probe) don't pay a fresh fork per read.
//
// It is for textual metadata reads only. Do NOT use it for "-b" binary
// extraction: the daemon protocol is newline-framed, so binary output can
// desync the stream — extract binary via a one-shot exec.Command instead.
func RunExiftool(args ...string) ([]byte, error) {
	return runExiftool(args...)
}

// runExiftool sends an exiftool request to the package daemon and returns
// its stdout. The first call spawns the daemon; subsequent calls reuse it.
// On any I/O error (or a timeout) the daemon is torn down and exiftool falls
// back to a per-call exec.Command. The callers can be either; both work.
func runExiftool(args ...string) ([]byte, error) {
	// The -stay_open argfile stream is newline-delimited and treats a leading
	// '#' as a comment, so any argument carrying a newline/carriage-return or
	// starting with '#' would corrupt the request framing and desync the daemon
	// for every later caller. Route those (pathological filenames) straight to
	// the one-shot exec, which passes argv directly and has no such hazard.
	if argsUnsafeForDaemon(args) {
		return runExiftoolOneShot(args...)
	}
	// A truly-absent exiftool never reaches the daemon pool: haveExiftool (a
	// cached PATH probe from S-06) short-circuits to the one-shot path, which
	// reports "not installed" cleanly. That keeps us from retrying a doomed
	// spawn every backoff window when the tool was simply never installed; the
	// backoff below is only for a transient failure with the tool present.
	if !haveExiftool() {
		return runExiftoolOneShot(args...)
	}
	// A recent spawn failure sends callers down the one-shot path, but only for
	// the backoff window — once it elapses the daemon spawn is retried, so a
	// transient failure doesn't degrade to fork-per-file for the process life.
	if exifSpawnBackedOff() {
		return runExiftoolOneShot(args...)
	}

	pool := exifPoolChan()
	d := <-pool // check out a slot; blocks only when all daemons are busy
	if d == nil {
		spawned, err := spawnExiftool()
		if err != nil {
			exifFailedAt.Store(time.Now().UnixNano())
			pool <- nil // return the empty slot
			return runExiftoolOneShot(args...)
		}
		// A successful spawn clears any prior backoff.
		exifFailedAt.Store(0)
		d = spawned
	}
	out, err := d.run(args)
	if err != nil {
		// The protocol is line-oriented; one bad request can desync a daemon's
		// stream, so retire just this instance and return an empty slot for the
		// next caller to respawn. Other pool daemons are unaffected.
		_ = d.close()
		pool <- nil
		return runExiftoolOneShot(args...)
	}
	pool <- d // check the live daemon back in
	return out, nil
}

func spawnExiftoolDaemon() (*exiftoolDaemon, error) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		return nil, err
	}
	cmd := exec.Command("exiftool", "-stay_open", "True", "-@", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	// stderr is intentionally discarded — exiftool emits ExifTool warnings
	// (missing tags, unknown makernotes) for nearly every photo, which
	// would otherwise spam the viewer's logs.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	return &exiftoolDaemon{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64*1024),
	}, nil
}

func (d *exiftoolDaemon) run(args []string) ([]byte, error) {
	var req strings.Builder
	for _, a := range args {
		req.WriteString(a)
		req.WriteByte('\n')
	}
	req.WriteString("-execute\n")
	if _, err := d.stdin.Write([]byte(req.String())); err != nil {
		return nil, err
	}

	// Read the response off a goroutine and race it against exiftoolReqTimeout.
	// The blocking ReadBytes is what used to pin a pool slot indefinitely;
	// running it off-thread lets us give up on a wedged request. On timeout we
	// kill the child, which unblocks the in-flight ReadBytes (its pipe closes) so the
	// goroutine can't leak, and return errExiftoolTimeout for the caller to
	// retire the daemon on.
	type readResult struct {
		out []byte
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		var out []byte
		for {
			line, err := d.stdout.ReadBytes('\n')
			if err != nil {
				done <- readResult{nil, err}
				return
			}
			// exiftool prints "{ready}\n" between requests in -stay_open mode.
			if strings.HasPrefix(string(line), "{ready}") {
				done <- readResult{out, nil}
				return
			}
			out = append(out, line...)
		}
	}()

	timer := time.NewTimer(exiftoolReqTimeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.out, res.err
	case <-timer.C:
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		return nil, errExiftoolTimeout
	}
}

// argsUnsafeForDaemon reports whether any argument would corrupt the daemon's
// newline-delimited -stay_open argfile stream: an embedded newline/carriage
// return reads as a premature end-of-argument (or a stray -execute), and a
// leading '#' is silently swallowed as an argfile comment. Such arguments —
// in practice only exotic filenames — must take the one-shot exec path, which
// passes them as a real argv vector.
func argsUnsafeForDaemon(args []string) bool {
	for _, a := range args {
		if strings.ContainsAny(a, "\n\r") || strings.HasPrefix(a, "#") {
			return true
		}
	}
	return false
}

func (d *exiftoolDaemon) close() error {
	if d == nil {
		return nil
	}
	_, _ = d.stdin.Write([]byte("-stay_open\nFalse\n"))
	_ = d.stdin.Close()
	return d.cmd.Wait()
}

// runExiftoolOneShot is the per-call fallback used when the daemon either
// failed to start or returned a desync error. Keeps the existing behaviour
// of GetMediaInfo / GetMediaDate intact in environments where the long-
// running exiftool can't run.
func runExiftoolOneShot(args ...string) ([]byte, error) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		return nil, errors.New("exiftool not installed")
	}
	cmd := exec.Command("exiftool", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, exiftoolError(err, &stderr)
	}
	return out.Bytes(), nil
}

// exiftoolError folds the first line of exiftool's stderr into the returned
// error so a one-shot failure reports *why* instead of a bare "exit status 1".
// Only the first line is kept — a failing exiftool often emits several warning
// lines that would otherwise spam logs. Mirrors ffmpegError in internal/thumb.
func exiftoolError(err error, stderr *bytes.Buffer) error {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		first, _, _ := strings.Cut(msg, "\n")
		return fmt.Errorf("exiftool: %s: %w", first, err)
	}
	return err
}
