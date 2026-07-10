package video

import (
	"os"
	"sync"
	"testing"
)

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
