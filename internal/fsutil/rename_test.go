package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func gone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s should be gone, stat err = %v", p, err)
	}
}

// Both the public entry point (renameat2 on Linux) and the portable fallback
// (link claim / checked rename) must move onto a free name and refuse — with
// both files intact — to replace an existing one.
func TestRenameNoReplace(t *testing.T) {
	impls := map[string]func(src, dst string) error{
		"RenameNoReplace":     RenameNoReplace,
		"linkOrCheckedRename": linkOrCheckedRename,
	}
	for name, rename := range impls {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.jpg")
			dst := filepath.Join(dir, "dst.jpg")

			write(t, src, "source")
			write(t, dst, "occupant")
			err := rename(src, dst)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("rename onto existing file: err = %v, want fs.ErrExist", err)
			}
			if got := read(t, dst); got != "occupant" {
				t.Errorf("occupant clobbered: %q", got)
			}
			if got := read(t, src); got != "source" {
				t.Errorf("source changed: %q", got)
			}

			if err := os.Remove(dst); err != nil {
				t.Fatal(err)
			}
			if err := rename(src, dst); err != nil {
				t.Fatalf("rename onto free name: %v", err)
			}
			if got := read(t, dst); got != "source" {
				t.Errorf("dst content = %q, want source", got)
			}
			gone(t, src)

			// Directories can't be hard-linked; the fallback must still move
			// them (trash can hold directories) and still refuse to replace.
			srcDir := filepath.Join(dir, "album")
			if err := os.Mkdir(srcDir, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(srcDir, "a.jpg"), "a")
			if err := rename(srcDir, dst); !errors.Is(err, fs.ErrExist) {
				t.Errorf("dir onto existing file: err = %v, want fs.ErrExist", err)
			}
			dstDir := filepath.Join(dir, "album-moved")
			if err := rename(srcDir, dstDir); err != nil {
				t.Fatalf("dir rename: %v", err)
			}
			if got := read(t, filepath.Join(dstDir, "a.jpg")); got != "a" {
				t.Errorf("moved dir content = %q", got)
			}
			gone(t, srcDir)
		})
	}
}

func TestSuffixName(t *testing.T) {
	for _, tc := range []struct {
		base string
		n    int
		want string
	}{
		{"IMG_1.jpg", 0, "IMG_1.jpg"},
		{"IMG_1.jpg", 2, "IMG_1_2.jpg"},
		{"clip", 1, "clip_1"},
		{"archive.tar.gz", 3, "archive.tar_3.gz"},
	} {
		if got := SuffixName("/d", tc.base, tc.n); got != filepath.Join("/d", tc.want) {
			t.Errorf("SuffixName(%q, %d) = %q, want %q", tc.base, tc.n, got, tc.want)
		}
	}
}

func TestRenameUnique(t *testing.T) {
	dir := t.TempDir()
	cand := func(n int) string { return SuffixName(dir, "IMG.jpg", n) }

	write(t, filepath.Join(dir, "IMG.jpg"), "first")
	write(t, filepath.Join(dir, "IMG_1.jpg"), "second")
	src := filepath.Join(t.TempDir(), "IMG.jpg")
	write(t, src, "third")

	got, err := RenameUnique(src, cand)
	if err != nil {
		t.Fatalf("RenameUnique: %v", err)
	}
	if want := filepath.Join(dir, "IMG_2.jpg"); got != want {
		t.Errorf("landed at %q, want %q", got, want)
	}
	for name, want := range map[string]string{"IMG.jpg": "first", "IMG_1.jpg": "second", "IMG_2.jpg": "third"} {
		if c := read(t, filepath.Join(dir, name)); c != want {
			t.Errorf("%s = %q, want %q", name, c, want)
		}
	}

	// Non-collision errors surface immediately instead of walking suffixes.
	if _, err := RenameUnique(filepath.Join(dir, "missing.jpg"), cand); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing source: err = %v, want fs.ErrNotExist", err)
	}
}
