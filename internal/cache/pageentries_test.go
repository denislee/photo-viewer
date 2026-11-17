package cache

import (
	"testing"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestForEachEntryPausedConsumerAllowsCheckpoint pins U4-08: the pager must not
// hold a read snapshot while visit runs, or a slow consumer blocks WAL
// checkpoints and the WAL grows without bound.
func TestForEachEntryPausedConsumerAllowsCheckpoint(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 1500)

	paused := make(chan struct{})
	release := make(chan struct{})
	done := make(chan int)

	go func() {
		visits := 0
		idx.ForEachEntry(func(Entry) bool {
			visits++
			if visits == 1 {
				close(paused)
				<-release
			}
			return true
		})
		done <- visits
	}()

	<-paused
	// Move the WAL past the reader snapshot: this write would block forever if
	// ForEachEntry still held a read transaction open.
	if err := idx.SetFavorite("/lib/000001.jpg", true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	var busy, logFrames, ckpt int
	if err := idx.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckpt); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	if busy != 0 {
		t.Fatalf("a paused ForEachEntry consumer pinned a read snapshot (busy=%d)", busy)
	}

	close(release)
	if got := <-done; got != 1500 {
		t.Fatalf("visits = %d, want 1500", got)
	}
}

// TestListByTypePausedConsumerAllowsCheckpoint is the ListByType twin of the
// test above; seed creates TypePhoto rows.
func TestListByTypePausedConsumerAllowsCheckpoint(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 1500)

	paused := make(chan struct{})
	release := make(chan struct{})
	done := make(chan int)

	go func() {
		visits := 0
		idx.ListByType(scan.TypePhoto, func(Entry) bool {
			visits++
			if visits == 1 {
				close(paused)
				<-release
			}
			return true
		})
		done <- visits
	}()

	<-paused
	if err := idx.SetFavorite("/lib/000001.jpg", true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	var busy, logFrames, ckpt int
	if err := idx.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckpt); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	if busy != 0 {
		t.Fatalf("a paused ListByType consumer pinned a read snapshot (busy=%d)", busy)
	}

	close(release)
	if got := <-done; got != 1500 {
		t.Fatalf("visits = %d, want 1500", got)
	}
}
