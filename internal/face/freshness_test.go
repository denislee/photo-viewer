package face

import (
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestZeroFaceImageNotRedetectedByNextPipeline is the F5-01 end-to-end check
// across a process boundary: an image that yielded zero detections must still
// be remembered as scanned, so the next pv-scan run (a brand-new Pipeline,
// whose freshness mirror starts empty and is loaded from the index) skips it
// instead of re-running detection forever. A newer thumbnail (different mtime)
// must still be re-detected.
func TestZeroFaceImageNotRedetectedByNextPipeline(t *testing.T) {
	idx := testIndex(t)

	const path = "/lib/000001.jpg"
	idx.ReconcileBatch([]scan.Result{{
		Path:    path,
		Type:    scan.TypePhoto,
		Size:    1,
		ModTime: time.Unix(1, 0),
	}})

	job := Job{Entry: cache.Entry{Type: scan.TypePhoto, Path: path}, ThumbMod: 100}

	// This run finds no faces at all.
	a := NewPipeline(idx, nil)
	if changed := a.assignAndPersist(job, nil); changed {
		t.Fatal("zero detections must not create a cluster")
	}
	if got, ok := idx.LoadFaceFreshness()[path]; !ok || got != 100 {
		t.Fatalf("freshness after zero-face write = %v (ok=%v), want %s:100", got, ok, path)
	}

	// The next pipeline (a new process, so an empty in-memory mirror) must see
	// the same run as fresh, and a newer thumbnail as stale.
	b := NewPipeline(idx, nil)
	if !b.hasFreshCached(path, 100) {
		t.Error("next pipeline would re-detect an image that was already scanned with zero faces")
	}
	if b.hasFreshCached(path, 101) {
		t.Error("a newer thumbnail mtime must invalidate the freshness record")
	}
}
