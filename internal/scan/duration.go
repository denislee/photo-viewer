package scan

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/tools"
)

// DurationProbeFailed is the persisted sentinel for a video whose ffprobe run
// found no duration (corrupt or unreadable). It is stored so incremental scans
// can skip the fork for that same file version rather than re-probing it every
// time. Any value <= 0 means "unknown" to display code.
const DurationProbeFailed int64 = -1

// probeTimeout bounds a single ffprobe call. Derived per-probe from the scan
// ctx so a wedged ffprobe (a truncated or malformed file it can't parse)
// returns the "unknown duration" sentinel and frees the scan worker, instead
// of parking that worker on a dead child until the *entire* scan is cancelled.
const probeTimeout = 10 * time.Second

// haveFFprobe reports whether ffprobe is available on PATH, cached in
// internal/tools. We probe a lot of files per scan, so a per-call
// exec.LookPath would dominate when the binary is missing.
var haveFFprobe = tools.FFprobe

// probeVideoDurationMs returns the playback length of path in milliseconds.
// It returns 0 when ffprobe is missing (nothing to record — a later scan may
// have it) or when the scan itself was cancelled, and DurationProbeFailed when
// ffprobe ran on this file version but produced no positive duration (corrupt
// or unreadable), so the failure is persisted and not re-probed every scan.
// Stream-level duration is preferred over container duration since some
// formats (MTS, raw H.264) report the container as N/A but still expose a
// per-stream duration.
func probeVideoDurationMs(ctx context.Context, path string) int64 {
	if !haveFFprobe() {
		return 0
	}
	// Bound this probe independently of the whole-scan ctx (see probeTimeout).
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "format=duration:stream=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		// A cancelled scan isn't the file's fault: leave the duration unknown
		// (0) so the next complete scan probes it again, instead of persisting
		// a probe-failure sentinel for a perfectly good video.
		if parent.Err() != nil {
			return 0
		}
		return DurationProbeFailed
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "N/A" {
			continue
		}
		f, err := strconv.ParseFloat(line, 64)
		if err != nil || f <= 0 {
			continue
		}
		return int64(f * 1000)
	}
	return DurationProbeFailed
}
