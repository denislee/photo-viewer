package video

import (
	"os"
	"testing"
	"time"
)

// TestRenderCropAssertion is a manual repro harness for the libmpv
// mp_image_crop abort seen in the field (`x1 <= img->w` during SW render at
// w=3514, h=2570). Point PV_CROP_VIDEO at a video and it drives the real SW
// render path across several target sizes; if mpv aborts, the test binary
// dies with the assertion, otherwise it passes.
//
// Synthetic clips (plain crop, rotation, anamorphic SAR, heavy upscale) did
// NOT reproduce it headlessly — the trigger needs the actual offending file
// and/or the real GPU context — so run this against the real file that
// crashed (e.g. PV_CROP_VIDEO=/path/to/clip.mp4) to confirm a fix locally.
// Gated on PV_CROP_VIDEO so it never runs in normal `go test ./...`.
func TestRenderCropAssertion(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file to run the crop-assertion repro")
	}

	p, err := New(func() {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	if err := p.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Render aggressively from t=0 (no settle wait) to catch the window where
	// mpv's render params can lag the decoded frame, and sweep several target
	// sizes including the exact field size. The crash, if any, fires inside
	// Render here.
	sizes := [][2]int{{3514, 2570}, {3513, 2569}, {4114, 2570}, {1, 1}, {2570, 3514}}
	got := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range sizes {
			if _, ok := p.Render(s[0], s[1]); ok {
				got = true
			}
		}
		if got {
			// keep hammering a bit past first frame to exercise re-renders
			for range 200 {
				p.Render(3514, 2570)
				p.Render(3513, 2569)
			}
			t.Log("rendered all sizes without aborting")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no frame within deadline (decode failed?) — inconclusive")
}
