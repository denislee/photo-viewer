package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// CopyTo copies src to exactly dst, crash-safely: the bytes go to a temp file
// next to dst (createTemp), are fsynced, get src's permission bits and mtime,
// and are then renamed into place with RenameNoReplace, so an existing dst is
// never replaced (the error then satisfies errors.Is(err, fs.ErrExist)).
// Cancelling ctx aborts the copy. The temp is removed on every error path.
func CopyTo(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}

	tmp, err := createTemp(filepath.Dir(dst), filepath.Base(dst))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := fillTemp(ctx, tmp, in, info.Mode().Perm()); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Best-effort, matching CopyUnique: a failed stamp doesn't invalidate a
	// good copy.
	_ = os.Chtimes(tmpName, info.ModTime(), info.ModTime())
	if err := RenameNoReplace(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// MoveTo renames src to exactly dst without replacing an existing dst. Only
// when the rename fails with EXDEV (different filesystems — the usual case
// when the destination is a USB drive on a separate mount) does it fall back
// to CopyTo, fsync dst's directory (SyncDir) so the new entry is durable, and
// only then remove src. Every other rename error is returned as is.
func MoveTo(ctx context.Context, src, dst string) error {
	err := RenameNoReplace(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Cross-filesystem: copy durably, then unlink the source. CopyTo fsyncs the
	// file data before its rename; here we additionally fsync the destination
	// directory so the rename's directory entry is itself durable before we
	// drop the source. Otherwise a power loss could lose the entry even though
	// the file's data was synced — leaving zero copies.
	if err := CopyTo(ctx, src, dst); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	// Count the move as a success only after the source unlink succeeds; a
	// leftover source with the copy already in place is surfaced so the caller
	// can reconcile it rather than believing the move fully completed.
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copied to %s but could not remove source %s: %w", dst, src, err)
	}
	return nil
}

// WriteFileAtomic replaces p with data crash-safely: a temp file in p's
// directory is written, chmodded to perm, fsynced and renamed over p, then the
// directory is fsynced. A crash mid-write leaves the previous file intact
// instead of a truncated one. The directory is created (0o755) if missing.
func WriteFileAtomic(p string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncDir(dir)
}
