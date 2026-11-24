package ui

import (
	"image"
	"os"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/video"
)

// TestPlayVideoPaintSizeMatchesFrame is the G6-B2 regression test: the size
// playVideo returns for painting must be the delivered frame's own size. On
// the PV_VIDEO_ASYNC path a resize hands back the previous size's frame until
// the re-render lands; returning the requested size then painted that frame
// at the wrong scale. Gated on PV_CROP_VIDEO (set by `make test-video`) since
// it drives a real libmpv player.
func TestPlayVideoPaintSizeMatchesFrame(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file (make test-video does)")
	}
	t.Setenv("PV_VIDEO_ASYNC", "1")
	p, err := video.New(func() {})
	if err != nil {
		t.Skipf("video.New: %v", err)
	}
	defer p.Close()
	v := &Viewer{}
	v.player.Store(p)
	e := cache.Entry{Path: path, Type: scan.TypeVideo}

	// Sizes below the clip's native size, so the window-size render is used
	// and every resize changes the requested size.
	sizes := []image.Point{{320, 180}, {400, 225}, {280, 158}, {360, 203}}
	rendered := 0
	deadline := time.Now().Add(8 * time.Second)
	for i := 0; time.Now().Before(deadline) && rendered < 60; i++ {
		sz := sizes[(i/3)%len(sizes)]
		op, got, ok := v.playVideo(e, sz.X, sz.Y)
		if ok {
			rendered++
			if got != op.Size() {
				t.Fatalf("paint size %v != frame size %v (requested %v)", got, op.Size(), sz)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rendered == 0 {
		t.Skip("no frame rendered (no GL/SW render path in this environment)")
	}
}
