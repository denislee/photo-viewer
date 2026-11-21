package cache

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// decoderFixture reconciles 24 rows that cover every media type, a spread of
// sizes (including 0), and a spread of durations — among them the
// DurationProbeFailed (-1) sentinel and the 0 "unknown" value — then flags every
// fifth path as a favorite. It returns the index plus the reconciled paths.
func decoderFixture(t *testing.T) (*Index, []string) {
	t.Helper()
	idx, cleanup := loadEmpty(t)
	t.Cleanup(cleanup)

	types := []scan.MediaType{scan.TypePhoto, scan.TypeRAW, scan.TypeVideo}
	durations := []int64{0, 1500, scan.DurationProbeFailed, 90000, 0, 250}
	results := make([]scan.Result, 0, 24)
	paths := make([]string, 0, 24)
	for n := 0; n < 24; n++ {
		typ := types[n%len(types)]
		ext := ".jpg"
		switch typ {
		case scan.TypeRAW:
			ext = ".cr2"
		case scan.TypeVideo:
			ext = ".mp4"
		}
		path := fmt.Sprintf("/lib/2024-05-01/%03d%s", n, ext)
		results = append(results, scan.Result{
			Path:       path,
			Type:       typ,
			Size:       int64(n) * 1237, // includes a legitimate 0-byte row
			ModTime:    time.Unix(1700000000+int64(n)*37, 0).UTC(),
			DurationMs: durations[n%len(durations)],
		})
		paths = append(paths, path)
	}
	idx.ReconcileBatch(results)

	for n := 0; n < 24; n += 5 {
		if err := idx.SetFavorite(paths[n], true); err != nil {
			t.Fatalf("SetFavorite(%q): %v", paths[n], err)
		}
	}
	return idx, paths
}

// TestEntryDecoderMatchesScanEntry proves the bulk decoder (scanEntries, via
// entryDecoder) and the single-row decoder (GetEntry, via getOne) agree
// field-for-field on real rows: same paths in the same order, and identical
// Path/Type/Size/ModTime/ThumbID/DurationMs/Favorite.
func TestEntryDecoderMatchesScanEntry(t *testing.T) {
	idx, paths := decoderFixture(t)

	v := View{Kind: "all", Filter: "All", ShowRAW: true}
	got := idx.ListPage(v, 0, 0)

	want := slices.Clone(paths)
	slices.Sort(want)
	if diff := viewPaths(got); !slices.Equal(diff, want) {
		t.Fatalf("ListPage paths = %v, want %v", diff, want)
	}

	favorites, probeFailures := 0, 0
	for _, e := range got {
		wantEntry, ok := idx.GetEntry(e.Path)
		if !ok {
			t.Fatalf("GetEntry(%q) not found, but ListPage returned it", e.Path)
		}
		if e.Path != wantEntry.Path ||
			e.Type != wantEntry.Type ||
			e.Size != wantEntry.Size ||
			!e.ModTime.Equal(wantEntry.ModTime) ||
			e.ThumbID != wantEntry.ThumbID ||
			e.DurationMs != wantEntry.DurationMs ||
			e.Favorite != wantEntry.Favorite {
			t.Errorf("entry mismatch for %q:\n list %+v\n  get %+v", e.Path, e, wantEntry)
		}
		if e.ThumbID != ThumbIDFor(e.Path) {
			t.Errorf("%q: ThumbID = %q, want %q", e.Path, e.ThumbID, ThumbIDFor(e.Path))
		}
		if e.Favorite {
			favorites++
		}
		if e.DurationMs == scan.DurationProbeFailed {
			probeFailures++
		}
	}

	// Guard the fixture itself: a decode bug that silently zeroed these columns
	// would otherwise leave nothing to compare.
	if favorites != 5 {
		t.Errorf("favorite rows = %d, want 5", favorites)
	}
	if probeFailures != 4 {
		t.Errorf("rows carrying DurationProbeFailed = %d, want 4", probeFailures)
	}
}

// TestScanEntriesSkipsNullRow pins the skip-on-malformed-row behaviour: the
// entries schema declares size nullable, so a NULL size is accepted by SQLite
// but cannot decode into an Entry; the drain must drop that row and keep going
// rather than fail the whole page.
func TestScanEntriesSkipsNullRow(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	t.Cleanup(cleanup)
	seed(t, idx, "/lib", 3)

	const broken = "/lib/2024-05-01/broken.jpg"
	if _, err := idx.db.Exec(
		"INSERT INTO entries (path, type, size, mtime, thumb_id, favorite, duration_ms) VALUES (?,?,NULL,?,?,0,0)",
		broken, int(scan.TypePhoto), time.Unix(1700000000, 0).Unix(), ThumbIDFor(broken),
	); err != nil {
		t.Skipf("schema does not accept a NULL size (%v); nothing to exercise", err)
	}

	got := idx.ListPage(View{Kind: "all", Filter: "All", ShowRAW: true}, 0, 0)
	if paths := viewPaths(got); slices.Contains(paths, broken) {
		t.Errorf("NULL-size row survived the drain: %v", paths)
	}
	if len(got) != 3 {
		t.Errorf("ListPage returned %d entries, want 3 (the NULL-size row skipped)", len(got))
	}
	if _, ok := idx.GetEntry(broken); ok {
		t.Errorf("GetEntry(%q) decoded a NULL-size row", broken)
	}
}

// BenchmarkListPageAll measures the full-library listing — the hot path behind
// "all" view rendering — over 20k reconciled rows, so the decoder's per-row
// allocations are visible in the reported allocs/op.
func BenchmarkListPageAll(b *testing.B) {
	idx, err := Load(filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatalf("Load: %v", err)
	}
	b.Cleanup(func() { idx.Close() })

	const rows = 20000
	results := make([]scan.Result, 0, rows)
	base := time.Unix(1700000000, 0)
	for n := 0; n < rows; n++ {
		results = append(results, scan.Result{
			Path:    fmt.Sprintf("/lib/2024-05-01/%06d.jpg", n),
			Type:    scan.TypePhoto,
			Size:    int64(1024 + n),
			ModTime: base.Add(time.Duration(n) * time.Second),
		})
	}
	idx.ReconcileBatch(results)

	v := View{Kind: "all", Filter: "All", ShowRAW: true}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if got := idx.ListPage(v, 0, 0); len(got) != rows {
			b.Fatalf("ListPage returned %d entries, want %d", len(got), rows)
		}
	}
}
