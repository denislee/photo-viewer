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
	dst, err := RenameUnique(src, func(n int) string { return SuffixName(dir, base, n) })
	if !errors.Is(err, syscall.EXDEV) {
		return dst, err
	}
	dst, err = CopyUnique(ctx, src, dir, base)
	if err != nil {
		return "", err
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

// CopyUnique copies src into dir with WriteUnique and returns the path of the
// copy. src is never modified.
func CopyUnique(ctx context.Context, src, dir, base string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	return WriteUnique(ctx, dir, base, in)
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
	tmp, err := createTemp(dir, base)
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// Plain io.Copy keeps the kernel copy_file_range/sendfile fast path when r
	// is an *os.File; ctxReader only interposes when ctx can be cancelled.
	if ctx.Done() != nil {
		r = ctxReader{ctx, r}
	}
	_, err = io.Copy(tmp, r)
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
	dst, err := RenameUnique(tmpName, func(n int) string { return SuffixName(dir, base, n) })
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return dst, nil
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

// ctxReader fails reads once ctx is cancelled, so a multi-GB copy stops at the
// next read after an interrupt instead of running to completion.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
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
