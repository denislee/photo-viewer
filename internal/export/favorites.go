// Package export provides reusable file-export helpers shared by the CLI
// tools (cmd/pv-export-favorites) and the GUI (Settings → Export favorites).
package export

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/fsutil"
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

	// Recompressing an image is CPU-bound (~600 ms for a 12 MP → 2048 px
	// recompress), so only those jobs run on a small worker pool. Video
	// transcodes, RAW fallbacks, plain copies and moves stay inline on this
	// goroutine. Progress still has to arrive in step order, so items are
	// queued in a FIFO and reported from the head (see pendingItem).
	n := min(runtime.NumCPU(), 4)

	favs := idx.ListFavorites()
	total := len(favs)
	var res Result

	// claimed tracks destinations already planned this run so two favorites that
	// map to the same name resolve to distinct destinations (base, then _1),
	// exactly as a real run's on-disk collision check would. Both dry and real
	// runs consult it: a dry run never creates the files, and in a real run a
	// parallel recompress job may not have written its output yet when a later
	// item is planned, so the disk alone can't tell the planner a name is spoken
	// for. Every planned destination is reserved (claimed[dest] = true).
	claimed := map[string]bool{}
	taken := func(p string) bool { return claimed[p] || fsutil.Taken(p) }

	sem := make(chan struct{}, n)
	var pending []*pendingItem

	// report applies one item's outcome on the calling goroutine, preserving
	// the action/msg strings this function has always emitted.
	report := func(it *pendingItem, out pendingOutcome) {
		switch {
		case out.skip:
			res.Skipped++
			if progress != nil {
				progress(it.step, total, "SKIP", out.msg)
			}
		case out.err != nil:
			res.Failed++
			if progress != nil {
				msg := out.msg
				if msg == "" {
					msg = fmt.Sprintf("%s -> %s: %v", it.src, it.dest, out.err)
				}
				progress(it.step, total, "FAIL", msg)
			}
		default:
			res.Done++
			if progress != nil {
				progress(it.step, total, out.action, fmt.Sprintf("%s -> %s", it.src, it.dest))
			}
		}
	}

	// advance reports the item at the head of the FIFO when its outcome is
	// known, so reporting never reorders: a settled item reports at once, and a
	// parallel job is polled non-blockingly when !block or waited for when
	// block. It returns whether it reported one — false when pending is empty
	// or, with !block, the head job is still running.
	advance := func(block bool) bool {
		if len(pending) == 0 {
			return false
		}
		head := pending[0]
		out := head.out
		if head.ch != nil {
			if block {
				out = <-head.ch
			} else {
				select {
				case out = <-head.ch:
				default:
					return false
				}
			}
		}
		report(head, out)
		pending = pending[1:]
		return true
	}

	// enqueue appends one planned item in step order and reports everything that
	// is already complete. Once the FIFO grows past 2*n it blocks on its head
	// before going on, so at most 2*n items are ever outstanding.
	enqueue := func(it *pendingItem) {
		pending = append(pending, it)
		for advance(len(pending) > 2*n) {
		}
	}

	// drain waits for every outstanding item and reports it in step order.
	drain := func() {
		for advance(true) {
		}
	}

	for i, e := range favs {
		if ctx.Err() != nil {
			drain()
			return res, ctx.Err()
		}
		step := i + 1
		src := e.Path
		if _, err := os.Stat(src); err != nil {
			enqueue(&pendingItem{
				step: step,
				src:  src,
				out:  pendingOutcome{msg: fmt.Sprintf("%s: %v", src, err), skip: true},
			})
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
			enqueue(&pendingItem{
				step: step,
				src:  src,
				out:  pendingOutcome{msg: fmt.Sprintf("%s: %v", src, err), err: err},
			})
			continue
		}
		// Reserve the planned destination before any worker can write it, so a
		// later same-named favorite advances to _1 instead of racing onto it.
		claimed[dest] = true

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
			enqueue(&pendingItem{
				step: step,
				src:  src,
				dest: dest,
				out:  pendingOutcome{action: action},
			})
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			enqueue(&pendingItem{
				step: step,
				src:  src,
				dest: dest,
				out:  pendingOutcome{msg: fmt.Sprintf("mkdir %s: %v", filepath.Dir(dest), err), err: err},
			})
			continue
		}

		if willRecompress && (e.Type == scan.TypePhoto || e.Type == scan.TypeHEIC) {
			// Only image recompresses are parallel: every other action would
			// either fork a tool, stream a whole file, or mutate the source, and
			// keeping those inline makes their progress ordering obvious.
			it := &pendingItem{step: step, src: src, dest: dest, ch: make(chan pendingOutcome, 1)}
			go func(it *pendingItem, typ scan.MediaType, src, dest, action string) {
				sem <- struct{}{}
				act, err := recompressOrCopy(ctx, typ, src, dest, opts.MaxLongEdge, jpegQ, crf, action)
				<-sem
				it.ch <- pendingOutcome{action: act, err: err}
			}(it, e.Type, src, dest, action)
			enqueue(it)
			continue
		}

		var opErr error
		switch {
		case opts.Move:
			opErr = fsutil.MoveTo(ctx, src, dest)
		case willRecompress:
			action, opErr = recompressOrCopy(ctx, e.Type, src, dest, opts.MaxLongEdge, jpegQ, crf, action)
		default:
			opErr = fsutil.CopyTo(ctx, src, dest)
		}
		enqueue(&pendingItem{
			step: step,
			src:  src,
			dest: dest,
			out:  pendingOutcome{action: action, err: opErr},
		})
	}
	drain()
	return res, nil
}

// pendingItem is one favorite's place in the ordered progress FIFO. A parallel
// recompress carries its outcome on ch; every other item (copy, move,
// transcode, skip, planning failure) is already complete and stores it in out.
type pendingItem struct {
	step int
	src  string
	dest string
	out  pendingOutcome
	ch   chan pendingOutcome
}

// pendingOutcome is the result of one planned item. msg, when non-empty, is the
// exact progress text for a SKIP or a planning-time FAIL; otherwise the
// reporter formats "<src> -> <dest>" (success) or "<src> -> <dest>: <err>".
type pendingOutcome struct {
	action string
	err    error
	msg    string
	skip   bool
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

// avoidCollision returns p, or the first free p_1, p_2, … according to taken
// (fsutil.FreeName over fsutil.SuffixName, so it gives up cleanly instead of
// spinning on an unreadable directory). taken lets the dry-run planner also
// treat already-planned names as occupied so its output matches a real run.
func avoidCollision(p string, taken func(string) bool) (string, error) {
	dir, base := filepath.Dir(p), filepath.Base(p)
	return fsutil.FreeName(func(n int) string { return fsutil.SuffixName(dir, base, n) }, taken)
}

// recompressOrCopy re-encodes src into dest at the given limits, falling back
// to a plain copy when recompression didn't handle the type (RAW) or a required
// tool is missing. It returns the action to report: the given action when
// recompression ran (success or hard failure), or "COPY" for the fallback —
// used by both the pooled and the inline recompress paths (S6-19).
func recompressOrCopy(ctx context.Context, t scan.MediaType, src, dest string, maxEdge, jpegQ, crf int, action string) (string, error) {
	handled, err := recompressFile(ctx, t, src, dest, maxEdge, jpegQ, crf)
	if !handled {
		// RAW (or required tool missing) — fall back to plain copy.
		return "COPY", fsutil.CopyTo(ctx, src, dest)
	}
	return action, err
}
