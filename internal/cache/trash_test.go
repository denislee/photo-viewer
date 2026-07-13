package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/scan"
)

// writeMedia creates a file at path with the given contents, making parent
// directories as needed. Used to stage originals that the trash lifecycle acts
// on.
func writeMedia(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestMoveToTrash pins the data-safety contract of a soft delete: the file is
// renamed into the trash dir under a name that keeps the original basename, the
// original location is emptied, and a sidecar .meta records the absolute
// original path so a later restore knows where it came from.
func TestMoveToTrash(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")
	orig := filepath.Join(root, "IMG_0001.jpg")
	const content = "original-bytes"
	writeMedia(t, orig, content)

	dst, err := MoveToTrash(orig, trashDir)
	if err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}

	// Original is gone; the bytes moved intact into the trash under trashDir.
	if exists(orig) {
		t.Errorf("original still present at %s after MoveToTrash", orig)
	}
	if filepath.Dir(dst) != trashDir {
		t.Errorf("trash dst %s not under trashDir %s", dst, trashDir)
	}
	if got := filepath.Base(dst); got[len(got)-len("IMG_0001.jpg"):] != "IMG_0001.jpg" {
		t.Errorf("trash basename %q does not preserve original basename", got)
	}
	if got := readFile(t, dst); got != content {
		t.Errorf("trashed content = %q, want %q", got, content)
	}

	// Sidecar records the absolute original path plus a timestamp.
	metaPath := dst + trashMetaSuffix
	if !exists(metaPath) {
		t.Fatalf("sidecar meta missing at %s", metaPath)
	}
	var meta trashMeta
	if err := json.Unmarshal([]byte(readFile(t, metaPath)), &meta); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	absOrig, _ := filepath.Abs(orig)
	if meta.OriginalPath != absOrig {
		t.Errorf("meta OriginalPath = %q, want %q", meta.OriginalPath, absOrig)
	}
	if meta.TrashedAt == "" {
		t.Errorf("meta TrashedAt is empty")
	}
}

// TestMoveToTrashRenameFailure exercises the give-up branch: when os.Rename
// fails, MoveToTrash returns the error and does NOT fall back to a copy+delete.
// A genuine cross-filesystem EXDEV cannot be reproduced headlessly (t.TempDir is
// a single filesystem), but a missing source drives the same
// "rename failed -> return err, write nothing" path the EXDEV contract relies on.
func TestMoveToTrashRenameFailure(t *testing.T) {
	tmp := t.TempDir()
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")
	missing := filepath.Join(tmp, "lib", "does-not-exist.jpg")

	dst, err := MoveToTrash(missing, trashDir)
	if err == nil {
		t.Fatalf("MoveToTrash on missing source returned nil error (dst=%q)", dst)
	}
	if dst != "" {
		t.Errorf("MoveToTrash returned dst %q on failure, want empty", dst)
	}
	// No copy fallback: nothing was written into the trash dir.
	if entries, _ := os.ReadDir(trashDir); len(entries) != 0 {
		t.Errorf("trash dir has %d entries after a failed move, want 0 (no copy fallback)", len(entries))
	}
}

// TestRestoreFromTrashRoundTrip verifies a trashed file returns to its exact
// original path with its bytes intact, the sidecar is removed, and — when a
// ThumbStore is wired — the cached thumbnail is re-keyed from the trash-path ID
// to the restored-path ID so the preview survives the round-trip.
func TestRestoreFromTrashRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")
	orig := filepath.Join(root, "IMG_0002.jpg")
	const content = "round-trip-bytes"
	writeMedia(t, orig, content)

	dst, err := MoveToTrash(orig, trashDir)
	if err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}

	// Stage a thumbnail under the trash-path ID, as the delete flow leaves it.
	store, err := NewThumbStore(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatalf("NewThumbStore: %v", err)
	}
	oldThumb := store.thumbPath(ThumbIDFor(dst))
	if err := os.MkdirAll(filepath.Dir(oldThumb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldThumb, []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreFromTrash(dst, store)
	if err != nil {
		t.Fatalf("RestoreFromTrash: %v", err)
	}

	if restored != orig {
		t.Errorf("restored path = %q, want original %q", restored, orig)
	}
	if got := readFile(t, restored); got != content {
		t.Errorf("restored content = %q, want %q", got, content)
	}
	if exists(dst) {
		t.Errorf("trash file still present at %s after restore", dst)
	}
	if exists(dst + trashMetaSuffix) {
		t.Errorf("sidecar meta still present after restore")
	}
	// Thumb re-keyed: old ID gone, new ID present.
	if exists(oldThumb) {
		t.Errorf("thumb still at old ID path %s after restore", oldThumb)
	}
	newThumb := store.thumbPath(ThumbIDFor(restored))
	if !exists(newThumb) {
		t.Errorf("thumb not relocated to restored ID path %s", newThumb)
	}
}

// TestRestoreFromTrashCollision drives the uniqueRestorePath loop through the
// public API: when the original location is re-occupied before a restore, the
// file comes back under a "(restored)" name instead of clobbering the occupant.
func TestRestoreFromTrashCollision(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")
	orig := filepath.Join(root, "photo.jpg")
	writeMedia(t, orig, "trashed")

	dst, err := MoveToTrash(orig, trashDir)
	if err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}

	// Something new now occupies the original path.
	writeMedia(t, orig, "occupant")

	restored, err := RestoreFromTrash(dst, nil)
	if err != nil {
		t.Fatalf("RestoreFromTrash: %v", err)
	}

	want := filepath.Join(root, "photo (restored).jpg")
	if restored != want {
		t.Errorf("restored path = %q, want %q", restored, want)
	}
	if got := readFile(t, restored); got != "trashed" {
		t.Errorf("restored content = %q, want %q", got, "trashed")
	}
	if got := readFile(t, orig); got != "occupant" {
		t.Errorf("occupant clobbered: content = %q, want %q", got, "occupant")
	}
}

// TestUniqueRestorePath unit-tests the collision loop directly: the first free
// name is returned unchanged, and each successive occupied name adds the next
// "(restored N)" suffix before the extension.
func TestTrashUniqueRestorePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clip.mov")

	// Nothing there yet -> target as-is.
	if got := uniqueRestorePath(target); got != target {
		t.Errorf("uniqueRestorePath on free name = %q, want %q", got, target)
	}

	writeMedia(t, target, "a")
	want1 := filepath.Join(dir, "clip (restored).mov")
	if got := uniqueRestorePath(target); got != want1 {
		t.Errorf("first collision = %q, want %q", got, want1)
	}

	writeMedia(t, want1, "b")
	want2 := filepath.Join(dir, "clip (restored 2).mov")
	if got := uniqueRestorePath(target); got != want2 {
		t.Errorf("second collision = %q, want %q", got, want2)
	}

	writeMedia(t, want2, "c")
	want3 := filepath.Join(dir, "clip (restored 3).mov")
	if got := uniqueRestorePath(target); got != want3 {
		t.Errorf("third collision = %q, want %q", got, want3)
	}
}

// TestRestoreRecreatesMissingParent covers the ENOENT guard: if the original
// parent directory was deleted after the file went to trash, RestoreFromTrash
// recreates it rather than failing the rename.
func TestTrashRestoreRecreatesMissingParent(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")
	orig := filepath.Join(root, "sub", "deep", "pic.jpg")
	writeMedia(t, orig, "bytes")

	dst, err := MoveToTrash(orig, trashDir)
	if err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}

	// User removes the whole subtree that held the original.
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreFromTrash(dst, nil)
	if err != nil {
		t.Fatalf("RestoreFromTrash with missing parent: %v", err)
	}
	if restored != orig {
		t.Errorf("restored path = %q, want %q", restored, orig)
	}
	if !exists(orig) {
		t.Errorf("restored file not present at %s (parent not recreated?)", orig)
	}
}

// TestRestoreFromTrashErrors covers the refuse-to-guess contract: without a
// usable sidecar there is no safe destination, so RestoreFromTrash errors
// instead of inventing one.
func TestRestoreFromTrashErrors(t *testing.T) {
	tmp := t.TempDir()

	t.Run("empty path", func(t *testing.T) {
		if _, err := RestoreFromTrash("", nil); err == nil {
			t.Error("want error for empty trash path")
		}
	})

	t.Run("missing sidecar", func(t *testing.T) {
		p := filepath.Join(tmp, "no-meta.jpg")
		writeMedia(t, p, "x")
		if _, err := RestoreFromTrash(p, nil); err == nil {
			t.Error("want error when sidecar meta is absent")
		}
	})

	t.Run("unparseable sidecar", func(t *testing.T) {
		p := filepath.Join(tmp, "bad-meta.jpg")
		writeMedia(t, p, "x")
		writeMedia(t, p+trashMetaSuffix, "{not json")
		if _, err := RestoreFromTrash(p, nil); err == nil {
			t.Error("want error for unparseable sidecar")
		}
	})

	t.Run("blank original path", func(t *testing.T) {
		p := filepath.Join(tmp, "blank-orig.jpg")
		writeMedia(t, p, "x")
		writeMedia(t, p+trashMetaSuffix, `{"original_path":""}`)
		if _, err := RestoreFromTrash(p, nil); err == nil {
			t.Error("want error when sidecar has no original_path")
		}
	})
}

// TestEmptyTrash asserts the reported count is the number of media (non-meta)
// entries removed, the reported bytes sum only those media files, and the trash
// dir is fully emptied — sidecars included.
func TestEmptyTrash(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")

	// Distinct source paths -> distinct sha1 hashes -> distinct trash names,
	// so nothing collides even within the same second.
	contents := []string{"aa", "bbbb", "cccccc"}
	var wantBytes int64
	for i, c := range contents {
		orig := filepath.Join(root, "m"+string(rune('0'+i))+".jpg")
		writeMedia(t, orig, c)
		if _, err := MoveToTrash(orig, trashDir); err != nil {
			t.Fatalf("MoveToTrash: %v", err)
		}
		wantBytes += int64(len(c))
	}

	// Sanity: 3 media + 3 sidecars are staged.
	if pre, _ := os.ReadDir(trashDir); len(pre) != 6 {
		t.Fatalf("staged %d trash entries, want 6 (3 media + 3 meta)", len(pre))
	}

	count, bytes, err := EmptyTrash(trashDir)
	if err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	if count != len(contents) {
		t.Errorf("EmptyTrash count = %d, want %d (non-meta entries)", count, len(contents))
	}
	if bytes != wantBytes {
		t.Errorf("EmptyTrash bytes = %d, want %d", bytes, wantBytes)
	}
	if remaining, _ := os.ReadDir(trashDir); len(remaining) != 0 {
		t.Errorf("trash dir has %d entries after EmptyTrash, want 0", len(remaining))
	}
}

// TestEmptyTrashEmptyDir covers the no-op inputs: an empty string and a
// nonexistent directory both report zero with no error.
func TestEmptyTrashEmptyDir(t *testing.T) {
	if c, b, err := EmptyTrash(""); c != 0 || b != 0 || err != nil {
		t.Errorf("EmptyTrash(\"\") = (%d,%d,%v), want (0,0,nil)", c, b, err)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if c, b, err := EmptyTrash(missing); c != 0 || b != 0 || err != nil {
		t.Errorf("EmptyTrash(missing) = (%d,%d,%v), want (0,0,nil)", c, b, err)
	}
}

// TestListTrash asserts the listing returns one Entry per trashed media file —
// skipping sidecars and non-media junk — and is sorted newest-first (reverse
// lexical on the timestamp-prefixed basename).
func TestListTrash(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	trashDir := filepath.Join(tmp, ".photo-viewer-trash")

	const nMedia = 3
	for i := range nMedia {
		orig := filepath.Join(root, "v"+string(rune('0'+i))+".jpg")
		writeMedia(t, orig, "data")
		if _, err := MoveToTrash(orig, trashDir); err != nil {
			t.Fatalf("MoveToTrash: %v", err)
		}
	}
	// A non-media file dropped straight into the trash dir must be ignored.
	writeMedia(t, filepath.Join(trashDir, "20240101T000000-deadbeef-notes.txt"), "junk")

	list := ListTrash(trashDir)
	if len(list) != nMedia {
		t.Fatalf("ListTrash returned %d entries, want %d (media only)", len(list), nMedia)
	}
	for _, e := range list {
		if e.Type == scan.TypeUnknown {
			t.Errorf("ListTrash returned an unknown-type entry: %s", e.Path)
		}
		if filepath.Dir(e.Path) != trashDir {
			t.Errorf("entry path %s not under trashDir", e.Path)
		}
		if e.ThumbID != ThumbIDFor(e.Path) {
			t.Errorf("entry ThumbID = %q, want %q", e.ThumbID, ThumbIDFor(e.Path))
		}
	}
	// Newest-first: basenames are in strictly descending order.
	for i := 1; i < len(list); i++ {
		if filepath.Base(list[i-1].Path) <= filepath.Base(list[i].Path) {
			t.Errorf("ListTrash not sorted newest-first at %d: %q then %q",
				i, filepath.Base(list[i-1].Path), filepath.Base(list[i].Path))
		}
	}
}

// TestListTrashEmpty covers the no-op inputs.
func TestListTrashEmpty(t *testing.T) {
	if got := ListTrash(""); got != nil {
		t.Errorf("ListTrash(\"\") = %v, want nil", got)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if got := ListTrash(missing); got != nil {
		t.Errorf("ListTrash(missing) = %v, want nil", got)
	}
}
