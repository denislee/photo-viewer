package fsutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// MoveUnique moves src into dir as base, or base_N when that name is taken,
// never replacing an existing file, and returns the path src now lives at.
//
// A plain rename can't cross filesystems (EXDEV) — an SD card or a separate
// library disk is the common case — so only then does it fall back to a
// crash-safe copy (WriteUnique) that claims its name the same no-replace way,
// fsyncs dir so the new entry is durable, and only then removes src. Every other
// rename error (permission denied, disk full, …) is returned as is instead of
// being masked by a silent copy. Cancelling ctx aborts the copy and leaves src
// untouched.
func MoveUnique(ctx context.Context, src, dir, base string) (string, error) {
	dst, copied, err := MoveUniqueDeferred(ctx, src, dir, base)
	if err != nil || !copied {
		return dst, err
	}
	// The copy fsynced its data; without also syncing the directory a power
	// loss could still drop the new entry after src is gone — zero copies.
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("copied to %s but could not remove source %s: %w", dst, src, err)
	}
	return dst, nil
}

// MoveUniqueDeferred is MoveUnique with the cross-device durability work left
// to the caller. Same-filesystem sources are renamed as usual (copied=false).
// When a rename fails with EXDEV it copies src into dir and returns
// copied=true — but, unlike MoveUnique, it does NOT fsync dir and does NOT
// remove src. The caller must SyncDir(dir) once the copies in that directory
// are in place and only then remove src, so a whole batch can share one fsync
// (U5-09) while still never deleting the only copy.
func MoveUniqueDeferred(ctx context.Context, src, dir, base string) (dst string, copied bool, err error) {
	dst, err = RenameUnique(src, func(n int) string { return SuffixName(dir, base, n) })
	if !errors.Is(err, syscall.EXDEV) {
		return dst, false, err
	}
	dst, err = CopyUnique(ctx, src, dir, base)
	if err != nil {
		return "", false, err
	}
	return dst, true, nil
}

// CopyUnique copies src into dir with writeUnique and returns the path of the
// copy. src is never modified.
//
// The copy keeps src's mtime, so an imported file still sorts (and dates, via
// GetOldestMediaDate's mtime branch) by capture time instead of copy time.
func CopyUnique(ctx context.Context, src, dir, base string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	return writeUnique(ctx, dir, base, in, info.ModTime())
}

// WriteUnique streams r into dir as base, or base_N when that name is taken,
// never replacing an existing file, and returns the final path.
//
// It is crash-safe: the bytes go to a temp file in dir, are fsynced, and only
// then renamed into place with RenameUnique. An interrupted write — yanked
// card, full disk, power loss, cancelled ctx — leaves no truncated file under
// a media name (which the next inbox walk would file into the library as
// valid), and the temp is removed on every error path. The fsync before the
// rename is also what lets a caller delete the source afterwards: its bytes
// are no longer only in the page cache.
func WriteUnique(ctx context.Context, dir, base string, r io.Reader) (string, error) {
	return writeUnique(ctx, dir, base, r, time.Time{})
}

func writeUnique(ctx context.Context, dir, base string, r io.Reader, mtime time.Time) (string, error) {
	tmp, err := createTemp(dir, base)
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// An *os.File source goes through copyFileCtx so the kernel copy fast path
	// survives a cancellable ctx; any other reader (a zip entry, a network
	// body) is wrapped so cancellation stops it at the next read.
	if f, ok := r.(*os.File); ok && ctx.Done() != nil {
		err = copyFileCtx(ctx, tmp, f)
	} else {
		_, err = io.Copy(tmp, NewContextReader(ctx, r))
	}
	if err == nil {
		// A failed Sync is exactly the "bytes still only in the page cache"
		// case, so it fails the write like any copy error.
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}
	// Best-effort, matching export.copyFile: a failed stamp doesn't invalidate
	// a good copy.
	if !mtime.IsZero() {
		_ = os.Chtimes(tmpName, mtime, mtime)
	}
	dst, err := RenameUnique(tmpName, func(n int) string { return SuffixName(dir, base, n) })
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return dst, nil
}

// copyChunk is the cancellation granularity of a file-to-file copy: each chunk
// is one copy_file_range-eligible io.CopyN.
const copyChunk = 16 << 20

// copyFileCtx copies src to dst in copyChunk pieces, checking ctx between them.
// io.CopyN wraps src in an io.LimitedReader, which (*os.File).ReadFrom still
// hands to copy_file_range on Linux, so the in-kernel copy (and reflinks on
// btrfs/XFS) survives a cancellable ctx; a contextReader wrapper around the
// file would hide it and force a userspace 32 KiB loop (H4-01).
func copyFileCtx(ctx context.Context, dst *os.File, src *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := io.CopyN(dst, src, copyChunk)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// createTemp exclusively creates base.tmp in dir (base.1.tmp, … if another
// writer or a crash left one behind). O_EXCL keeps two concurrent writers of
// the same name from interleaving into one temp; mode 0666 lets the umask
// decide the final permissions, as for any newly created file (os.CreateTemp
// would leave every moved file 0600).
func createTemp(dir, base string) (*os.File, error) {
	for n := 0; ; n++ {
		name := filepath.Join(dir, base+".tmp")
		if n > 0 {
			name = filepath.Join(dir, fmt.Sprintf("%s.%d.tmp", base, n))
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if !errors.Is(err, fs.ErrExist) || n >= MaxCollisionSuffix {
			return f, err
		}
	}
}

// NewContextReader wraps r so reads fail with ctx.Err() once ctx is done. Go's
// image decoders and io.Copy pull their input in small chunks, so wrapping a
// source in one turns a cancelled context into an early error instead of a run
// to completion. A ctx that can never be cancelled (Done() == nil) returns r
// unchanged.
func NewContextReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx.Done() == nil {
		return r
	}
	return &contextReader{ctx: ctx, r: r}
}

// contextReader fails reads once ctx is cancelled, so a multi-GB copy stops at
// the next read after an interrupt instead of running to completion.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// SyncDir fsyncs a directory so a rename or create within it is itself
// durable. A file's own fsync covers its data, not the directory entry that a
// rename adds; when a copy is used as a move (the source is unlinked
// afterwards), a lost entry would leave zero copies.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Taken reports whether p is unavailable as a destination: either something
// exists there, or it can't be stat'd for a reason other than "not found"
// (e.g. EACCES). Treating the latter as taken is what lets a name search
// advance and eventually give up instead of spinning on an unreadable
// directory.
func Taken(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// FreeName returns the first of candidate(0), candidate(1), … for which taken
// is false, giving up after MaxCollisionSuffix. Dry-run planners use it with
// SuffixName so the names they print match what RenameUnique / WriteUnique
// would actually claim.
func FreeName(candidate func(n int) string, taken func(string) bool) (string, error) {
	for n := 0; n <= MaxCollisionSuffix; n++ {
		if p := candidate(n); !taken(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("gave up after %d name collisions (last tried %s)", MaxCollisionSuffix, candidate(MaxCollisionSuffix))
}

// SameDevice reports whether two paths live on the same filesystem, i.e.
// whether a rename between them can be atomic. Any stat failure returns false
// so callers take the safe copy path.
func SameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// SameContent reports whether two files hold identical bytes. It returns early
// on a size mismatch, then compares block by block and stops at the first
// difference, so a re-imported duplicate never costs two full-file hashes.
// Stat/open/read errors are returned unchanged.
func SameContent(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}

	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	// io.ReadFull keeps the two streams block-aligned despite short OS reads;
	// at end of file it returns io.EOF or io.ErrUnexpectedEOF. The sizes
	// match, so both streams end together.
	const block = 64 * 1024
	bufA := make([]byte, block)
	bufB := make([]byte, block)
	for {
		na, ea := io.ReadFull(fa, bufA)
		nb, eb := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			for _, e := range []error{ea, eb} {
				if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
					return false, e
				}
			}
			return true, nil
		}
	}
}
