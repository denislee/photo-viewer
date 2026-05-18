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

// Walk emits a Result for each media file under root and all its subdirectories.
// It skips hidden directories (names starting with '.') so the cache directory
// (.photo-viewer-cache) is never picked up.
// The channel is closed when the walk finishes or ctx is cancelled.
func Walk(ctx context.Context, root string) <-chan Result {
	out := make(chan Result, 1024)

	type work struct {
		path string
		t    MediaType
		d    fs.DirEntry
	}
	jobs := make(chan work, 1024)
	var wg sync.WaitGroup

	numWorkers := runtime.NumCPU() * 2
	if numWorkers < 16 {
		numWorkers = 16
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				info, err := w.d.Info()
				if err == nil {
					r := Result{
						Path:    w.path,
						Type:    w.t,
						Size:    info.Size(),
						ModTime: info.ModTime(),
					}
					if w.t == TypeVideo {
						r.DurationMs = probeVideoDurationMs(ctx, w.path)
					}
					select {
					case out <- r:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	go func() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
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
		wg.Wait()
		close(out)
	}()

	return out
}
