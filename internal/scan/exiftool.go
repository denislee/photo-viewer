package scan

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
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

// runExiftool sends an exiftool request to the package daemon and returns
// its stdout. The first call spawns the daemon; subsequent calls reuse it.
// On any I/O error the daemon is torn down and exiftool falls back to a
// per-call exec.Command. The callers can be either; both work.
func runExiftool(args ...string) ([]byte, error) {
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
	var out []byte
	for {
		line, err := d.stdout.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		// exiftool prints "{ready}\n" between requests in -stay_open mode.
		if strings.HasPrefix(string(line), "{ready}") {
			return out, nil
		}
		out = append(out, line...)
	}
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
