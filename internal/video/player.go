// Package video embeds libmpv into the Gio UI via mpv's software render API.
//
// We use MPV_RENDER_API_TYPE_SW so mpv composites the current frame into a
// CPU pixel buffer we own, which we then hand to Gio as a paint.ImageOp.
// This avoids needing to share an OpenGL context between mpv and Gio — at the
// cost of a per-frame texture upload from system RAM to GPU. For a photo
// library player that's an acceptable trade.
package video

/*
#cgo pkg-config: mpv
#include <mpv/client.h>
#include <mpv/render.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Go-side callbacks. We pass our cgo.Handle as a uintptr_t through C so
// Go never has to do an unsafe.Pointer(uintptr(...)) round-trip, which
// `go vet` flags as a possible GC-correctness issue.
extern void goMpvRenderUpdate(uintptr_t handle);
extern void goMpvWakeup(uintptr_t handle);

static void render_update_trampoline(void *ctx) {
    goMpvRenderUpdate((uintptr_t)ctx);
}

static void wakeup_trampoline(void *ctx) {
    goMpvWakeup((uintptr_t)ctx);
}

static void set_render_update_cb(mpv_render_context *ctx, uintptr_t user) {
    mpv_render_context_set_update_callback(ctx, render_update_trampoline, (void*)user);
}

static void set_wakeup_cb(mpv_handle *h, uintptr_t user) {
    mpv_set_wakeup_callback(h, wakeup_trampoline, (void*)user);
}

// Build the render-context creation params on the C side; constructing
// arrays of struct mpv_render_param from Go through cgo is awkward and
// error-prone.
static int create_render_ctx_sw(mpv_handle *h, mpv_render_context **ctx) {
    mpv_render_param params[3];
    params[0].type = MPV_RENDER_PARAM_API_TYPE;
    params[0].data = MPV_RENDER_API_TYPE_SW;
    params[1].type = MPV_RENDER_PARAM_INVALID;
    params[1].data = NULL;
    return mpv_render_context_create(ctx, h, params);
}

// render_sw blits the current frame into the pixel buffer pointed at by
// dst, using "rgb0" — 4 bytes per pixel, R G B at offsets 0..2, the 4th
// byte left as garbage. The caller is expected to fix up alpha to 0xff
// (or rely on a black background, since transparent-black ≡ opaque-black
// on screen).
static int render_sw(mpv_render_context *ctx, int w, int h, size_t stride, void *dst) {
    int sz[2] = {w, h};
    char fmt[] = "rgb0";
    size_t st = stride;
    mpv_render_param params[5];
    params[0].type = MPV_RENDER_PARAM_SW_SIZE;
    params[0].data = sz;
    params[1].type = MPV_RENDER_PARAM_SW_FORMAT;
    params[1].data = fmt;
    params[2].type = MPV_RENDER_PARAM_SW_STRIDE;
    params[2].data = &st;
    params[3].type = MPV_RENDER_PARAM_SW_POINTER;
    params[3].data = dst;
    params[4].type = MPV_RENDER_PARAM_INVALID;
    params[4].data = NULL;
    return mpv_render_context_render(ctx, params);
}

static int command_str(mpv_handle *h, const char *s) {
    return mpv_command_string(h, s);
}

static int loadfile(mpv_handle *h, const char *path) {
    const char *cmd[3];
    cmd[0] = "loadfile";
    cmd[1] = path;
    cmd[2] = NULL;
    return mpv_command(h, cmd);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Player is an embedded libmpv instance that renders frames into a reusable
// CPU buffer. Methods are safe to call from the Gio UI goroutine; the
// update callback (which wakes the UI) is fired from mpv's internal
// threads.
type Player struct {
	h   *C.mpv_handle
	ctx *C.mpv_render_context

	handle cgo.Handle

	invalidate func()

	mu      sync.Mutex
	buf     *image.RGBA // reused across frames; reallocated on size change
	closed  atomic.Bool
	loaded  atomic.Bool
	current string

	// updatePending is set by the render update callback and cleared on
	// the next successful render. It exists so callers can poll cheaply
	// for "is there a new frame to draw?" without entering cgo.
	updatePending atomic.Bool
}

// New constructs and initialises a libmpv handle with the SW render API
// attached. invalidate is called from mpv's worker threads whenever a new
// frame is available or an internal state change occurs; the caller should
// wire it to the Gio window's Invalidate so the frame loop wakes and calls
// Render.
func New(invalidate func()) (*Player, error) {
	h := C.mpv_create()
	if h == nil {
		return nil, errors.New("mpv_create failed")
	}

	// Options must be set before mpv_initialize. We disable on-screen text
	// (the host UI provides its own controls), enable looping for the
	// photo-viewer use case, and ask for hardware decoding when available.
	setOpt := func(name, value string) error {
		cn := C.CString(name)
		cv := C.CString(value)
		defer C.free(unsafe.Pointer(cn))
		defer C.free(unsafe.Pointer(cv))
		if rc := C.mpv_set_option_string(h, cn, cv); rc < 0 {
			return fmt.Errorf("mpv set_option %s=%s: %s", name, value, C.GoString(C.mpv_error_string(rc)))
		}
		return nil
	}
	for _, kv := range [][2]string{
		{"vo", "libmpv"},
		{"hwdec", "auto-safe"},
		{"loop-file", "inf"},
		{"keep-open", "always"},
		{"audio-display", "no"},
		{"osc", "no"},
		{"input-default-bindings", "no"},
		{"input-vo-keyboard", "no"},
		{"terminal", "no"},
	} {
		if err := setOpt(kv[0], kv[1]); err != nil {
			C.mpv_terminate_destroy(h)
			return nil, err
		}
	}

	if rc := C.mpv_initialize(h); rc < 0 {
		C.mpv_terminate_destroy(h)
		return nil, fmt.Errorf("mpv_initialize: %s", C.GoString(C.mpv_error_string(rc)))
	}

	p := &Player{h: h, invalidate: invalidate}
	p.handle = cgo.NewHandle(p)
	user := C.uintptr_t(p.handle)

	var ctx *C.mpv_render_context
	if rc := C.create_render_ctx_sw(h, &ctx); rc < 0 {
		p.handle.Delete()
		C.mpv_terminate_destroy(h)
		return nil, fmt.Errorf("mpv_render_context_create: %s", C.GoString(C.mpv_error_string(rc)))
	}
	p.ctx = ctx
	C.set_render_update_cb(ctx, user)
	C.set_wakeup_cb(h, user)

	// Drain events in the background so mpv's queue doesn't fill up.
	go p.drainEvents()

	return p, nil
}

// Load starts playback of path. Any previously-playing file is replaced.
// Safe to call repeatedly as the user navigates between videos.
func (p *Player) Load(path string) error {
	if p == nil || p.closed.Load() {
		return errors.New("player closed")
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if rc := C.loadfile(p.h, cp); rc < 0 {
		return fmt.Errorf("mpv loadfile: %s", C.GoString(C.mpv_error_string(rc)))
	}
	p.current = path
	p.loaded.Store(true)
	p.updatePending.Store(true)
	return nil
}

// Stop halts playback and clears the active file. Used when the viewer
// moves off a video so we don't keep decoding in the background.
func (p *Player) Stop() {
	if p == nil || p.closed.Load() || !p.loaded.Load() {
		return
	}
	cs := C.CString("stop")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
	p.loaded.Store(false)
	p.current = ""
}

// TogglePause toggles the pause property.
func (p *Player) TogglePause() {
	if p == nil || p.closed.Load() {
		return
	}
	cs := C.CString("cycle pause")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
}

// ToggleMute toggles the mute property.
func (p *Player) ToggleMute() {
	if p == nil || p.closed.Load() {
		return
	}
	cs := C.CString("cycle mute")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
}

// SeekRelative seeks by delta seconds relative to the current position.
// Negative values seek backwards.
func (p *Player) SeekRelative(delta float64) {
	if p == nil || p.closed.Load() {
		return
	}
	cmd := fmt.Sprintf("seek %f relative+exact", delta)
	cs := C.CString(cmd)
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
}

// Current returns the path of the file currently loaded into mpv, or "" if
// nothing is playing.
func (p *Player) Current() string {
	if p == nil {
		return ""
	}
	return p.current
}

// Render asks mpv to blit the current frame into an internal w×h RGBA
// buffer and returns it. The returned image's backing array is reused on
// the next call, so callers must finish painting before calling Render
// again. Returns (nil, false) when nothing is loaded or the buffer can't
// be sized.
func (p *Player) Render(w, h int) (*image.RGBA, bool) {
	if p == nil || p.closed.Load() || !p.loaded.Load() {
		return nil, false
	}
	if w <= 0 || h <= 0 {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.buf == nil || p.buf.Rect.Dx() != w || p.buf.Rect.Dy() != h {
		p.buf = image.NewRGBA(image.Rect(0, 0, w, h))
	}

	stride := C.size_t(p.buf.Stride)
	rc := C.render_sw(p.ctx, C.int(w), C.int(h),
		stride, unsafe.Pointer(&p.buf.Pix[0]))
	if rc < 0 {
		return nil, false
	}
	// mpv wrote R, G, B at byte offsets 0, 1, 2; the 4th byte is garbage.
	// Force alpha to 0xff so the buffer is a valid premultiplied RGBA
	// image regardless of what background the caller paints over.
	pix := p.buf.Pix
	for i := 3; i < len(pix); i += 4 {
		pix[i] = 0xff
	}
	p.updatePending.Store(false)
	return p.buf, true
}

// NeedsRedraw reports whether mpv has signalled a new frame since the
// last Render. The viewer can use this to skip unnecessary redraws while
// the video is paused on a still frame.
func (p *Player) NeedsRedraw() bool {
	if p == nil {
		return false
	}
	return p.updatePending.Load()
}

// Close tears down the render context and the mpv handle.  After Close
// the Player must not be used again.
func (p *Player) Close() {
	if p == nil {
		return
	}
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	if p.ctx != nil {
		C.mpv_render_context_free(p.ctx)
		p.ctx = nil
	}
	if p.h != nil {
		C.mpv_terminate_destroy(p.h)
		p.h = nil
	}
	p.handle.Delete()
}

// drainEvents pumps mpv's event queue so it never blocks waiting for the
// host to consume events. The loop exits when mpv signals shutdown (which
// happens during Close via mpv_terminate_destroy).
func (p *Player) drainEvents() {
	for {
		ev := C.mpv_wait_event(p.h, -1)
		if ev == nil {
			return
		}
		if ev.event_id == C.MPV_EVENT_SHUTDOWN {
			return
		}
		if p.closed.Load() {
			return
		}
	}
}

//export goMpvRenderUpdate
func goMpvRenderUpdate(userdata C.uintptr_t) {
	h := cgo.Handle(uintptr(userdata))
	p, ok := h.Value().(*Player)
	if !ok || p == nil {
		return
	}
	p.updatePending.Store(true)
	if p.invalidate != nil {
		p.invalidate()
	}
}

//export goMpvWakeup
func goMpvWakeup(userdata C.uintptr_t) {
	// Wakeups for event-queue activity. The drainEvents goroutine is
	// already blocked on mpv_wait_event, so libmpv's internal signalling
	// will unblock it; we only need this callback registered so mpv
	// switches into asynchronous-event mode. No work to do here.
	_ = userdata
}
