package fsutil

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	switch err {
	case nil:
		return nil
	case unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP:
		// Kernel < 3.15, or a filesystem that rejects the flag. EINVAL also
		// covers genuinely invalid renames (a directory into itself); the
		// fallback's os.Rename reports those.
		return errNoReplaceUnsupported
	}
	return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
}
