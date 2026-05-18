package scan

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// ffprobeOnce caches whether ffprobe is available on PATH. We probe a lot of
// files per scan, so a per-call exec.LookPath would dominate when the binary
// is missing.
var (
	ffprobeChecked bool
	ffprobeOK      bool
	ffprobeMu      sync.Mutex
)

func haveFFprobe() bool {
	ffprobeMu.Lock()
	defer ffprobeMu.Unlock()
	if !ffprobeChecked {
		_, err := exec.LookPath("ffprobe")
		ffprobeOK = err == nil
		ffprobeChecked = true
	}
	return ffprobeOK
}

// probeVideoDurationMs returns the playback length of path in milliseconds,
// or 0 if ffprobe is missing or fails. Stream-level duration is preferred
// over container duration since some formats (MTS, raw H.264) report the
// container as N/A but still expose a per-stream duration.
func probeVideoDurationMs(ctx context.Context, path string) int64 {
	if !haveFFprobe() {
		return 0
	}
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
		return 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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
	return 0
}
