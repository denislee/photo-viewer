package scan

import (
	"context"
	"io/fs"
)

// WalkMediaPaths calls fn for every media file under root, with the same rules
// as the indexer's walk (walkMedia in scan.go): hidden directories (other than
// root) and hidden files — including macOS ._* AppleDouble forks — are
// skipped, and only DetectType != TypeUnknown files are visited. Walk errors
// are ignored. fn may return fs.SkipAll to stop early; any other non-nil error
// aborts the walk and is returned. Returns ctx.Err() if ctx is cancelled.
func WalkMediaPaths(ctx context.Context, root string, fn func(path string, t MediaType) error) error {
	err := walkMedia(ctx, root, nil, func(p string, t MediaType, _ fs.DirEntry) error {
		return fn(p, t)
	})
	// filepath.WalkDir already turns fs.SkipAll into nil, so the walk's error
	// is returned as is.
	return err
}

// WalkVisibleFiles calls fn for every regular (non-directory) file under root
// that is not hidden and not inside a hidden directory (same hidden rules as
// WalkMediaPaths) without any type filter, for callers that also need
// non-media files such as ZIP archives. fn may return fs.SkipAll to stop
// early; any other non-nil error aborts the walk and is returned. Walk errors
// are ignored. Returns ctx.Err() if ctx is cancelled.
func WalkVisibleFiles(ctx context.Context, root string, fn func(path string) error) error {
	return walkVisible(ctx, root, nil, func(p string, _ fs.DirEntry) error { return fn(p) })
}
