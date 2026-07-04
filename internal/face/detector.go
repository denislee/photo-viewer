// Package face wraps face detection and incremental clustering. The actual
// detection is delegated to an external Python helper (`pv-face-detect`)
// following the project's "missing tool degrades gracefully" pattern shared by
// internal/thumb/{raw,heic,video}.go.
package face

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BinaryName is the helper executable expected on PATH.
const BinaryName = "pv-face-detect"

// resolveBinary finds the helper. PATH first, then alongside the running
// executable (Makefile installs `./pv-face-detect` next to `./photo-viewer`),
// then the current working directory as a last resort.
func resolveBinary() (string, error) {
	if p, err := exec.LookPath(BinaryName); err == nil {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err := filepath.EvalSymlinks(exe); err == nil {
			cand := filepath.Join(filepath.Dir(exe), BinaryName)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return cand, nil
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		cand := filepath.Join(cwd, BinaryName)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New(BinaryName + " not found on PATH or next to the photo-viewer binary")
}

// Detection is one face returned by the helper. BBox is x, y, w, h in the
// detected image's pixel space (the thumbnail, not the original).
type Detection struct {
	BBox      [4]int    `json:"bbox"`
	Embedding []float32 `json:"embedding"`
}

// Status reports the helper's reachability for the UI. Three states:
//   - BinaryPath == ""              → missing (not on PATH)
//   - BinaryPath != "" && !Working  → broken (PATH hit, --check failed)
//   - Working                       → usable
type Status struct {
	BinaryPath string
	Working    bool
	Error      string // populated when not working; safe to display verbatim
}

// Probe inspects PATH and runs the helper's --check mode. The timeout is
// generous because importing dlib + face_recognition the first time can
// easily take several seconds on slow disks.
func Probe() Status {
	bin, err := resolveBinary()
	if err != nil {
		return Status{Error: err.Error()}
	}
	s := Status{BinaryPath: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--check")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		s.Error = "helper timed out during --check (>15s); first-run dlib import may be slow"
		return s
	}
	// face_recognition exits 0 even when broken (its __init__ calls
	// sys.exit on missing models), so trust the explicit "ok " token on
	// stdout rather than the exit code alone.
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "ok") {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "helper exited without 'ok' marker — likely a broken install"
		}
		s.Error = msg
		return s
	}
	s.Working = true
	return s
}

// probe caching. Probe() forks a 1–15s Python --check every call (dlib import),
// so hot-path callers (Available, NewPipeline) must not re-run it. cachedProbe
// memoises the first result; refreshProbe re-runs it and updates the cache
// (used by Pipeline.Recheck when the helper may have been installed/removed).
var (
	probeMu     sync.Mutex
	probeCached *Status
)

// cachedProbe returns the memoised helper status, running the (potentially
// multi-second) probe only on the first call.
func cachedProbe() Status {
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeCached == nil {
		s := Probe()
		probeCached = &s
	}
	return *probeCached
}

// refreshProbe re-runs the probe unconditionally and stores the fresh result,
// so a helper installed or removed after startup is picked up.
func refreshProbe() Status {
	s := Probe()
	probeMu.Lock()
	probeCached = &s
	probeMu.Unlock()
	return s
}

// Available is a thin wrapper for hot paths that just need a yes/no. It reads
// the cached probe result, so only the first call pays the Python fork cost.
func Available() bool { return cachedProbe().Working }

// perImageError is returned by Detect when the daemon replied with an explicit
// {"error": ...} for one image but is otherwise healthy — the caller skips the
// image and keeps reusing the daemon. Every other error from Detect (pipe
// write/read, unmarshal, context timeout) means the child is dead or the stream
// is out of sync and the daemon must be respawned. The pipeline distinguishes
// the two with errors.As.
type perImageError struct{ msg string }

func (e *perImageError) Error() string { return e.msg }

// daemon is a long-running pv-face-detect --server child. Each pipeline
// worker owns one daemon for its lifetime so the dlib import (~1–3s per
// process startup) is amortised across every detection that worker handles.
//
// Daemons are not goroutine-safe: a worker uses its own daemon serially.
type daemon struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// spawnDaemon starts a new helper process in --server mode and waits for it
// to be ready to read on stdin (which is implicit — the child blocks on the
// first stdin read until we send a request).
func spawnDaemon() (*daemon, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, errors.New(BinaryName + " not installed")
	}
	cmd := exec.Command(bin, "--server")
	cmd.Stderr = os.Stderr
	// Pin the child to a single OpenMP thread. dlib/numpy otherwise spin up one
	// OMP thread per core in *every* daemon, so N daemons × NumCPU threads
	// thrash the scheduler for no throughput gain (each detection is already a
	// separate process). One thread per daemon is both cheaper and faster here.
	cmd.Env = append(os.Environ(), "OMP_NUM_THREADS=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	return &daemon{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}, nil
}

// Detect sends one request to the daemon and returns the parsed detections.
// On context cancellation the daemon is killed (the in-flight request can't
// be safely aborted otherwise); callers should treat the daemon as dead and
// not reuse it.
func (d *daemon) Detect(ctx context.Context, imagePath string) ([]Detection, error) {
	req, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: imagePath})
	if err != nil {
		return nil, err
	}
	req = append(req, '\n')
	if _, err := d.stdin.Write(req); err != nil {
		return nil, err
	}

	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := d.stdout.ReadBytes('\n')
		ch <- readResult{line, err}
	}()

	select {
	case <-ctx.Done():
		_ = d.cmd.Process.Kill()
		<-ch
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		var resp struct {
			Detections []Detection `json:"detections"`
			Error      string      `json:"error"`
		}
		if err := json.Unmarshal(r.line, &resp); err != nil {
			// A malformed line corrupts stream framing — treat as transport.
			return nil, err
		}
		if resp.Error != "" {
			// Daemon is healthy; it just couldn't handle this one image.
			return nil, &perImageError{msg: resp.Error}
		}
		return resp.Detections, nil
	}
}

// Close signals EOF to the daemon and reaps the child. Safe to call once.
func (d *daemon) Close() {
	_ = d.stdin.Close()
	_ = d.cmd.Wait()
}
