package cache

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
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
// same-filesystem rename regardless of the source file's size. Falls back to
// <cacheDir>/trash if the library root is read-only. The dir name starts with
// "." so internal/scan skips it.
func TrashDir(libraryRoot, cacheDir string) (string, error) {
	if libraryRoot != "" {
		candidate := filepath.Join(libraryRoot, ".photo-viewer-trash")
		if writable(candidate) {
			return candidate, nil
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

	dst := uniqueRestorePath(meta.OriginalPath)
	if err := os.Rename(trashPath, dst); err != nil {
		return "", fmt.Errorf("rename back: %w", err)
	}
	if store != nil {
		_ = store.Rename(ThumbIDFor(trashPath), ThumbIDFor(dst))
	}
	_ = os.Remove(metaPath)
	return dst, nil
}

// uniqueRestorePath returns target if no file lives there, otherwise
// inserts " (restored)" / " (restored 2)" / ... before the extension until
// it finds a free name.
func uniqueRestorePath(target string) string {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return target
	}
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for n := 1; ; n++ {
		var candidate string
		if n == 1 {
			candidate = fmt.Sprintf("%s (restored)%s", stem, ext)
		} else {
			candidate = fmt.Sprintf("%s (restored %d)%s", stem, n, ext)
		}
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// EmptyTrash deletes every entry inside trashDir. Returns the number of
// top-level entries removed, total bytes freed (best-effort; symlinks and
// stat errors count as 0), and the first error encountered (other entries
// are still attempted).
func EmptyTrash(trashDir string) (count int, bytes int64, err error) {
	if trashDir == "" {
		return 0, 0, nil
	}
	entries, rerr := os.ReadDir(trashDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, nil
		}
		return 0, 0, rerr
	}
	for _, e := range entries {
		full := filepath.Join(trashDir, e.Name())
		isMeta := strings.HasSuffix(e.Name(), trashMetaSuffix)
		if !isMeta {
			if info, statErr := e.Info(); statErr == nil {
				if info.Mode().IsRegular() {
					bytes += info.Size()
				} else if info.IsDir() {
					bytes += dirSize(full)
				}
			}
		}
		if rmErr := os.RemoveAll(full); rmErr != nil {
			if err == nil {
				err = rmErr
			}
			continue
		}
		if !isMeta {
			count++
		}
	}
	return count, bytes, err
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
	for _, de := range dirEntries {
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
	sort.Slice(out, func(i, j int) bool {
		// Filenames lead with a UTC timestamp prefix (YYYYMMDDTHHMMSS), so a
		// reverse-lexical sort puts the most recently trashed item first.
		return filepath.Base(out[i].Path) > filepath.Base(out[j].Path)
	})
	return out
}

// TrashStats reports a best-effort count and total size of items in trashDir
// without removing anything. Errors are swallowed — this is for the settings
// readout, not a load-bearing computation.
func TrashStats(trashDir string) (count int, bytes int64) {
	if trashDir == "" {
		return 0, 0
	}
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), trashMetaSuffix) {
			continue
		}
		full := filepath.Join(trashDir, e.Name())
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		count++
		if info.Mode().IsRegular() {
			bytes += info.Size()
		} else if info.IsDir() {
			bytes += dirSize(full)
		}
	}
	return count, bytes
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
