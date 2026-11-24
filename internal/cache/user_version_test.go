package cache

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestLoadFailsWhenUserVersionUnreadable is the C6-B2 regression test: a
// failed PRAGMA user_version read must fail Load instead of being taken as
// version 0, which would re-run the destructive migrations (v1 NULLs every
// quick_hash, v4 deletes the faces) against an up-to-date database.
func TestLoadFailsWhenUserVersionUnreadable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	idx.ReconcileBatch([]scan.Result{{Path: "/lib/a.jpg", Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1, 0)}})
	if _, err := idx.db.Exec("UPDATE entries SET quick_hash = 'q' WHERE path = '/lib/a.jpg'"); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	orig := readUserVersion
	readUserVersion = func(*sql.DB) (int, error) { return 0, errors.New("disk I/O error") }
	idx2, err := Load(dbPath)
	readUserVersion = orig
	if err == nil {
		_ = idx2.Close()
		t.Error("Load succeeded although user_version could not be read")
	}

	idx3, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx3.Close()
	var q sql.NullString
	if err := idx3.db.QueryRow("SELECT quick_hash FROM entries WHERE path = '/lib/a.jpg'").Scan(&q); err != nil {
		t.Fatal(err)
	}
	if q.String != "q" {
		t.Errorf("quick_hash = %v after the failed load, want 'q' (v1 migration re-ran)", q)
	}
}
