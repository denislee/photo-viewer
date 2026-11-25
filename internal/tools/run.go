package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Error is a failed external-tool run: the tool's name, its trimmed stderr, and
// the exec error.
type Error struct {
	Name   string
	Stderr string
	Err    error
}

// Error reports "<name>: <first stderr line>: <err>", or "<name>: <err>" when
// the tool wrote nothing to stderr. Only the first stderr line is kept: a
// failing exiftool or ffmpeg often adds warning lines that would spam logs. The
// full text stays in Stderr.
func (e *Error) Error() string {
	if e.Stderr != "" {
		first, _, _ := strings.Cut(e.Stderr, "\n")
		return fmt.Sprintf("%s: %s: %v", e.Name, first, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Name, e.Err)
}

// Unwrap returns the underlying exec error so errors.Is/As can reach it.
func (e *Error) Unwrap() error { return e.Err }

// Run runs name with args under ctx (stdin fed from stdin when non-nil) and
// returns its stdout. stderr is captured; a failed run returns the stdout so
// far and a *Error.
func Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &Error{Name: name, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return out.Bytes(), nil
}
