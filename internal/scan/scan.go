package scan

import (
	"context"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Result is one media file found during a walk.
type Result struct {
	Path    string
	Type    MediaType
	Size    int64
	ModTime time.Time
	// DurationMs is the playback length in milliseconds for videos. 0 when
	// the file is not a video, or when probing failed (e.g. ffprobe missing
	// or corrupt file). Probed lazily in the scan workers.
	DurationMs int64
}

// WalkOptions configures Walk. The zero value matches the historical
// behaviour of Walk(ctx, root) — every video is probed via ffprobe.
type WalkOptions struct {
	// KnownDurationMs, if non-nil, is consulted before launching ffprobe
	// against a video. Returning a non-zero value short-circuits the probe.
	// Used by the controller to skip re-probing every video on incremental
	// scans when the cached duration is already in the index.
	KnownDurationMs func(path string) int64

	// SkipDurationProbe, when true, suppresses the per-video ffprobe entirely
	// so Result.DurationMs is always 0 for videos. Callers that never read the
	// duration (e.g. pv-organize, which only needs paths + dates) set this to
	// avoid forking ffprobe once per clip.
	SkipDurationProbe bool

	// OnError, if non-nil, is called for each filesystem error encountered
	// while walking (permission denied, stale symlinks, etc.). The walk
	// continues into sibling paths after the callback returns. If nil,
	// errors are silently ignored — preserving the historical behaviour.
	OnError func(path string, err error)
}

// Walk emits a Result for each media file under root and all its subdirectories.
// It skips hidden directories (names starting with '.') so the cache directory
// (.photo-viewer-cache) is never picked up.
// The channel is closed when the walk finishes or ctx is cancelled.
func Walk(ctx context.Context, root string) <-chan Result {
	return WalkWith(ctx, root, WalkOptions{})
}

// WalkWith is Walk with options. Splits the pipeline into two stages so
// the cheap d.Info()/stat work isn't blocked by the per-video ffprobe fork:
//
//	walker → metaJobs → metaWorkers (NumCPU)
//	            ├─ non-video Result → out
//	            └─ video → probeJobs → probeWorkers (NumCPU) → out
//
// Channel buffers are sized as a function of the worker counts so a fast
// SSD walker doesn't stall on a full jobs queue.
func WalkWith(ctx context.Context, root string, opts WalkOptions) <-chan Result {
	type work struct {
		path string
		t    MediaType
		d    fs.DirEntry
	}
	type vidResult struct {
		r Result
	}

	metaWorkers := runtime.NumCPU()
	if metaWorkers < 4 {
		metaWorkers = 4
	}
	probeWorkers := metaWorkers
	if probeWorkers < 4 {
		probeWorkers = 4
	}

	jobs := make(chan work, metaWorkers*256)
	probe := make(chan vidResult, probeWorkers*16)
	out := make(chan Result, metaWorkers*256)

	var metaWG sync.WaitGroup
	for i := 0; i < metaWorkers; i++ {
		metaWG.Add(1)
		go func() {
			defer metaWG.Done()
			for w := range jobs {
				info, err := w.d.Info()
				if err != nil {
					continue
				}
				r := Result{
					Path:    w.path,
					Type:    w.t,
					Size:    info.Size(),
					ModTime: info.ModTime(),
				}
				if w.t == TypeVideo {
					if opts.SkipDurationProbe {
						// Caller doesn't need durations; emit straight through
						// without forking ffprobe.
						select {
						case out <- r:
						case <-ctx.Done():
							return
						}
						continue
					}
					if opts.KnownDurationMs != nil {
						r.DurationMs = opts.KnownDurationMs(w.path)
					}
					if r.DurationMs > 0 {
						// Already known; skip the ffprobe fork entirely.
						select {
						case out <- r:
						case <-ctx.Done():
							return
						}
						continue
					}
					select {
					case probe <- vidResult{r: r}:
					case <-ctx.Done():
						return
					}
					continue
				}
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	var probeWG sync.WaitGroup
	for i := 0; i < probeWorkers; i++ {
		probeWG.Add(1)
		go func() {
			defer probeWG.Done()
			for vr := range probe {
				vr.r.DurationMs = probeVideoDurationMs(ctx, vr.r.Path)
				select {
				case out <- vr.r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if opts.OnError != nil {
					opts.OnError(path, err)
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && strings.HasPrefix(name, ".") {
					return fs.SkipDir
				}
				return nil
			}
			// Skip hidden files, matching the hidden-directory semantics above.
			// macOS-written SD cards litter every folder with AppleDouble forks
			// (._IMG_1234.jpg, ._clip.mov) whose extensions match real media;
			// without this they get indexed as ~4 KB resource-fork "media" that
			// pollute the grid and double per-directory counts.
			if strings.HasPrefix(name, ".") {
				return nil
			}
			t := DetectType(path)
			if t == TypeUnknown {
				return nil
			}
			select {
			case jobs <- work{path: path, t: t, d: d}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
		close(jobs)
		metaWG.Wait()
		close(probe)
		probeWG.Wait()
		close(out)
	}()

	return out
}
