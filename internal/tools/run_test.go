package tools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestRunSuccessReturnsStdout checks the happy path: a clean run returns the
// tool's stdout and no error.
func TestRunSuccessReturnsStdout(t *testing.T) {
	t.Parallel()

	out, err := Run(context.Background(), nil, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(out), "out\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestRunFailureKeepsFirstStderrLine pins the error shape: the message carries
// the tool name, only the first stderr line, and the exec error, while the full
// stderr stays on the *Error.
func TestRunFailureKeepsFirstStderrLine(t *testing.T) {
	t.Parallel()

	_, err := Run(context.Background(), nil, "sh", "-c", "echo first >&2; echo second >&2; exit 3")
	if err == nil {
		t.Fatal("Run = nil error, want failure")
	}
	const want = "sh: first: exit status 3"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(*Error) = false for %T", err)
	}
	if got, want := te.Stderr, "first\nsecond"; got != want {
		t.Errorf("Stderr = %q, want %q", got, want)
	}
	var exitErr *exec.ExitError
	if !errors.As(errors.Unwrap(err), &exitErr) {
		t.Errorf("errors.Unwrap err = %T, want *exec.ExitError", errors.Unwrap(err))
	}
}

// TestRunFailureWithoutStderr checks the message falls back to "<name>: <err>"
// when the tool wrote nothing to stderr.
func TestRunFailureWithoutStderr(t *testing.T) {
	t.Parallel()

	_, err := Run(context.Background(), nil, "sh", "-c", "exit 1")
	if err == nil {
		t.Fatal("Run = nil error, want failure")
	}
	const want = "sh: exit status 1"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestRunFeedsStdin checks that a non-nil stdin reaches the tool's stdin.
func TestRunFeedsStdin(t *testing.T) {
	t.Parallel()

	out, err := Run(context.Background(), strings.NewReader("hello stdin"), "sh", "-c", "cat")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(out), "hello stdin"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}
