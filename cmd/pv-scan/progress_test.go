package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
)

func TestProgressCounts(t *testing.T) {
	// Silence the error log for the error-path record.
	old := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(old)

	p := &progress{out: io.Discard, every: 0}
	p.record("a.jpg", "a.thumb", nil)         // success -> file + thumb
	p.record("b.jpg", "", errors.New("boom")) // error   -> file only
	p.record("c.mp4", "", nil)                // no thumb -> file only

	if got := p.files.Load(); got != 3 {
		t.Fatalf("files = %d, want 3", got)
	}
	if got := p.thumbs.Load(); got != 1 {
		t.Fatalf("thumbs = %d, want 1", got)
	}
	if got, want := p.line(), "processed 3 files (1 thumbs)"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestProgressPeriodic(t *testing.T) {
	var buf bytes.Buffer
	p := &progress{out: &buf, every: 4}
	for range 10 {
		p.record("x.jpg", "x.thumb", nil)
	}
	// Lines emitted at files == 4 and files == 8 only.
	lines := nonEmptyLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d periodic lines, want 2:\n%s", len(lines), buf.String())
	}
	if lines[0] != "processed 4 files (4 thumbs)" {
		t.Fatalf("first line = %q", lines[0])
	}
	if lines[1] != "processed 8 files (8 thumbs)" {
		t.Fatalf("second line = %q", lines[1])
	}
}

func TestProgressVerboseFirehose(t *testing.T) {
	var buf bytes.Buffer
	// every is small, but verbose mode must print the per-file line and never
	// the periodic counter line.
	p := &progress{out: &buf, verbose: true, every: 1}
	p.record("photo.jpg", "photo.thumb", nil)

	out := buf.String()
	if !strings.Contains(out, "photo.jpg -> photo.thumb") {
		t.Fatalf("verbose output missing per-file line: %q", out)
	}
	if strings.Contains(out, "processed") {
		t.Fatalf("verbose mode should not print the periodic counter line: %q", out)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
