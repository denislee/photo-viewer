// Package export provides reusable file-export helpers shared by the CLI
// tools (cmd/pv-export-favorites) and the GUI (Settings → Export favorites).
package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// maxCollisionSuffix caps how many "_1", "_2", … variants avoidCollision will
// try before giving up on a single file. It's a backstop: the old loop only
// exited on os.IsNotExist, so a persistent stat error on the destination
// directory (e.g. EACCES) looked like a permanent collision and spun forever.
// With the cap the worst case is a single per-file failure, never a hung run.
const maxCollisionSuffix = 10000

// Options controls how Favorites lays out and writes the exported files.
type Options struct {
	// Root is the library root; relative paths under it are preserved in
	// the destination unless Flatten is true.
	Root string
	// Dst is the destination directory. Created if it does not exist.
	Dst string
	// Flatten copies files directly under Dst (basename only), resolving
	// collisions by appending _1, _2, … before the extension.
	Flatten bool
	// Move renames the source into Dst instead of copying. Falls back to
	// copy+remove across filesystems.
	Move bool
	// DryRun reports what would happen via Progress without touching the
	// filesystem.
	DryRun bool
	// MaxLongEdge, when > 0, asks the exporter to re-encode images and
	// videos so their longest edge does not exceed this many pixels.
	// Images become JPEG (.jpg); videos become H.264 MP4 (.mp4). Sources
	// already at or below the limit are still re-encoded — that's the
	// point of the option (PNG → JPEG, lossless → lossy at fixed quality).
	// RAW files always fall back to a plain copy.
	MaxLongEdge int
	// JpegQuality is the libjpeg quality (1-100) used when recompressing
	// images. 0 selects the default (82). Ignored when MaxLongEdge == 0.
	JpegQuality int
	// VideoCRF is the libx264 constant-rate-factor (lower = better
	// quality / larger file). 0 selects the default (26). Ignored when
	// MaxLongEdge == 0.
	VideoCRF int
}

// Progress is called once per favorite. step counts attempted items
// (1-indexed) and total is the favorite count taken at the start of the
// run. action is a short verb ("COPY", "MOVE", "SKIP", "FAIL") and msg is
// the human-readable detail (typically "src -> dst" or an error string).
type Progress func(step, total int, action, msg string)

// Result summarises a Favorites run.
type Result struct {
	Done    int
	Skipped int
	Failed  int
}

// Favorites copies (or moves) every entry flagged as a favorite in idx
// into opts.Dst. ctx cancellation stops the loop between files. progress
// may be nil.
func Favorites(ctx context.Context, idx *cache.Index, opts Options, progress Progress) (Result, error) {
	if opts.Root == "" || opts.Dst == "" {
		return Result{}, fmt.Errorf("export: Root and Dst are required")
	}
	absRoot, err := filepath.Abs(opts.Root)
	if err != nil {
		return Result{}, fmt.Errorf("resolve root: %w", err)
	}
	absDst, err := filepath.Abs(opts.Dst)
	if err != nil {
		return Result{}, fmt.Errorf("resolve dst: %w", err)
	}

	if !opts.DryRun {
		if err := os.MkdirAll(absDst, 0o755); err != nil {
			return Result{}, fmt.Errorf("create dst: %w", err)
		}
	}

	jpegQ := opts.JpegQuality
	if jpegQ <= 0 {
		jpegQ = 82
	}
	crf := opts.VideoCRF
	if crf <= 0 {
		crf = 26
	}
	recompress := opts.MaxLongEdge > 0

	favs := idx.ListFavorites()
	total := len(favs)
	var res Result

	// claimed tracks destinations already planned this run so a dry-run's
	// printed paths match a real run file-for-file: two favorites that map to
	// the same name resolve to distinct destinations (base, then _1), exactly
	// as a real run's on-disk collision check would. A real run consults only
	// the filesystem (statTaken); the dry-run also consults claimed because it
	// never actually creates the files, so the disk alone can't tell it a name
	// is spoken for. Without this the old dry-run printed the same destination
	// twice and lied about the outcome.
	claimed := map[string]bool{}
	taken := func(p string) bool { return statTaken(p) }
	if opts.DryRun {
		taken = func(p string) bool { return claimed[p] || statTaken(p) }
	}

	for i, e := range favs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		step := i + 1
		src := e.Path
		if _, err := os.Stat(src); err != nil {
			res.Skipped++
			if progress != nil {
				progress(step, total, "SKIP", fmt.Sprintf("%s: %v", src, err))
			}
			continue
		}

		dest := destinationPath(absRoot, absDst, src, opts.Flatten)
		// Recompression may rewrite the extension (e.g. .png → .jpg,
		// .mov → .mp4). Resolve that BEFORE collision-checking so two
		// favorites that both become foo.jpg get distinct names.
		// (recompress and Move are mutually exclusive — the CLI rejects the
		// combination up front — but guard here too so any other caller can't
		// silently produce a recompressed name for a plain move.)
		willRecompress := recompress && !opts.Move
		if willRecompress {
			dest = recompressOutputPath(dest, e.Type)
		}
		dest, err := avoidCollision(dest, taken)
		if err != nil {
			res.Failed++
			if progress != nil {
				progress(step, total, "FAIL", fmt.Sprintf("%s: %v", src, err))
			}
			continue
		}

		action := "COPY"
		switch {
		case opts.Move:
			action = "MOVE"
		case willRecompress && e.Type == scan.TypeVideo:
			action = "TRANSCODE"
		case willRecompress && (e.Type == scan.TypePhoto || e.Type == scan.TypeHEIC):
			action = "RECOMPRESS"
		}
		if opts.DryRun {
			// Reserve the planned destination so a later same-named favorite
			// advances to _1 instead of printing the same path twice.
			claimed[dest] = true
			res.Done++
			if progress != nil {
				progress(step, total, action, fmt.Sprintf("%s -> %s", src, dest))
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			res.Failed++
			if progress != nil {
				progress(step, total, "FAIL", fmt.Sprintf("mkdir %s: %v", filepath.Dir(dest), err))
			}
			continue
		}

		var opErr error
		switch {
		case opts.Move:
			opErr = moveFile(src, dest)
		case willRecompress:
			handled, err := recompressFile(ctx, e.Type, src, dest, opts.MaxLongEdge, jpegQ, crf)
			if !handled {
				// RAW (or required tool missing) — fall back to plain copy.
				opErr = copyFile(src, dest)
				action = "COPY"
			} else {
				opErr = err
			}
		default:
			opErr = copyFile(src, dest)
		}
		if opErr != nil {
			res.Failed++
			if progress != nil {
				progress(step, total, "FAIL", fmt.Sprintf("%s -> %s: %v", src, dest, opErr))
			}
			continue
		}
		res.Done++
		if progress != nil {
			progress(step, total, action, fmt.Sprintf("%s -> %s", src, dest))
		}
	}
	return res, nil
}

// destinationPath resolves where src should land under dst. When flatten
// is true the file is placed directly under dst with just its basename;
// otherwise its path relative to root is preserved (files outside root
// fall back to the basename).
func destinationPath(root, dst, src string, flatten bool) string {
	if flatten {
		return filepath.Join(dst, filepath.Base(src))
	}
	rel, err := filepath.Rel(root, src)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Join(dst, filepath.Base(src))
	}
	return filepath.Join(dst, rel)
}

// avoidCollision returns a path that is free according to taken(), appending
// _1, _2, … before the extension. It is capped at maxCollisionSuffix so a
// pathological or unreadable destination directory produces a clean per-file
// failure instead of the old infinite loop, which only stopped on
// os.IsNotExist and therefore spun forever on any other stat error (e.g. an
// EACCES on the destination dir). taken lets the dry-run planner also treat
// already-planned names as occupied so its output matches a real run.
func avoidCollision(p string, taken func(string) bool) (string, error) {
	if !taken(p) {
		return p, nil
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i <= maxCollisionSuffix; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if !taken(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("gave up after %d collisions finding a free name for %s", maxCollisionSuffix, p)
}

// statTaken reports whether p is unavailable as a destination: either a file
// really exists there, or it can't be stat'd for a reason OTHER than "not
// found" (e.g. EACCES). Treating the latter as taken is what lets
// avoidCollision advance and eventually fail cleanly instead of hanging on an
// unreadable destination directory.
func statTaken(p string) bool {
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// copyFile copies src to dst crash-safely: it streams into a sibling dst+".tmp",
// fsyncs it to stable storage, and only then renames it into place. The fsync
// is the linchpin of -move safety: moveFile unlinks the source right after
// copyFile returns, and on a USB drive (the typical -move target) the copied
// bytes can still be entirely in the destination's page cache at that point — a
// yanked drive or power loss would then destroy the sole surviving copy of the
// favorite. On any error the temp is removed so a truncated file never lands at
// dst. Source mode and mtime are preserved on the copy.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	// Plain io.Copy: dst's .tmp is a real *os.File, so io.Copy still takes the
	// kernel copy_file_range/sendfile fast path. (An earlier version pooled a
	// 64 KiB buffer for io.CopyBuffer, but *os.File implements io.ReaderFrom so
	// the buffer was ignored — a no-op that's been reverted.)
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp) // best-effort: never leave a partial temp behind
		return err
	}
	// A failed Sync is exactly the "bytes still only in the page cache" case, so
	// treat it as a copy failure: drop the temp and report the error so no
	// caller (moveFile) deletes the source believing the copy is durable.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	// Rename is atomic within a filesystem: dst is either absent or the fully
	// written, fsynced file — never a half-copy.
	return os.Rename(tmp, dst)
}

// moveFile renames src to dst; only when that fails with EXDEV (the paths are
// on different filesystems — the usual case when -move targets a USB drive on a
// separate mount) does it fall back to a crash-safe copy + remove. Every other
// rename error (permission denied, read-only dst, …) is surfaced as a failure
// rather than silently masked as a "successful move" that switched to
// copy+delete — the old unconditional fallback hid real errors that way.
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Cross-filesystem: copy durably, then unlink the source. copyFile fsyncs
	// the file data before its rename; here we additionally fsync the
	// destination directory so the rename's directory entry is itself durable
	// before we drop the source. Otherwise a power loss could lose the entry
	// even though the file's data was synced — again leaving zero copies.
	if err := copyFile(src, dst); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	// Count the move as a success only after the source unlink succeeds; a
	// leftover source with the copy already in place is surfaced so the operator
	// can reconcile it rather than believing the move fully completed.
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copied to %s but could not remove source %s: %w", dst, src, err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename/create within it is durable. Opening
// the directory read-only and calling Sync is the portable way on Linux to
// flush the directory entry to stable storage.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
