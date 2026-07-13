package main

import (
	"fmt"
	"io"
	"log"
	"sync/atomic"
)

// progressEvery is the default number of processed files between periodic
// progress lines in the quiet (non-verbose) mode.
const progressEvery = 500

// progress tracks scan throughput and emits low-noise output. In the default
// mode it prints a single counter line every `every` files; in verbose mode it
// restores the per-file firehose. Thumb-generation errors always print. Its
// methods are safe to call from multiple worker goroutines concurrently.
type progress struct {
	out     io.Writer
	verbose bool
	every   int64
	files   atomic.Int64
	thumbs  atomic.Int64
}

// record accounts for one processed entry: path is the source file, thumb the
// generated thumbnail path ("" when none was produced), and err the generation
// error (nil on success). It counts the file (and the thumb on success), logs
// any error, and prints either the per-file line (verbose) or a periodic
// counter line (default).
func (p *progress) record(path, thumb string, err error) {
	n := p.files.Add(1)
	switch {
	case err != nil:
		log.Printf("pv-scan: thumb %s: %v", path, err)
	case thumb != "":
		p.thumbs.Add(1)
	}
	if p.verbose {
		fmt.Fprintf(p.out, "  %s -> %s\n", path, thumb)
		return
	}
	if p.every > 0 && n%p.every == 0 {
		fmt.Fprintln(p.out, p.line())
	}
}

// line is the periodic progress summary.
func (p *progress) line() string {
	return fmt.Sprintf("processed %d files (%d thumbs)", p.files.Load(), p.thumbs.Load())
}
