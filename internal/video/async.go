package video

import (
	"image"
	"os"
	"runtime"
	"strings"
	"sync"
)

// Asynchronous GL rendering (V4-01, prototype). On the default GL path every
// new video frame costs the UI goroutine an eglMakeCurrent pair, the mpv
// render and a synchronous glReadPixels (p90 5–6 ms at 1080p/1440p). With
// PV_VIDEO_ASYNC=1 a dedicated, OS-thread-locked goroutine does that work
// instead: it is woken by mpv's render-update callback (or a size change),
// renders into one of two frame slots, and invalidates the window once the
// frame is finished. Render on the UI goroutine only hands out the newest
// finished slot. The software path is unchanged, and so is the default.
//
// Slot discipline: the slot last returned by Render ("shown") may still be
// on screen, aliased by the viewer's cached paint.ImageOp, so the render
// goroutine only ever writes the other slot. A finished frame the UI has not
// picked up yet may be overwritten by a newer one (a dropped frame, not a
// torn one): while a slot is being written it is never "latest".

// asyncWanted reports whether PV_VIDEO_ASYNC asks for the render goroutine.
func asyncWanted() bool {
	v := strings.TrimSpace(os.Getenv("PV_VIDEO_ASYNC"))
	return v == "1" || strings.EqualFold(v, "true")
}

type asyncRender struct {
	wake chan struct{} // capacity 1: a pending render request
	quit chan struct{}
	done chan struct{} // closed when the loop exits

	mu     sync.Mutex
	w, h   int // size the UI last asked for
	slots  [2]*image.RGBA
	latest int  // newest finished slot, -1 if none
	shown  int  // slot last handed to the UI, -1 if none
	fresh  bool // latest finished after the last Render handed one out
}

func newAsyncRender() *asyncRender {
	return &asyncRender{
		wake:   make(chan struct{}, 1),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		latest: -1,
		shown:  -1,
	}
}

// kick asks the loop for a render; never blocks. Safe from mpv's threads.
func (a *asyncRender) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// take records the size the UI wants and returns the newest finished frame,
// marking it shown. A size change requests a re-render; until it lands the
// previous size's frame is returned.
func (a *asyncRender) take(w, h int) (*image.RGBA, bool) {
	a.mu.Lock()
	resized := a.w != w || a.h != h
	a.w, a.h = w, h
	var img *image.RGBA
	if a.latest >= 0 {
		a.shown = a.latest
		a.fresh = false
		img = a.slots[a.latest]
	}
	a.mu.Unlock()
	if resized {
		a.kick()
	}
	return img, img != nil
}

// hasFresh reports whether a finished frame is waiting for the UI.
func (a *asyncRender) hasFresh() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fresh
}

// beginWrite picks the slot to render into (never the shown one), sized
// w×h, and withdraws it from latest while it is written. ok is false before
// the UI has asked for a size.
func (a *asyncRender) beginWrite() (slot int, buf *image.RGBA, w, h int, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, h = a.w, a.h
	if w <= 0 || h <= 0 {
		return 0, nil, 0, 0, false
	}
	slot = 0
	if a.shown == 0 {
		slot = 1
	}
	if a.latest == slot {
		a.latest = -1
		a.fresh = false
	}
	buf = sizeRGBA(a.slots[slot], w, h)
	a.slots[slot] = buf
	return slot, buf, w, h, true
}

// endWrite publishes a finished slot.
func (a *asyncRender) endWrite(slot int) {
	a.mu.Lock()
	a.latest = slot
	a.fresh = true
	a.mu.Unlock()
}

// reset forgets every frame (playback stopped). The UI's copy of the shown
// frame stays valid; it is simply no longer reused.
func (a *asyncRender) reset() {
	a.mu.Lock()
	a.slots = [2]*image.RGBA{}
	a.latest, a.shown, a.fresh = -1, -1, false
	a.mu.Unlock()
}

// sizeRGBA returns buf resized to w×h, reusing its backing array when it is
// big enough, with the alpha bytes set to 0xff (see ensureBuf).
func sizeRGBA(buf *image.RGBA, w, h int) *image.RGBA {
	if buf != nil && buf.Rect.Dx() == w && buf.Rect.Dy() == h {
		return buf
	}
	need := 4 * w * h
	if buf != nil && cap(buf.Pix) >= need {
		buf = &image.RGBA{Pix: buf.Pix[:need], Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
	} else {
		buf = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for i := 3; i < len(buf.Pix); i += 4 {
		buf.Pix[i] = 0xff
	}
	return buf
}

// renderLoop is the dedicated render goroutine. It is locked to one OS
// thread so the EGL make-current / restore pair in render_gl never touches a
// thread Gio renders on.
func (p *Player) renderLoop(a *asyncRender) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(a.done)
	for {
		select {
		case <-a.wake:
		case <-a.quit:
			return
		}
		if p.renderAsyncFrame(a) && p.invalidate != nil {
			p.invalidate()
		}
	}
}

// renderAsyncFrame renders one frame into a free slot. It holds p.mu like
// the synchronous Render, so Load/Stop/Close never overlap a render.
func (p *Player) renderAsyncFrame(a *asyncRender) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || !p.loaded.Load() || p.ctx == nil || p.gl == nil {
		return false
	}
	slot, buf, w, h, ok := a.beginWrite()
	if !ok {
		return false
	}
	// Clear before rendering, as Render does, so an update callback during
	// the render re-arms a wake instead of being lost.
	p.updatePending.Store(false)
	if p.renderGLInto(buf, w, h) < 0 {
		return false
	}
	a.endWrite(slot)
	return true
}
