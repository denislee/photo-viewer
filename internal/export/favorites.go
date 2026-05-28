// Package export provides reusable file-export helpers shared by the CLI
// tools (cmd/pv-export-favorites) and the GUI (Settings → Export favorites).
package export

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

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
		willRecompress := recompress && !opts.Move
		if willRecompress {
			dest = recompressOutputPath(dest, e.Type)
		}
		dest = avoidCollision(dest)

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

// avoidCollision returns a path that does not yet exist by appending _1,
// _2, … before the extension.
func avoidCollision(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

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
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	return os.Rename(tmp, dst)
}

// moveFile renames src to dst; if that fails because the paths are on
// different filesystems, it falls back to copy + remove.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}
