package video

import (
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAsyncSlotDiscipline drives the two-slot handoff with a Go writer
// standing in for the GL render, under -race. Each frame fills its slot with
// one byte value; the UI side checks that the frame it holds is never
// written (it stays uniform and unchanged until the next take), whatever the
// writer does meanwhile, including resizes.
func TestAsyncSlotDiscipline(t *testing.T) {
	a := newAsyncRender()
	sizes := [][2]int{{64, 36}, {80, 45}, {64, 36}}
	a.take(sizes[0][0], sizes[0][1]) // the UI asks for a size first
	var stop atomic.Bool
	var frames atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := byte(1); !stop.Load(); n++ {
			slot, buf, _, _, ok := a.beginWrite()
			if !ok {
				continue
			}
			for i := range buf.Pix {
				if i%4 != 3 {
					buf.Pix[i] = n
				}
			}
			a.endWrite(slot)
			frames.Add(1)
		}
	})
	uniform := func(pix []byte) (byte, bool) {
		v := pix[0]
		for i, b := range pix {
			if i%4 != 3 && b != v {
				return 0, false
			}
		}
		return v, true
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		sz := sizes[(i/200)%len(sizes)]
		img, ok := a.take(sz[0], sz[1])
		if !ok {
			continue
		}
		v, ok := uniform(img.Pix)
		if !ok {
			t.Fatal("handed-out frame is torn")
		}
		time.Sleep(20 * time.Microsecond) // the writer keeps going meanwhile
		if v2, ok := uniform(img.Pix); !ok || v2 != v {
			t.Fatal("handed-out frame was written while shown")
		}
	}
	stop.Store(true)
	wg.Wait()
	if frames.Load() < 10 {
		t.Fatalf("writer produced only %d frames", frames.Load())
	}
}

// TestAsyncBeginWriteNeedsSize: no render before the UI has asked for one.
func TestAsyncBeginWriteNeedsSize(t *testing.T) {
	a := newAsyncRender()
	if _, _, _, _, ok := a.beginWrite(); ok {
		t.Fatal("beginWrite succeeded with no size requested")
	}
	if _, ok := a.take(8, 8); ok {
		t.Fatal("take returned a frame before any was rendered")
	}
	select {
	case <-a.wake:
	default:
		t.Fatal("first take (a size change) did not request a render")
	}
}

// TestAsyncPlayerRace is the -race stress test for the PV_VIDEO_ASYNC path,
// modelled on player_race_test.go: the UI-side Render/NeedsRedraw loop, a
// Load/Stop toggler and a final Close all race the render goroutine. Needs a
// real video (PV_CROP_VIDEO) and a GL context; skipped otherwise.
func TestAsyncPlayerRace(t *testing.T) {
	path := os.Getenv("PV_CROP_VIDEO")
	if path == "" {
		t.Skip("set PV_CROP_VIDEO to a video file to run the async render race test")
	}
	t.Setenv("PV_VIDEO_ASYNC", "1")
	p, err := New(func() {})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.async == nil {
		p.Close()
		t.Skip("no GL render path here; the async renderer is GL-only")
	}
	if err := p.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	var stop atomic.Bool
	var got atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		sizes := [][2]int{{640, 360}, {1280, 720}, {320, 240}}
		for i := 0; !stop.Load(); i++ {
			sz := sizes[(i/50)%len(sizes)]
			_ = p.NeedsRedraw()
			if img, ok := p.Render(sz[0], sz[1]); ok {
				_ = img.Pix[len(img.Pix)-1] // touch the frame like an upload would
				got.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	})
	wg.Go(func() {
		for i := 0; !stop.Load(); i++ {
			time.Sleep(40 * time.Millisecond)
			if i%2 == 0 {
				p.Stop()
			} else {
				_ = p.Load(path)
			}
		}
	})
	time.Sleep(1500 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	p.Close()
	if got.Load() == 0 {
		t.Fatal("async renderer never delivered a frame")
	}
}

// TestRenderTimings measures what Render costs the calling (UI) goroutine on
// the default synchronous GL path and on PV_VIDEO_ASYNC, at two window sizes,
// counting only calls that delivered a new frame. Set PV_BENCH_VIDEO to a
// (4K) clip to run it; results are logged, not asserted.
func TestRenderTimings(t *testing.T) {
	path := os.Getenv("PV_BENCH_VIDEO")
	if path == "" {
		t.Skip("set PV_BENCH_VIDEO to a video file to measure Render timings")
	}
	for _, mode := range []string{"sync", "async"} {
		for _, sz := range [][2]int{{1920, 1080}, {2560, 1440}} {
			t.Run(fmt.Sprintf("%s-%dx%d", mode, sz[0], sz[1]), func(t *testing.T) {
				if mode == "async" {
					t.Setenv("PV_VIDEO_ASYNC", "1")
				} else {
					t.Setenv("PV_VIDEO_ASYNC", "0")
				}
				p, err := New(func() {})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				defer p.Close()
				if p.gl == nil {
					t.Skip("no GL render path")
				}
				if err := p.Load(path); err != nil {
					t.Fatalf("Load: %v", err)
				}
				// Warm up: let the file open and the first frames decode.
				warm := time.Now().Add(1500 * time.Millisecond)
				for time.Now().Before(warm) {
					p.Render(sz[0], sz[1])
					time.Sleep(16 * time.Millisecond)
				}
				var d []time.Duration
				end := time.Now().Add(4 * time.Second)
				for time.Now().Before(end) {
					if p.NeedsRedraw() {
						t0 := time.Now()
						if _, ok := p.Render(sz[0], sz[1]); ok {
							d = append(d, time.Since(t0))
						}
					}
					time.Sleep(16600 * time.Microsecond) // a 60 Hz frame loop
				}
				if len(d) == 0 {
					t.Fatal("no frames rendered")
				}
				slices.Sort(d)
				q := func(f float64) time.Duration { return d[min(int(f*float64(len(d))), len(d)-1)] }
				t.Logf("%s %dx%d: frames=%d p50=%v p90=%v max=%v", mode, sz[0], sz[1], len(d),
					q(0.5).Round(100*time.Nanosecond), q(0.9).Round(100*time.Nanosecond), d[len(d)-1].Round(100*time.Nanosecond))
			})
		}
	}
}
