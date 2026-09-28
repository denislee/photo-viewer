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

// request_log asks mpv to deliver log messages at min_level and above as
// MPV_EVENT_LOG_MESSAGE events on the normal event queue, where drainEvents
// picks them up. Used purely for crash diagnostics: an internal libmpv
// assertion is usually preceded by an error-level log line.
static int request_log(mpv_handle *h, const char *min_level) {
    return mpv_request_log_messages(h, min_level);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"runtime/cgo"
	"strings"
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

	// mu serialises every entry into libmpv from Go: Load, Stop, the
	// command helpers, and Render all hold it, so no two of them issue mpv
	// calls (or mutate buf / current / loaded) concurrently. The one thing
	// that must NOT take mu is the render-update / wakeup callbacks: mpv can
	// fire them synchronously from inside a command we're issuing under the
	// lock, so they stay lock-free (they only touch atomics + invalidate).
	mu        sync.Mutex
	buf       *image.RGBA // reused across frames; Pix capacity reused across size changes, freed on Stop
	closed    atomic.Bool
	loaded    atomic.Bool
	drainDone chan struct{} // closed when drainEvents returns
	// current holds the path of the file currently loaded. Read by the UI
	// goroutine (Current) and written by Load/Stop on whichever goroutine
	// called them; published via atomic.Pointer to avoid torn reads of
	// the string header.
	current atomic.Pointer[string]

	// updatePending is set by the render update callback and cleared on
	// the next successful render. It exists so callers can poll cheaply
	// for "is there a new frame to draw?" without entering cgo.
	updatePending atomic.Bool
}

// hwdecMode returns the value for mpv's hwdec option. It defaults to "no"
// (software decode) because hardware decoding is unavailable on the target
// machines and its failed probes destabilise the SW render path; PV_HWDEC
// overrides it for hosts with a working GPU stack (e.g. PV_HWDEC=auto-safe).
func hwdecMode() string {
	if v := strings.TrimSpace(os.Getenv("PV_HWDEC")); v != "" {
		return v
	}
	return "no"
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
		// Software decode only. On the target machines every hardware
		// backend is unavailable (no libcuda, vaapi setup fails, vulkan
		// lacks VK_KHR_video_decode_queue, the vdpau nvidia driver is
		// missing), so hwdec=auto-safe just churns through failing
		// "Failed setup … no frame!" probes on every load. Besides the log
		// spam, a half-initialised hwdec frame can be handed to the SW
		// renderer with dimensions that disagree with the params it computed
		// its source crop from — which trips libmpv's mp_image_crop assertion
		// (`x1 <= img->w`) and aborts the whole process. Forcing pure SW
		// decode removes that class of frame entirely. Set PV_HWDEC to
		// override (e.g. "auto-safe") on a box with a working GPU stack.
		{"hwdec", hwdecMode()},
		// Defer all cropping to mpv's own (params-consistent) VO crop rather
		// than letting libavcodec crop inside the decoder. With the default
		// vd-apply-cropping=yes, libavcodec applies only the alignment-aligned
		// part of a codec/container crop and shrinks the emitted frame to a
		// per-frame aligned width, leaving the sub-alignment remainder in the
		// frame's crop_* fields. The libmpv SW renderer, meanwhile, derives its
		// source crop rect from the VO's img_params (captured at reconfig), then
		// crops the *current* frame to it (libmpv_sw.c render()). When the
		// aligned frame comes out a few pixels narrower than the params-declared
		// size, that src rect overshoots the frame and libmpv aborts the whole
		// process on mp_image_crop's `x1 <= img->w` assertion. Forcing
		// apply-cropping=no makes every decoded frame the full, constant coded
		// size, so the params-derived crop rect is always in bounds. (hwdec=no
		// above only removes a *different* source of frame/params disagreement —
		// half-initialised hwdec frames — and does not cover this SW-decode path.)
		{"vd-apply-cropping", "no"},
		// Never hand the VO a rotated frame. vo_libmpv advertises
		// VO_CAP_ROTATE90 (true for its GPU backend), so mpv skips its own
		// autorotate filter and computes the src rect in *rotated*
		// coordinates. The SW backend we use ignores rotation, though, and
		// crops the unrotated frame with that rect: a 1920×1080 phone clip
		// tagged rotate=90 gets src y1=1920 against h=1080 and libmpv aborts
		// on mp_image_crop's `y1 <= img->h` assertion. We zero the rotation
		// here and instead bake it into the pixels with a lavfi transpose
		// filter per file (applyRotation, on MPV_EVENT_FILE_LOADED).
		{"video-rotate", "no"},
		{"loop-file", "inf"},
		{"keep-open", "always"},
		{"audio-display", "no"},
		{"osc", "no"},
		{"input-default-bindings", "no"},
		{"input-vo-keyboard", "no"},
		{"terminal", "no"},
		// Speed up first-frame display on large videos:
		// - profile=fast bundles vd-lavc-fast=yes,
		//   vd-lavc-skiploopfilter=nonkey, and scaling shortcuts.
		// - cache=yes + demuxer-* gives the demuxer a generous read-ahead
		//   so big MP4/MOV files don't bounce the disk seeking through
		//   the MOOV atom when seeking around.
		// - vd-lavc-threads=0 lets libavcodec autodetect cores; matters
		//   a lot for 4K HEVC.
		// - hr-seek-framedrop=yes drops frames during seeks so the first
		//   frame after a seek lands faster.
		{"profile", "fast"},
		{"cache", "yes"},
		{"demuxer-max-bytes", "256MiB"},
		{"demuxer-max-back-bytes", "64MiB"},
		{"demuxer-readahead-secs", "20"},
		{"vd-lavc-threads", "0"},
		{"hr-seek-framedrop", "yes"},
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

	// Surface mpv's own warnings/errors to stderr (see drainEvents). When an
	// internal libmpv/ffmpeg/hwdec assertion aborts the process, the message
	// it logs first lands in the log immediately above the Go crash dump,
	// which is the only breadcrumb a C-side abort() leaves behind.
	if lvl := C.CString("warn"); lvl != nil {
		C.request_log(h, lvl)
		C.free(unsafe.Pointer(lvl))
	}

	p := &Player{h: h, invalidate: invalidate, drainDone: make(chan struct{})}
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
	if p == nil {
		return errors.New("player closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return errors.New("player closed")
	}
	// Idempotent: the UI goroutine (playVideo) and the background preload
	// goroutine both call Load for the same entry, so without this guard a
	// race between their Current() checks would issue two loadfile commands
	// and double-write current/loaded. Holding mu makes the check-and-load
	// atomic and collapses the duplicate into a no-op.
	if cur := p.current.Load(); cur != nil && *cur == path {
		return nil
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	if rc := C.loadfile(p.h, cp); rc < 0 {
		return fmt.Errorf("mpv loadfile: %s", C.GoString(C.mpv_error_string(rc)))
	}
	pathCopy := path
	p.current.Store(&pathCopy)
	p.loaded.Store(true)
	p.updatePending.Store(true)
	return nil
}

// Stop halts playback and clears the active file. Used when the viewer
// moves off a video so we don't keep decoding in the background.
func (p *Player) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || !p.loaded.Load() {
		return
	}
	cs := C.CString("stop")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
	p.loaded.Store(false)
	p.current.Store(nil)
	// Drop the frame buffer: after leaving a video we'd otherwise retain a
	// window-sized RGBA (~33 MB for a 4K window) for the rest of the session
	// while the user browses photos.
	p.releaseBuf()
}

// TogglePause toggles the pause property.
func (p *Player) TogglePause() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	cs := C.CString("cycle pause")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
}

// ToggleMute toggles the mute property.
func (p *Player) ToggleMute() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	cs := C.CString("cycle mute")
	defer C.free(unsafe.Pointer(cs))
	C.command_str(p.h, cs)
}

// SeekRelative seeks by delta seconds relative to the current position.
// Negative values seek backwards.
func (p *Player) SeekRelative(delta float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
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
	if s := p.current.Load(); s != nil {
		return *s
	}
	return ""
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
	// Re-check after acquiring mu: Close may have freed p.ctx in the window
	// between the atomic checks above and this lock.
	if p.closed.Load() {
		return nil, false
	}

	p.ensureBuf(w, h)

	// Clear the "new frame pending" flag before the blit, not after. A render
	// update callback firing between render_sw returning and this store would
	// be lost if we cleared afterwards; clearing first means such a callback
	// re-arms updatePending and the next wakeup redraws. Otherwise a video
	// paused in that window could sit on one stale frame until the next
	// callback.
	p.updatePending.Store(false)
	stride := C.size_t(p.buf.Stride)
	rc := C.render_sw(p.ctx, C.int(w), C.int(h),
		stride, unsafe.Pointer(&p.buf.Pix[0]))
	if rc < 0 {
		return nil, false
	}
	return p.buf, true
}

// ensureBuf makes p.buf a w×h RGBA, reusing the existing backing array when
// it is large enough rather than allocating a fresh one on every size change
// (an interactive resize would otherwise reallocate every frame and churn the
// GC). A shrink keeps the larger capacity, so a later grow back within it
// reuses the same array too. mpv's rgb0 SW format writes only R, G, B per
// pixel and leaves the 4th byte untouched, so the alpha column is pre-filled
// to 0xff once per (re)size — the per-frame loop that used to scan every pixel
// (~480 MB/s of writes at 1080p60) is gone. Caller must hold p.mu.
func (p *Player) ensureBuf(w, h int) {
	if p.buf != nil && p.buf.Rect.Dx() == w && p.buf.Rect.Dy() == h {
		return // already the right size; alpha column already 0xff
	}
	// RGBA with no row padding: stride is exactly 4*w and the buffer is
	// 4*w*h bytes, matching image.NewRGBA's own layout.
	need := 4 * w * h
	if p.buf != nil && cap(p.buf.Pix) >= need {
		// Reslice the existing backing array (valid since need <= cap) and
		// rebuild the header for the new dimensions.
		p.buf = &image.RGBA{
			Pix:    p.buf.Pix[:need],
			Stride: 4 * w,
			Rect:   image.Rect(0, 0, w, h),
		}
	} else {
		p.buf = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	pix := p.buf.Pix
	for i := 3; i < len(pix); i += 4 {
		pix[i] = 0xff
	}
}

// releaseBuf drops the frame buffer so a large RGBA is not retained after
// playback stops. Caller must hold p.mu.
func (p *Player) releaseBuf() {
	p.buf = nil
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
//
// Teardown order matters: the drainEvents goroutine calls into p.h (it
// blocks in mpv_wait_event and runs applyRotation on FILE_LOADED), so it
// must be gone before the handle is destroyed. closed is set first, then
// mpv_wakeup (thread-safe) makes a pending or the next mpv_wait_event
// return MPV_EVENT_NONE; drainEvents sees closed and exits. Only then are
// the render context and the handle freed.
func (p *Player) Close() {
	if p == nil {
		return
	}
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	if p.h != nil {
		C.mpv_wakeup(p.h)
		if p.drainDone != nil {
			<-p.drainDone
		}
	}
	// Swap p.ctx out under mu so any in-progress Render either completes
	// before we free the render context, or sees closed=true when it
	// re-checks under mu. Render never touches p.ctx after seeing
	// closed=true, so no call reaches a freed pointer. Taking mu also
	// waits out any Load/Stop/command already inside libmpv, so nothing
	// is using p.h when it is destroyed below. libmpv requires the render
	// context to be freed before the handle.
	p.mu.Lock()
	ctx := p.ctx
	p.ctx = nil
	p.mu.Unlock()
	if ctx != nil {
		C.mpv_render_context_free(ctx)
	}
	if p.h != nil {
		C.mpv_terminate_destroy(p.h)
		p.h = nil
	}
	p.handle.Delete()
}

// drainEvents pumps mpv's event queue so it never blocks waiting for the
// host to consume events. The loop exits once Close has set closed (Close
// wakes mpv_wait_event with mpv_wakeup and waits for drainDone before
// destroying the handle), or if mpv shuts down on its own.
func (p *Player) drainEvents() {
	defer close(p.drainDone)
	for {
		ev := C.mpv_wait_event(p.h, -1)
		if ev == nil || p.closed.Load() {
			return
		}
		switch ev.event_id {
		case C.MPV_EVENT_SHUTDOWN:
			return
		case C.MPV_EVENT_LOG_MESSAGE:
			logMpvMessage((*C.mpv_event_log_message)(ev.data))
		case C.MPV_EVENT_FILE_LOADED:
			p.applyRotation()
		}
	}
}

// rotationFilters maps a container rotation (degrees clockwise) to the
// lavfi chain that bakes it into the pixels. Anything else — 0, or a
// non-right angle — gets no filter.
var rotationFilters = map[string]string{
	"90":  "lavfi=[transpose=clock]",
	"180": "lavfi=[hflip,vflip]",
	"270": "lavfi=[transpose=cclock]",
}

// rotationVF returns the vf value applyRotation sets for a demux-rotation
// property value: the transpose chain for a right angle, "" (clear the
// chain) for everything else.
func rotationVF(rot string) string {
	return rotationFilters[rot]
}

// applyRotation replaces the vf chain with the transpose matching the
// current video track's rotation metadata (see the video-rotate=no comment
// in New for why mpv can't do this itself). Always sets vf, so a rotated
// file's filter doesn't leak into the next, unrotated one. Runs on the
// drainEvents goroutine; Close waits for that goroutine to exit before it
// destroys p.h.
func (p *Player) applyRotation() {
	name := C.CString("current-tracks/video/demux-rotation")
	defer C.free(unsafe.Pointer(name))
	var rot string
	if s := C.mpv_get_property_string(p.h, name); s != nil {
		rot = C.GoString(s)
		C.mpv_free(unsafe.Pointer(s))
	}
	vfName := C.CString("vf")
	defer C.free(unsafe.Pointer(vfName))
	vf := C.CString(rotationVF(rot))
	defer C.free(unsafe.Pointer(vf))
	if rc := C.mpv_set_property_string(p.h, vfName, vf); rc < 0 {
		log.Printf("video: set vf for rotation %q: %s", rot, C.GoString(C.mpv_error_string(rc)))
	}
}

// logMpvMessage relays one mpv log line to stderr. The text mpv hands us
// already carries a trailing newline; we trim it so Go's log package adds
// exactly one. These lines are the breadcrumb that survives a C-side abort()
// — they print immediately before the crash dump.
func logMpvMessage(msg *C.mpv_event_log_message) {
	if msg == nil {
		return
	}
	text := strings.TrimRight(C.GoString(msg.text), "\n")
	if text == "" {
		return
	}
	log.Printf("[mpv %s] %s: %s", C.GoString(msg.level), C.GoString(msg.prefix), text)
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
