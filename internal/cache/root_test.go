package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocateWithoutRootWritesNothing covers S6-02: pv-export-favorites looks
// the db up with Locate(root, false), which must not create anything in the
// library, yet must still find an index the GUI wrote there with --root.
func TestLocateWithoutRootWritesNothing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()

	_, db, err := Locate(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(db) == root {
		t.Fatalf("no in-root db yet, but Locate picked %s", db)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatalf("Locate(root, false) wrote into the library: %v", ents)
	}

	local := filepath.Join(root, ".photo-viewer.db")
	if err := os.WriteFile(local, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, db, _ := Locate(root, false); db != local {
		t.Fatalf("Locate(root, false) = %s, want the existing %s", db, local)
	}
	if _, viaRoot, _ := Locate(root, true); viaRoot != local {
		t.Fatalf("Locate(root, true) = %s, want %s", viaRoot, local)
	}
}
