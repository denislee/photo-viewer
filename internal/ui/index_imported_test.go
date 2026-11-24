package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIndexImportedScansOutboxInsideLibrary pins I-03 step 4: after an import
// files photos into an outbox under the library root, IndexImported indexes
// them without a manual rescan; an outbox outside the library is ignored.
func TestIndexImportedScansOutboxInsideLibrary(t *testing.T) {
	root := t.TempDir()
	outbox := filepath.Join(root, "Imported", "2024-03-05")
	outside := t.TempDir()
	for _, p := range []string{filepath.Join(outbox, "a.jpg"), filepath.Join(outside, "b.jpg")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestController(t, root, filepath.Join(t.TempDir(), "cache"))

	c.IndexImported(outside)
	c.IndexImported(filepath.Join(root, "Imported"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := c.index.GetEntry(filepath.Join(outbox, "a.jpg")); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("imported file was not indexed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitRefreshSettled(t, c)
	if _, ok := c.index.GetEntry(filepath.Join(outside, "b.jpg")); ok {
		t.Error("a file outside the library root was indexed")
	}
}
