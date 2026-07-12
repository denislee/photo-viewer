package cache

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// seedMixed inserts a small interleaved set of photo/raw/video entries so that
// a correct WHERE-type filter (not slice position) is what selects a type.
func seedMixed(t *testing.T, idx *Index, root string) {
	t.Helper()
	results := []scan.Result{
		{Path: root + "/a_photo.jpg", Type: scan.TypePhoto},
		{Path: root + "/b_video.mp4", Type: scan.TypeVideo},
		{Path: root + "/c_raw.cr2", Type: scan.TypeRAW},
		{Path: root + "/d_video.mov", Type: scan.TypeVideo},
		{Path: root + "/e_photo.png", Type: scan.TypePhoto},
	}
	for i := range results {
		results[i].Size = int64(100 + i)
		results[i].ModTime = time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC)
	}
	idx.ReconcileBatch(results)
}

// TestListByTypeStreamsOnlyMatching verifies ListByType pushes the type filter
// into SQL and yields exactly the rows of that type, in path order — the
// organize scan relies on this to enumerate videos without materializing the
// whole index (U-06).
func TestListByTypeStreamsOnlyMatching(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seedMixed(t, idx, root)

	var got []string
	idx.ListByType(scan.TypeVideo, func(e Entry) bool {
		if e.Type != scan.TypeVideo {
			t.Errorf("ListByType(video) yielded non-video type %d for %s", e.Type, e.Path)
		}
		got = append(got, filepath.Base(e.Path))
		return true
	})

	want := []string{"b_video.mp4", "d_video.mov"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListByType(video) = %v, want %v", got, want)
	}
}

// TestCountByTypeMatchesListByType verifies CountByType agrees with the number
// of rows ListByType streams for the same type.
func TestCountByTypeMatchesListByType(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seedMixed(t, idx, root)

	if got := idx.CountByType(scan.TypeVideo); got != 2 {
		t.Errorf("CountByType(video) = %d, want 2", got)
	}
	if got := idx.CountByType(scan.TypePhoto); got != 2 {
		t.Errorf("CountByType(photo) = %d, want 2", got)
	}
	if got := idx.CountByType(scan.TypeRAW); got != 1 {
		t.Errorf("CountByType(raw) = %d, want 1", got)
	}
}

// TestListByTypeEarlyStop verifies returning false from visit halts iteration —
// the cancel path in the organize producer depends on this.
func TestListByTypeEarlyStop(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seedMixed(t, idx, root)

	count := 0
	idx.ListByType(scan.TypeVideo, func(Entry) bool {
		count++
		return false // stop after the first row
	})
	if count != 1 {
		t.Errorf("visit called %d times after returning false, want 1", count)
	}
}
