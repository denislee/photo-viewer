package scan

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// Result is one media file found during a walk.
type Result struct {
	Path    string
	Type    MediaType
	Size    int64
	ModTime time.Time
}

// Walk emits a Result for each media file under root and all its subdirectories.
// It skips hidden directories (names starting with '.') so the cache directory
// (.photo-viewer-cache) is never picked up.
// The channel is closed when the walk finishes or ctx is cancelled.
func Walk(ctx context.Context, root string) <-chan Result {
	out := make(chan Result, 64)
	go func() {
		defer close(out)
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
			info, err := d.Info()
			if err != nil {
				return nil
			}
			r := Result{
				Path:    path,
				Type:    t,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			}
			select {
			case out <- r:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	return out
}
