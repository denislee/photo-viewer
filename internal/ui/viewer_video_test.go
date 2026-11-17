package ui

import (
	"errors"
	"image"
	"sync/atomic"
	"testing"
	"time"

	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/video"
)

// TestPlayVideoNeverBuildsPlayer covers G4-01: while the preload goroutine is
// stuck in the (59–86 ms in real life) libmpv constructor, the frame-goroutine
// path must return at once and let Layout fall back to the thumbnail, instead
// of waiting on the constructor's sync.Once.
func TestPlayVideoNeverBuildsPlayer(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	orig := newPlayer
	newPlayer = func(func()) (*video.Player, error) {
		calls.Add(1)
		close(entered)
		<-release
		return nil, errors.New("no libmpv in tests")
	}
	t.Cleanup(func() { newPlayer = orig })

	v := &Viewer{}
	clip := cache.Entry{Path: "/v/clip.mp4", Type: scan.TypeVideo}
	v.Show([]cache.Entry{clip}, 0)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preload goroutine never started building the player")
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := v.playVideo(clip, 640, 360)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("playVideo reported a frame with no player")
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("playVideo blocked on the player constructor")
	}

	close(release)
	// Waits on the Once until the preload goroutine's constructor returns;
	// this call must not construct a second player.
	if p, created := v.ensurePlayer(); p != nil || created {
		t.Fatalf("ensurePlayer = %v, created %v; want nil, false", p, created)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("constructor ran %d times, want 1", n)
	}
	// A failed constructor leaves the viewer on the thumbnail path for good.
	if _, ok := v.playVideo(clip, 640, 360); ok {
		t.Fatal("playVideo reported a frame after the constructor failed")
	}
}

// TestViewerCloseDropsVideoFrame covers G4-06: the last video op aliases the
// player's frame buffer, so Close must forget it or the window-sized RGBA
// stays reachable after Stop released it.
func TestViewerCloseDropsVideoFrame(t *testing.T) {
	v := &Viewer{}
	v.lastVideoOp = paint.NewImageOp(image.NewRGBA(image.Rect(0, 0, 4, 4)))
	v.lastVideoPath = "/v/clip.mp4"
	v.lastVideoSize = image.Pt(4, 4)
	v.Close()
	if v.lastVideoOp != (paint.ImageOp{}) || v.lastVideoPath != "" || v.lastVideoSize != (image.Point{}) {
		t.Fatalf("Close kept the video frame: path=%q size=%v", v.lastVideoPath, v.lastVideoSize)
	}
}
