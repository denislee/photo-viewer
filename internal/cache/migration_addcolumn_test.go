package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddColumnSwallowsOnlyDuplicate is the C-08 regression guard. The column-add
// migrations in Load used to run as `_, _ = db.Exec("ALTER TABLE ...")`, which
// tolerated the benign "already migrated" case but ALSO hid genuine failures
// (I/O error, malformed DB). A hidden failure leaves the column missing, so every
// later multi-column SELECT errors and the scan helpers return empty — the app
// runs against a mysteriously-empty library with no trail. addColumn is the fix:
// it swallows ONLY the "duplicate column name" case and propagates everything
// else. A read-only-DB Load test can't isolate this branch (the WAL/-shm setup
// on the very first Exec fails first, well before any ALTER), so we exercise the
// helper directly.
func TestAddColumnSwallowsOnlyDuplicate(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	// Sanity: re-adding an existing column really does raise "duplicate column
	// name" from the driver — so the nil below is the benign branch firing, not
	// a no-op that never errored.
	if _, err := idx.db.Exec("ALTER TABLE entries ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0"); err == nil {
		t.Fatal("expected a duplicate-column error re-adding favorite, got nil")
	} else if !strings.Contains(err.Error(), "duplicate column name") {
		t.Fatalf("unexpected error text for duplicate column: %v", err)
	}

	// Benign branch: an already-migrated column is swallowed and returns nil, so
	// an idempotent re-open still succeeds silently.
	if err := addColumn(idx.db, "ALTER TABLE entries ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Errorf("addColumn on a duplicate column = %v, want nil", err)
	}

	// Genuine failures MUST propagate rather than be swallowed. A missing table
	// and a syntactically broken ALTER stand in for the real-world I/O/malformed
	// failures the blanket `_, _ =` used to hide.
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"no such table", "ALTER TABLE definitely_not_here ADD COLUMN x TEXT"},
		{"syntax error", "ALTER TABLE entries ADD COLUMN"},
	} {
		if err := addColumn(idx.db, tc.sql); err == nil {
			t.Errorf("addColumn(%s) = nil, want a propagated error", tc.name)
		} else if strings.Contains(err.Error(), "duplicate column name") {
			t.Errorf("addColumn(%s) misclassified a real error as duplicate: %v", tc.name, err)
		}
	}
}

// TestAddColumnPropagatesRealErrorFromLoad closes Load's own database handle and
// confirms addColumn surfaces the resulting failure instead of swallowing it —
// the "return anything else from Load" contract, exercised against the exact db
// handle Load builds.
func TestAddColumnPropagatesRealErrorFromLoad(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	if err := idx.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	// A closed handle is a genuine failure. The old `_, _ =` would have discarded
	// it and marched on with a missing column; addColumn returns it.
	if err := addColumn(idx.db, "ALTER TABLE entries ADD COLUMN whatever TEXT"); err == nil {
		t.Error("addColumn on a closed db = nil, want a propagated error")
	}
}

// TestLoadReopenIsIdempotent pins the successful-migration and already-migrated
// paths the C-08 fix must preserve: a fresh Load runs every column-add, and a
// second Load over the same file re-runs them all against columns that now exist
// — every ALTER hits "duplicate column name", addColumn swallows each, and Load
// still succeeds silently with the seeded data intact.
func TestLoadReopenIsIdempotent(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-reopen-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	seed(t, idx, "/lib", 4)
	if got := idx.Count(); got != 4 {
		t.Fatalf("after seed: Count = %d, want 4", got)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open: every column-add now duplicates an existing column. Load must
	// still return a usable index (silent re-migration) with data preserved.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatalf("re-open Load errored on already-migrated db: %v", err)
	}
	defer idx2.Close()
	if got := idx2.Count(); got != 4 {
		t.Errorf("after re-open: Count = %d, want 4", got)
	}
}
