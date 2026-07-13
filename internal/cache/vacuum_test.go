package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVacuumShrinksAfterClear verifies C-05: Clear moves pages to the freelist
// but leaves them allocated to the file, so the db keeps its high-water mark;
// Vacuum reclaims that freelist space and the file shrinks. The test inflates
// the file with entries plus several 128-float (512-byte) face embeddings and
// their per-face clusters, wipes it with Clear, then asserts Vacuum shrinks the
// on-disk .db below both the pre-Clear size and the post-Clear (freelist-still-
// allocated) size.
func TestVacuumShrinksAfterClear(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	idx, err := Load(dbPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer idx.Close()

	root := "/lib"
	const nFiles = 4000
	seed(t, idx, root, nFiles)

	// Inflate with face + cluster rows: 4 faces/file, each a 128-float (512-byte)
	// embedding, each seeding its own cluster centroid — enough bytes that the
	// freelist left behind by Clear dwarfs any page-count noise.
	emb := make([]float32, 128)
	for k := range emb {
		emb[k] = float32(k) * 0.01
	}
	for i := range nFiles {
		p := filepath.Join(root, padded(i)+".jpg")
		ops := make([]FaceOp, 4)
		for f := range ops {
			ops[f] = FaceOp{
				Face:               Face{Path: p, ThumbMtime: 1, BBox: [4]int{1, 2, 3, 4}, Embedding: emb},
				NewClusterCentroid: emb,
			}
		}
		if _, err := idx.WriteFacesForPath(p, ops); err != nil {
			t.Fatalf("WriteFacesForPath: %v", err)
		}
	}

	// Fold the WAL into the main .db file (and truncate the WAL) so os.Stat on
	// dbPath reflects the true allocated size at each checkpoint. Without this
	// the freshly written pages could still be sitting in the -wal sidecar.
	checkpoint := func() {
		if _, err := idx.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
	}

	checkpoint()
	before := dbSize(t, dbPath)

	if err := idx.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	checkpoint()
	afterClear := dbSize(t, dbPath)

	// Clear only frees pages to the freelist; the file must not have shrunk yet.
	if afterClear < before {
		t.Fatalf("Clear unexpectedly shrank the db (before=%d afterClear=%d); "+
			"the test can no longer prove Vacuum is what reclaims space", before, afterClear)
	}

	if err := idx.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	checkpoint()
	afterVacuum := dbSize(t, dbPath)

	if afterVacuum >= afterClear {
		t.Errorf("Vacuum did not reclaim freelist space: afterClear=%d afterVacuum=%d",
			afterClear, afterVacuum)
	}
	if afterVacuum >= before {
		t.Errorf("Vacuum did not shrink the db below its pre-Clear size: before=%d afterVacuum=%d",
			before, afterVacuum)
	}

	// The handle must stay live after a Vacuum, just like after Clear.
	seed(t, idx, root, 3)
	if got := idx.Count(); got != 3 {
		t.Fatalf("after re-seed on vacuumed handle: Count = %d, want 3", got)
	}
}

// dbSize returns the size in bytes of the SQLite main database file.
func dbSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
