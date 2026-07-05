package scan

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
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

var (
	exifMu     sync.Mutex
	exifD      *exiftoolDaemon
	exifFailed bool
)

// exiftoolReqTimeout bounds a single -stay_open round-trip. The daemon is a
// shared, long-lived process guarded by exifMu; before this bound existed a
// request that never produced "{ready}" (a hung child, a pathological file the
// tool stalls on) would hold exifMu forever and freeze *every* metadata
// feature until the app was restarted. It is a var, not a const, only so tests
// can shrink it — production code never reassigns it.
var exiftoolReqTimeout = 15 * time.Second

// errExiftoolTimeout marks a request that blew exiftoolReqTimeout. It flows
// through runExiftool's existing "any error retires the daemon" path, so a
// timed-out request tears the daemon down (killing the wedged child) and falls
// back to the one-shot exec, and the next caller transparently respawns it.
var errExiftoolTimeout = errors.New("exiftool: request timed out")

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

	exifMu.Lock()
	if exifFailed {
		exifMu.Unlock()
		return runExiftoolOneShot(args...)
	}
	if exifD == nil {
		d, err := spawnExiftoolDaemon()
		if err != nil {
			exifFailed = true
			exifMu.Unlock()
			return runExiftoolOneShot(args...)
		}
		exifD = d
	}
	d := exifD
	out, err := d.run(args)
	if err != nil {
		// The protocol is line-oriented; one bad request can desync the
		// stream, so on any error retire the daemon and let the next call
		// either restart it or fall back to per-call exec.
		_ = d.close()
		exifD = nil
	}
	exifMu.Unlock()
	if err != nil {
		return runExiftoolOneShot(args...)
	}
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
	// The blocking ReadBytes is what used to pin exifMu indefinitely; running it
	// off-thread lets us give up on a wedged request. On timeout we kill the
	// child, which unblocks the in-flight ReadBytes (its pipe closes) so the
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
	return cmd.Output()
}
