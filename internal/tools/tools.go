// Package tools caches external-tool availability probes. exec.LookPath walks
// every PATH entry, and a tool's presence cannot change meaningfully mid-run,
// so each probe resolves at most once per process; a process restart re-probes.
package tools

import (
	"os/exec"
	"sync"
)

// probe returns a cached predicate for whether name is on PATH.
func probe(name string) func() bool {
	return sync.OnceValue(func() bool {
		_, err := exec.LookPath(name)
		return err == nil
	})
}

var (
	// Ffmpeg reports whether ffmpeg is on PATH.
	Ffmpeg = probe("ffmpeg")
	// FFprobe reports whether ffprobe is on PATH.
	FFprobe = probe("ffprobe")
	// Exiftool reports whether exiftool is on PATH.
	Exiftool = probe("exiftool")
	// HeifConvert reports whether heif-convert is on PATH.
	HeifConvert = probe("heif-convert")
	// Udisksctl reports whether udisksctl is on PATH.
	Udisksctl = probe("udisksctl")
)
