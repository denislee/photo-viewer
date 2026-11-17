package main

import (
	"context"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// stubFacePipeline replaces newFacePipeline with a counting stub for the
// duration of the test, restoring the original afterwards.
func stubFacePipeline(t *testing.T) *int {
	t.Helper()
	var calls int
	orig := newFacePipeline
	newFacePipeline = func(idx *cache.Index) *face.Pipeline {
		calls++
		return &face.Pipeline{}
	}
	t.Cleanup(func() { newFacePipeline = orig })
	return &calls
}

func TestNoFacesNeverProbes(t *testing.T) {
	calls := stubFacePipeline(t)

	p := startFaces(context.Background(), nil, true)

	if p != nil {
		t.Fatalf("startFaces(-no-faces) = %v, want nil", p)
	}
	if *calls != 0 {
		t.Fatalf("newFacePipeline called %d times with -no-faces, want 0", *calls)
	}
}

func TestFacesProbeWithoutNoFaces(t *testing.T) {
	calls := stubFacePipeline(t)

	p := startFaces(context.Background(), nil, false)

	if p == nil {
		t.Fatal("startFaces(no -no-faces) == nil, want non-nil pipeline")
	}
	if *calls != 1 {
		t.Fatalf("newFacePipeline called %d times, want 1", *calls)
	}
	if p.Enabled() {
		t.Fatal("zero Pipeline.Enabled() = true, want false")
	}
	// A zero Pipeline was never started; Stop must not panic.
	p.Stop()
}
