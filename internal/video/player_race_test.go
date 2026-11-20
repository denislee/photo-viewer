package video

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestConcurrentCommandsAndClose covers V5-03: TogglePause, ToggleMute and
// SeekRelative no longer hold p.mu, so they must be safe against each other,
// against Render, and against a Close that destroys the handle underneath
// them. cmdMu keeps the handle alive until they return, and Close's write side
// waits them out. Run with -race and PV_CROP_VIDEO set to a real video file.
func TestConcurrentCommandsAndClose(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file to run the command/Close race test")
	}

	p, err := New(func() {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				delta := 1.0
				if i%2 == 1 {
					delta = -1
				}
				p.SeekRelative(delta)
				p.TogglePause()
				p.ToggleMute()
				p.Render(320, 240)
				p.SetVisible(i%2 == 0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		p.Close()
	}()
	wg.Wait()
}

// TestNativeSizeNilPlayer checks the nil-receiver contract of the V5-02 API:
// a nil *Player reports no known native size and accepts SetVisible, so the
// viewer can call both before the preload goroutine has built the player.
func TestNativeSizeNilPlayer(t *testing.T) {
	var p *Player
	if w, h := p.NativeSize(); w != 0 || h != 0 {
		t.Fatalf("NativeSize on nil player = %d, %d; want 0, 0", w, h)
	}
	p.SetVisible(true)
	p.SetVisible(false)
}

// TestCloseRenderRace verifies that concurrent Close and Render calls do not
// race on p.ctx. Before the fix, Close freed p.ctx without holding mu while
// Render read p.ctx after a pre-lock closed check that could have already
// passed. Run with -race and PV_CROP_VIDEO set to a real video file.
//
// Without a real file the mpv handle hangs in mpv_terminate_destroy in this
// headless environment, matching the gating strategy used by
// TestRenderCropAssertion.
func TestCloseRenderRace(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file to run the Close/Render race test")
	}

	p, err := New(func() {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			p.Render(320, 240)
		}
	}()
	go func() {
		defer wg.Done()
		p.Close()
	}()
	wg.Wait()
}

// TestCloseDuringFileLoaded closes the player at staggered delays after Load
// so Close races the FILE_LOADED event (drainEvents → applyRotation, which
// calls into p.h). Before the fix Close destroyed the handle without first
// waiting for drainEvents, so applyRotation could run on a handle being
// torn down. Run with -race and PV_CROP_VIDEO set to a real video file.
func TestCloseDuringFileLoaded(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file to run the Close/FILE_LOADED race test")
	}

	for i := range 30 {
		p, err := New(func() {})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := p.Load(path); err != nil {
			t.Fatalf("Load: %v", err)
		}
		time.Sleep(time.Duration(i) * time.Millisecond)
		p.Close()
	}
}
