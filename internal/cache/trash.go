package cache

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/scan"
	"golang.org/x/sys/unix"
)

// trashMetaSuffix is appended to a trashed file's name to form the sidecar
// metadata path. Restore reads the original location from this file.
const trashMetaSuffix = ".meta"

// trashMeta is the sidecar JSON written next to each trashed file. Keeps the
// original absolute path so Restore can put the file back where it came from
// across app restarts.
type trashMeta struct {
	OriginalPath string `json:"original_path"`
	TrashedAt    string `json:"trashed_at"`
}

// TrashDir returns the directory used for soft-deleted files. It prefers
// <libraryRoot>/.photo-viewer-trash so MoveToTrash can use an atomic
// same-filesystem rename regardless of the source file's size — but it only
// decides on it: the directory itself is created lazily by MoveToTrash on the
// first delete. Writability is checked with access(2) W_OK on the existing
// trash dir or on the library root, so startup writes no directory and no
// probe file into the library (C4-10). Falls back to <cacheDir>/trash when the
// library root is read-only. The dir name starts with "." so internal/scan
// skips it.
func TrashDir(libraryRoot, cacheDir string) (string, error) {
	if libraryRoot != "" {
		candidate := filepath.Join(libraryRoot, ".photo-viewer-trash")
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() && unix.Access(candidate, unix.W_OK) == nil {
				return candidate, nil
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if info, err := os.Stat(libraryRoot); err == nil && info.IsDir() && unix.Access(libraryRoot, unix.W_OK) == nil {
				return candidate, nil
			}
		}
	}
	if cacheDir == "" {
		return "", errors.New("no writable trash location")
	}
	fallback := filepath.Join(cacheDir, "trash")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		return "", err
	}
	return fallback, nil
}

// MoveToTrash moves src into trashDir under a unique name that preserves the
// original basename for later inspection. On a same-filesystem trash dir this
// is an O(1) rename, which is the whole point. Returns the destination path.
//
// If src and trashDir live on different filesystems, rename returns
// syscall.EXDEV and MoveToTrash gives up — the caller should fall back to a
// plain os.Remove (the rename being the only fast path; doing a copy+delete
// here would defeat the purpose).
func MoveToTrash(src, trashDir string) (string, error) {
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", err
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		absSrc = src
	}
	base := filepath.Base(absSrc)
	sum := sha1.Sum([]byte(absSrc))
	stamp := time.Now().UTC().Format("20060102T150405")
	name := fmt.Sprintf("%s-%x-%s", stamp, sum[:4], base)
	dst := filepath.Join(trashDir, name)
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	// Best-effort sidecar — the file is already in trash even if this fails;
	// without it Restore won't know where the file came from, but the user
	// can still Empty Trash to free space.
	meta := trashMeta{OriginalPath: absSrc, TrashedAt: time.Now().UTC().Format(time.RFC3339)}
	if data, mErr := json.Marshal(meta); mErr == nil {
		_ = os.WriteFile(dst+trashMetaSuffix, data, 0o644)
	}
	return dst, nil
}

// RestoreFromTrash moves trashPath back to the location recorded in its
// sidecar metadata. If the original destination already has a file, the
// restored name has " (restored N)" appended before the extension so nothing
// gets clobbered. The thumbnail is renamed back via store (if non-nil) so
// the cached preview survives the round-trip. The sidecar meta file is
// removed on success.
//
// Returns the final on-disk path of the restored file. Returns an error if
// the sidecar is missing or unparseable — in that case the caller has no
// safe place to put the file and has to fall through to a manual workflow.
func RestoreFromTrash(trashPath string, store *ThumbStore) (string, error) {
	if trashPath == "" {
		return "", errors.New("empty trash path")
	}
	metaPath := trashPath + trashMetaSuffix
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", fmt.Errorf("read trash metadata: %w", err)
	}
	var meta trashMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse trash metadata: %w", err)
	}
	if meta.OriginalPath == "" {
		return "", errors.New("trash metadata missing original_path")
	}

	// Recreate the parent dir in case the user deleted it after sending the
	// file to trash. Without this, rename would fail with ENOENT.
	parent := filepath.Dir(meta.OriginalPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("recreate parent: %w", err)
	}

	// Claim the name atomically: a stat-then-rename would silently replace a
	// file created at the chosen name in between (C-16).
	dst, err := fsutil.RenameUnique(trashPath, func(n int) string {
		return restoreCandidate(meta.OriginalPath, n)
	})
	if err != nil {
		return "", fmt.Errorf("rename back: %w", err)
	}
	if store != nil {
		_ = store.Rename(ThumbIDFor(trashPath), ThumbIDFor(dst))
	}
	_ = os.Remove(metaPath)
	return dst, nil
}

// restoreCandidate returns the n-th restore name for target: n == 0 is target
// itself, then " (restored)", " (restored 2)", … inserted before the extension.
func restoreCandidate(target string, n int) string {
	if n == 0 {
		return target
	}
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	if n == 1 {
		return fmt.Sprintf("%s (restored)%s", stem, ext)
	}
	return fmt.Sprintf("%s (restored %d)%s", stem, n, ext)
}

// EmptyTrash deletes every entry inside trashDir. Returns the number of
// top-level media entries removed, total bytes freed (best-effort; items whose
// Info fails are still removed but count as 0), and the first error encountered
// (other entries are still attempted).
func EmptyTrash(trashDir string) (count int, bytes int64, err error) {
	lerr := trashItems(trashDir, func(full string, n int64, ok bool) {
		if rmErr := os.RemoveAll(full); rmErr != nil {
			if err == nil {
				err = rmErr
			}
			return
		}
		if ok {
			count++
			bytes += n
		}
	})
	if err == nil {
		err = lerr
	}
	return count, bytes, err
}

// trashItems calls visit for every top-level entry of trashDir with its on-disk
// size. ok is false when the entry's Info fails or when it is a ".meta"
// sidecar, in which case n is 0 and callers skip counting it. A missing
// trashDir yields no items.
func trashItems(trashDir string, visit func(full string, n int64, ok bool)) error {
	if trashDir == "" {
		return nil
	}
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		full := filepath.Join(trashDir, e.Name())
		isMeta := strings.HasSuffix(e.Name(), trashMetaSuffix)
		var n int64
		var ok bool
		if !isMeta {
			var statErr error
			if n, statErr = trashItemBytes(full, e); statErr == nil {
				ok = true
			} else {
				n = 0
			}
		}
		visit(full, n, ok)
	}
	return nil
}

// ListTrash returns one Entry per file inside trashDir, suitable for handing
// to the grid/viewer the same way a normal directory listing would be. Newest
// first (by trash-time, which is encoded in the filename prefix). Files whose
// extension doesn't match a known media format are skipped — the trash dir
// shouldn't contain anything else, but ext-detection is the same gate the
// regular scanner uses.
func ListTrash(trashDir string) []Entry {
	if trashDir == "" {
		return nil
	}
	dirEntries, err := os.ReadDir(trashDir)
	if err != nil {
		return nil
	}
	out := make([]Entry, 0, len(dirEntries))
	// os.ReadDir returns entries sorted by filename ascending, and trash names
	// lead with a UTC timestamp (YYYYMMDDTHHMMSS), so walking the slice in
	// reverse yields newest first without a sort.
	for _, de := range slices.Backward(dirEntries) {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if strings.HasSuffix(name, trashMetaSuffix) {
			continue
		}
		// Trash basenames look like <stamp>-<hash>-<originalBasename>. The
		// original basename retains its extension, so DetectType still works
		// on the full filename without us having to parse the prefix.
		t := scan.DetectType(name)
		if t == scan.TypeUnknown {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(trashDir, name)
		out = append(out, Entry{
			Path:    full,
			Type:    t,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			ThumbID: ThumbIDFor(full),
		})
	}
	return out
}

// TrashStats reports a best-effort count and total size of items in trashDir
// without removing anything. Errors are swallowed — this is for the settings
// readout, not a load-bearing computation.
func TrashStats(trashDir string) (count int, bytes int64) {
	_ = trashItems(trashDir, func(_ string, n int64, ok bool) {
		if ok {
			count++
			bytes += n
		}
	})
	return count, bytes
}

// trashItemBytes returns the on-disk size of one trash entry: the size of a
// regular file, the recursive total for a directory, and 0 for anything else
// (symlinks, devices, …).
func trashItemBytes(full string, e os.DirEntry) (int64, error) {
	info, err := e.Info()
	if err != nil {
		return 0, err
	}
	switch {
	case info.Mode().IsRegular():
		return info.Size(), nil
	case info.IsDir():
		return dirSize(full), nil
	}
	return 0, nil
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
