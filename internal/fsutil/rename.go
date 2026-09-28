// Package fsutil holds the filesystem primitives shared by every flow that
// moves or copies the user's files (import, organize, pv-organize, trash
// restore, selection and favorites export): no-replace renames, collision
// suffixes, and crash-safe copies.
package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MaxCollisionSuffix caps how many candidate names RenameUnique tries before
// giving up, so an unwritable or pathological directory fails one file instead
// of spinning forever.
const MaxCollisionSuffix = 10000

// RenameNoReplace renames src to dst but never replaces an existing dst: if
// dst exists it fails with an error satisfying errors.Is(err, fs.ErrExist) and
// leaves both files untouched. A plain os.Rename silently clobbers dst, so a
// stat-then-rename leaves a window in which a file created at dst is lost.
//
// It uses renameat2(RENAME_NOREPLACE) where the kernel and filesystem support
// it (one atomic syscall), else an os.Link claim (fails EEXIST) plus removing
// src, and only on filesystems with neither (e.g. some FUSE mounts) falls back
// to an Lstat check before os.Rename. Like os.Rename, a cross-filesystem move
// fails with an error satisfying errors.Is(err, syscall.EXDEV).
func RenameNoReplace(src, dst string) error {
	err := renameNoReplace(src, dst)
	if !errors.Is(err, errNoReplaceUnsupported) {
		return err
	}
	return linkOrCheckedRename(src, dst)
}

// errNoReplaceUnsupported reports that the atomic rename primitive isn't
// available for this platform, kernel, or filesystem.
var errNoReplaceUnsupported = errors.New("rename without replace unsupported")

// linkOrCheckedRename is RenameNoReplace's portable fallback.
func linkOrCheckedRename(src, dst string) error {
	err := os.Link(src, dst)
	switch {
	case err == nil:
		if rmErr := os.Remove(src); rmErr != nil {
			// Roll the claim back so a failed move leaves just the source,
			// exactly like a failed rename would.
			_ = os.Remove(dst)
			return rmErr
		}
		return nil
	case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.EXDEV):
		return err
	}
	// Hard links unsupported (FAT/exFAT surface EPERM) or src is a directory:
	// check, then rename. The check-to-rename window only remains on
	// filesystems that support neither primitive above.
	if _, err := os.Lstat(dst); err == nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

// SuffixName returns the n-th candidate name for base in dir: n == 0 is the
// bare name, n > 0 inserts "_n" before the extension (IMG_1.jpg → IMG_1_2.jpg).
func SuffixName(dir, base string, n int) string {
	if n == 0 {
		return filepath.Join(dir, base)
	}
	ext := filepath.Ext(base)
	return filepath.Join(dir, fmt.Sprintf("%s_%d%s", strings.TrimSuffix(base, ext), n, ext))
}

// RenameUnique renames src to candidate(0), or the first of candidate(1),
// candidate(2), … that is free, never replacing an existing file. It returns
// the path src now lives at. Errors other than "name taken" (including EXDEV)
// are returned immediately.
func RenameUnique(src string, candidate func(n int) string) (string, error) {
	for n := 0; n <= MaxCollisionSuffix; n++ {
		dst := candidate(n)
		err := RenameNoReplace(src, dst)
		if err == nil {
			return dst, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("gave up after %d name collisions for %s (last tried %s)", MaxCollisionSuffix, src, candidate(MaxCollisionSuffix))
}
