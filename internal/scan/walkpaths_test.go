package scan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// walkpathsFixture builds the SD-card-like tree these tests walk: one real
// media file at the root, one in a subdirectory, and the macOS litter real
// cards carry — AppleDouble forks (._*), .Trashes, .Spotlight-V100 — plus
// plain non-media files (notes.txt, z.zip).
func walkpathsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"a.jpg",
		"._a.jpg",
		"notes.txt",
		".Trashes/x.jpg",
		filepath.Join("sub", "b.mp4"),
		filepath.Join("sub", "._b.mp4"),
		filepath.Join(".Spotlight-V100", "c.jpg"),
		"z.zip",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func relPaths(t *testing.T, root string, paths []string) []string {
	t.Helper()
	rel := make([]string, len(paths))
	for i, p := range paths {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("rel %s: %v", p, err)
		}
		rel[i] = r
	}
	slices.Sort(rel)
	return rel
}

func TestWalkMediaPaths(t *testing.T) {
	root := walkpathsFixture(t)
	var visited []string
	types := map[string]MediaType{}
	err := WalkMediaPaths(context.Background(), root, func(p string, mt MediaType) error {
		visited = append(visited, p)
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		types[rel] = mt
		return nil
	})
	if err != nil {
		t.Fatalf("WalkMediaPaths: %v", err)
	}
	want := []string{"a.jpg", filepath.Join("sub", "b.mp4")}
	if got := relPaths(t, root, visited); !slices.Equal(got, want) {
		t.Errorf("WalkMediaPaths visited %v, want %v", got, want)
	}
	if got := types["a.jpg"]; got != TypePhoto {
		t.Errorf("a.jpg type = %v, want %v", got, TypePhoto)
	}
	if got := types[filepath.Join("sub", "b.mp4")]; got != TypeVideo {
		t.Errorf("sub/b.mp4 type = %v, want %v", got, TypeVideo)
	}
}

func TestWalkMediaPathsSkipAll(t *testing.T) {
	root := walkpathsFixture(t)
	calls := 0
	err := WalkMediaPaths(context.Background(), root, func(string, MediaType) error {
		calls++
		return fs.SkipAll
	})
	if err != nil {
		t.Fatalf("WalkMediaPaths with fs.SkipAll: %v", err)
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want 1", calls)
	}
}

func TestWalkVisibleFiles(t *testing.T) {
	root := walkpathsFixture(t)
	var visited []string
	err := WalkVisibleFiles(context.Background(), root, func(p string) error {
		visited = append(visited, p)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkVisibleFiles: %v", err)
	}
	want := []string{"a.jpg", "notes.txt", filepath.Join("sub", "b.mp4"), "z.zip"}
	if got := relPaths(t, root, visited); !slices.Equal(got, want) {
		t.Errorf("WalkVisibleFiles visited %v, want %v", got, want)
	}
}

func TestWalkMediaPathsCancelled(t *testing.T) {
	root := walkpathsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WalkMediaPaths(ctx, root, func(string, MediaType) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WalkMediaPaths on a cancelled ctx = %v, want context.Canceled", err)
	}
}
