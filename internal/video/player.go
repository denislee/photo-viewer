// Package video embeds libmpv into the Gio UI.
//
// mpv renders each frame into a CPU pixel buffer we own, which we then hand
// to Gio as a paint.ImageOp, so mpv never has to share a GPU context with
// Gio. Two render paths produce that buffer:
//
//   - OpenGL (preferred): mpv gets its own headless EGL context (no window,
//     no surface) and renders through MPV_RENDER_API_TYPE_OPENGL into an
//     offscreen framebuffer, which we read back at window size. Decoding uses
//     VAAPI with zero-copy GPU interop, and scaling/colour conversion happen
//     on the GPU, so only the final window-sized RGBA crosses back to RAM.
//     This is what makes 4K 10-bit HEVC (e.g. DJI footage) play in real time:
//     software decode of those manages ~0.6x, and even hwdec=*-copy only
//     ~0.9x because downloading full-size 10-bit frames is the bottleneck.
//   - Software (fallback): MPV_RENDER_API_TYPE_SW, decode and scale on the
//     CPU. Used when no EGL/OpenGL context can be created, or when forced
//     with PV_VIDEO_RENDER=sw.
package video

/*
#cgo pkg-config: mpv egl
#include <mpv/client.h>
#include <mpv/render.h>
#include <mpv/render_gl.h>
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GL/gl.h>
#include <GL/glext.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

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
    // Don't let mpv sleep inside render until the frame's display time: the
    // call runs on the Gio UI goroutine, and blocking there for up to a full
    // frame period stalls the whole frame loop (input, paint, texture upload),
    // so the next render lands late and mpv drops frames to catch up. Gio's
    // own vsync-paced loop plus the render-update callback provide timing.
    int block = 0;
    mpv_render_param params[6];
    params[0].type = MPV_RENDER_PARAM_SW_SIZE;
    params[0].data = sz;
    params[1].type = MPV_RENDER_PARAM_SW_FORMAT;
    params[1].data = fmt;
    params[2].type = MPV_RENDER_PARAM_SW_STRIDE;
    params[2].data = &st;
    params[3].type = MPV_RENDER_PARAM_SW_POINTER;
    params[3].data = dst;
    params[4].type = MPV_RENDER_PARAM_BLOCK_FOR_TARGET_TIME;
    params[4].data = &block;
    params[5].type = MPV_RENDER_PARAM_INVALID;
    params[5].data = NULL;
    return mpv_render_context_render(ctx, params);
}

// ---- OpenGL render path -------------------------------------------------
//
// pv_gl owns a headless EGL context (Mesa surfaceless platform, no config, no
// surface) plus the offscreen framebuffer mpv renders into. The context is
// made current only for the duration of each call below, so it is never
// bound to a thread between calls: Go may run successive cgo calls on
// different OS threads, and the Go side serialises every call through
// Player.mu.
//
// EGL's current context and bound API are per *thread*, shared with Gio,
// whose own window context is often current on the very thread that calls
// Render. So "release" must not mean "make nothing current": that unbinds
// Gio's context and its next eglSwapBuffers fails with EGL_BAD_SURFACE,
// destroying the window. pv_gl_make_current snapshots the thread's EGL
// state and pv_gl_release puts exactly that state back. All GL entry points are resolved
// via eglGetProcAddress so we link against libEGL only.
typedef struct pv_gl {
    EGLDisplay dpy;
    EGLContext ctx;
    GLuint fbo, tex;
    int w, h;
    // drm_fd is the DRM render node of the GPU behind dpy, or -1. mpv's
    // VAAPI interop opens its VA display from it: with no window there is
    // no X11/Wayland display to derive one from.
    int drm_fd;
    // The calling thread's EGL state from before pv_gl_make_current.
    EGLenum saved_api;
    EGLDisplay saved_dpy;
    EGLContext saved_ctx;
    EGLSurface saved_draw, saved_read;
    PFNGLGENFRAMEBUFFERSPROC GenFramebuffers;
    PFNGLDELETEFRAMEBUFFERSPROC DeleteFramebuffers;
    PFNGLBINDFRAMEBUFFERPROC BindFramebuffer;
    PFNGLFRAMEBUFFERTEXTURE2DPROC FramebufferTexture2D;
    PFNGLCHECKFRAMEBUFFERSTATUSPROC CheckFramebufferStatus;
    void (*GenTextures)(GLsizei, GLuint *);
    void (*DeleteTextures)(GLsizei, const GLuint *);
    void (*BindTexture)(GLenum, GLuint);
    void (*TexImage2D)(GLenum, GLint, GLint, GLsizei, GLsizei, GLint, GLenum, GLenum, const void *);
    void (*TexParameteri)(GLenum, GLenum, GLint);
    void (*PixelStorei)(GLenum, GLint);
    void (*ReadPixels)(GLint, GLint, GLsizei, GLsizei, GLenum, GLenum, void *);
} pv_gl;

static void *pv_gl_proc(void *ctx, const char *name) {
    (void)ctx;
    return (void *)eglGetProcAddress(name);
}

// pv_gl_restore puts back the EGL state pv_gl_make_current saved.
static void pv_gl_restore(pv_gl *g) {
    if (g->saved_ctx != EGL_NO_CONTEXT)
        eglMakeCurrent(g->saved_dpy, g->saved_draw, g->saved_read, g->saved_ctx);
    else
        eglMakeCurrent(g->dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
    eglBindAPI(g->saved_api);
}

// pv_gl_make_current saves the thread's EGL state and makes g current. Every
// successful call must be paired with pv_gl_release.
static int pv_gl_make_current(pv_gl *g) {
    g->saved_api = eglQueryAPI();
    g->saved_dpy = eglGetCurrentDisplay();
    g->saved_ctx = eglGetCurrentContext();
    g->saved_draw = eglGetCurrentSurface(EGL_DRAW);
    g->saved_read = eglGetCurrentSurface(EGL_READ);
    if (eglBindAPI(EGL_OPENGL_API) &&
        eglMakeCurrent(g->dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, g->ctx))
        return 0;
    pv_gl_restore(g);
    return -1;
}

static void pv_gl_release(pv_gl *g) {
    pv_gl_restore(g);
}

// pv_gl_destroy frees the framebuffer, the context and the display. Safe on
// a partially-initialised g.
static void pv_gl_destroy(pv_gl *g) {
    if (!g) return;
    if (g->ctx != EGL_NO_CONTEXT) {
        if (pv_gl_make_current(g) == 0) {
            if (g->fbo) g->DeleteFramebuffers(1, &g->fbo);
            if (g->tex) g->DeleteTextures(1, &g->tex);
            pv_gl_release(g);
        }
        eglDestroyContext(g->dpy, g->ctx);
    }
    // No eglReleaseThread: it would also unbind Gio's context on this thread.
    if (g->dpy != EGL_NO_DISPLAY) eglTerminate(g->dpy);
    if (g->drm_fd >= 0) close(g->drm_fd);
    free(g);
}

// pv_gl_create returns a usable headless GL context, or NULL when the
// platform has none (no Mesa surfaceless EGL, no GPU, no driver).
static pv_gl *pv_gl_create(void) {
    pv_gl *g = calloc(1, sizeof *g);
    if (!g) return NULL;
    // eglCreateContext needs EGL_OPENGL_API bound; restore the caller's
    // binding (Gio's GLES) on the way out either way.
    EGLenum prev_api = eglQueryAPI();
    g->dpy = EGL_NO_DISPLAY;
    g->ctx = EGL_NO_CONTEXT;
    g->drm_fd = -1;
    g->dpy = eglGetPlatformDisplay(EGL_PLATFORM_SURFACELESS_MESA, EGL_DEFAULT_DISPLAY, NULL);
    if (g->dpy == EGL_NO_DISPLAY || !eglInitialize(g->dpy, NULL, NULL)) {
        g->dpy = EGL_NO_DISPLAY;
        goto fail;
    }
    if (!eglBindAPI(EGL_OPENGL_API)) goto fail;
    EGLint core[] = {
        EGL_CONTEXT_MAJOR_VERSION, 3,
        EGL_CONTEXT_MINOR_VERSION, 3,
        EGL_CONTEXT_OPENGL_PROFILE_MASK, EGL_CONTEXT_OPENGL_CORE_PROFILE_BIT,
        EGL_NONE,
    };
    g->ctx = eglCreateContext(g->dpy, EGL_NO_CONFIG_KHR, EGL_NO_CONTEXT, core);
    if (g->ctx == EGL_NO_CONTEXT)
        g->ctx = eglCreateContext(g->dpy, EGL_NO_CONFIG_KHR, EGL_NO_CONTEXT, NULL);
    if (g->ctx == EGL_NO_CONTEXT) goto fail;

#define PV_LOAD(field, name) \
    if (!(*(void **)&g->field = (void *)eglGetProcAddress(name))) goto fail;
    PV_LOAD(GenFramebuffers, "glGenFramebuffers")
    PV_LOAD(DeleteFramebuffers, "glDeleteFramebuffers")
    PV_LOAD(BindFramebuffer, "glBindFramebuffer")
    PV_LOAD(FramebufferTexture2D, "glFramebufferTexture2D")
    PV_LOAD(CheckFramebufferStatus, "glCheckFramebufferStatus")
    PV_LOAD(GenTextures, "glGenTextures")
    PV_LOAD(DeleteTextures, "glDeleteTextures")
    PV_LOAD(BindTexture, "glBindTexture")
    PV_LOAD(TexImage2D, "glTexImage2D")
    PV_LOAD(TexParameteri, "glTexParameteri")
    PV_LOAD(PixelStorei, "glPixelStorei")
    PV_LOAD(ReadPixels, "glReadPixels")
#undef PV_LOAD

    // Surfaceless make-current needs EGL_KHR_surfaceless_context; prove it
    // works now rather than on the first frame.
    if (pv_gl_make_current(g) < 0) goto fail;
    pv_gl_release(g);

    // Find the render node of the device EGL picked (EGL_EXT_device_query),
    // so VAAPI decodes on the same GPU that renders; else the first node.
    const char *node = NULL;
    PFNEGLQUERYDISPLAYATTRIBEXTPROC qda =
        (PFNEGLQUERYDISPLAYATTRIBEXTPROC)eglGetProcAddress("eglQueryDisplayAttribEXT");
    PFNEGLQUERYDEVICESTRINGEXTPROC qds =
        (PFNEGLQUERYDEVICESTRINGEXTPROC)eglGetProcAddress("eglQueryDeviceStringEXT");
    EGLAttrib dev = 0;
    if (qda && qds && qda(g->dpy, EGL_DEVICE_EXT, &dev) && dev)
        node = qds((EGLDeviceEXT)dev, EGL_DRM_RENDER_NODE_FILE_EXT);
    if (!node) node = "/dev/dri/renderD128";
    g->drm_fd = open(node, O_RDWR | O_CLOEXEC);
    eglBindAPI(prev_api);
    return g;
fail:
    pv_gl_destroy(g);
    eglBindAPI(prev_api);
    return NULL;
}

static int create_render_ctx_gl(mpv_handle *h, pv_gl *g, mpv_render_context **ctx) {
    if (pv_gl_make_current(g) < 0) return MPV_ERROR_UNSUPPORTED;
    mpv_opengl_init_params ip = { .get_proc_address = pv_gl_proc, .get_proc_address_ctx = NULL };
    // Only render_fd matters here (VAAPI's VA display); fd = -1 means no
    // KMS master, which keeps mpv's drmprime-overlay (KMS planes) disabled.
    mpv_opengl_drm_params_v2 drm = { .fd = -1, .render_fd = g->drm_fd };
    mpv_render_param params[4];
    int n = 0;
    params[n].type = MPV_RENDER_PARAM_API_TYPE;
    params[n++].data = (void *)MPV_RENDER_API_TYPE_OPENGL;
    params[n].type = MPV_RENDER_PARAM_OPENGL_INIT_PARAMS;
    params[n++].data = &ip;
    if (g->drm_fd >= 0) {
        params[n].type = MPV_RENDER_PARAM_DRM_DISPLAY_V2;
        params[n++].data = &drm;
    }
    params[n].type = MPV_RENDER_PARAM_INVALID;
    params[n].data = NULL;
    int rc = mpv_render_context_create(ctx, h, params);
    pv_gl_release(g);
    return rc;
}

// free_render_ctx_gl frees an mpv GL render context; mpv needs the GL
// context current to release its GPU resources.
static void free_render_ctx_gl(pv_gl *g, mpv_render_context *ctx) {
    int cur = pv_gl_make_current(g) == 0;
    mpv_render_context_free(ctx);
    if (cur) pv_gl_release(g);
}

// pv_gl_ensure_fbo (re)creates the w×h RGBA8 render target. Context must be
// current.
static int pv_gl_ensure_fbo(pv_gl *g, int w, int h) {
    if (g->fbo && g->w == w && g->h == h) return 0;
    if (!g->tex) g->GenTextures(1, &g->tex);
    g->BindTexture(GL_TEXTURE_2D, g->tex);
    g->TexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
    g->TexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
    g->TexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, w, h, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
    g->BindTexture(GL_TEXTURE_2D, 0);
    if (!g->fbo) g->GenFramebuffers(1, &g->fbo);
    g->BindFramebuffer(GL_FRAMEBUFFER, g->fbo);
    g->FramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, g->tex, 0);
    GLenum st = g->CheckFramebufferStatus(GL_FRAMEBUFFER);
    g->BindFramebuffer(GL_FRAMEBUFFER, 0);
    if (st != GL_FRAMEBUFFER_COMPLETE) {
        g->w = g->h = 0;
        return -1;
    }
    g->w = w;
    g->h = h;
    return 0;
}

// render_gl renders the current frame into the offscreen w×h framebuffer
// and reads it back into dst (tightly packed RGBA, top row first). Like
// render_sw it does not block for the frame's target time (see there).
static int render_gl(pv_gl *g, mpv_render_context *ctx, int w, int h, void *dst) {
    if (pv_gl_make_current(g) < 0) return MPV_ERROR_GENERIC;
    int rc = MPV_ERROR_GENERIC;
    if (pv_gl_ensure_fbo(g, w, h) == 0) {
        mpv_opengl_fbo fbo = { .fbo = (int)g->fbo, .w = w, .h = h, .internal_format = GL_RGBA8 };
        // No flip: into an FBO, mpv writes the top image row at GL row 0,
        // which is also the first row glReadPixels returns, so the buffer
        // comes back top-row-first as image.RGBA expects. (FLIP_Y=1 is for
        // the default framebuffer and would read back upside down.)
        int flip = 0;
        int block = 0;
        mpv_render_param params[4];
        params[0].type = MPV_RENDER_PARAM_OPENGL_FBO;
        params[0].data = &fbo;
        params[1].type = MPV_RENDER_PARAM_FLIP_Y;
        params[1].data = &flip;
        params[2].type = MPV_RENDER_PARAM_BLOCK_FOR_TARGET_TIME;
        params[2].data = &block;
        params[3].type = MPV_RENDER_PARAM_INVALID;
        params[3].data = NULL;
        rc = mpv_render_context_render(ctx, params);
        if (rc >= 0) {
            g->BindFramebuffer(GL_FRAMEBUFFER, g->fbo);
            g->PixelStorei(GL_PACK_ALIGNMENT, 4);
            g->ReadPixels(0, 0, w, h, GL_RGBA, GL_UNSIGNED_BYTE, dst);
            g->BindFramebuffer(GL_FRAMEBUFFER, 0);
        }
    }
    pv_gl_release(g);
    return rc;
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
	// gl is the headless GL context behind ctx when the OpenGL render path
	// is active, nil on the software path. Only touched under mu (and by
	// Close after the last Render).
	gl *C.pv_gl

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

// hwdecMode returns the value for mpv's hwdec option; PV_HWDEC overrides
// the default on either render path.
//
// On the OpenGL path it defaults to "vaapi": zero-copy GPU decode mapped
// straight into mpv's GL renderer, which is what lets 4K 10-bit HEVC play in
// real time. Where VAAPI is missing mpv logs the failure once per file and
// decodes in software, still scaling on the GPU.
//
// On the software path it defaults to "no": there, hwdec frames have to be
// copied back to RAM (too slow for 4K 10-bit anyway), and on machines with a
// broken GPU stack the failing probes hand the SW renderer half-initialised
// frames that trip libmpv's mp_image_crop assertion (see New).
func hwdecMode(gl bool) string {
	if v := strings.TrimSpace(os.Getenv("PV_HWDEC")); v != "" {
		return v
	}
	if gl {
		return "vaapi"
	}
	return "no"
}

// hwdecInterop returns the gpu-hwdec-interop option matching hwdecMode:
// "vaapi" for the GL path's default, "auto" when PV_HWDEC picks something
// else (or on the SW path, where no interop is used).
func hwdecInterop(gl bool) string {
	if gl && hwdecMode(gl) == "vaapi" {
		return "vaapi"
	}
	return "auto"
}

// wantGL reports whether to try the OpenGL render path. PV_VIDEO_RENDER=sw
// forces the software path (e.g. to rule out a GPU driver problem).
func wantGL() bool {
	return strings.TrimSpace(os.Getenv("PV_VIDEO_RENDER")) != "sw"
}

// New constructs and initialises a libmpv handle with a render context
// attached: OpenGL on a headless EGL context when one can be created,
// otherwise the software renderer (see the package doc). invalidate is
// called from mpv's worker threads whenever a new frame is available or an
// internal state change occurs; the caller should wire it to the Gio
// window's Invalidate so the frame loop wakes and calls Render.
func New(invalidate func()) (*Player, error) {
	// Create the GL context first: whether we have one decides the hwdec
	// and rotation options below, which must be set before mpv_initialize.
	var gl *C.pv_gl
	if wantGL() {
		gl = C.pv_gl_create()
		if gl == nil {
			log.Printf("video: no headless OpenGL context, using software rendering")
		}
	}
	h := C.mpv_create()
	if h == nil {
		C.pv_gl_destroy(gl)
		return nil, errors.New("mpv_create failed")
	}
	// rotate is the video-rotate option: on the software path rotation is
	// zeroed and baked in with a filter instead (see below); mpv's GL
	// renderer rotates correctly by itself.
	rotate := "no"
	if gl != nil {
		rotate = "0"
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
		// Software path: software decode only. On some machines every
		// hardware backend is unavailable (no libcuda, vaapi setup fails,
		// vulkan lacks VK_KHR_video_decode_queue, the vdpau nvidia driver is
		// missing), so hwdec=auto-safe just churns through failing
		// "Failed setup … no frame!" probes on every load. Besides the log
		// spam, a half-initialised hwdec frame can be handed to the SW
		// renderer with dimensions that disagree with the params it computed
		// its source crop from — which trips libmpv's mp_image_crop assertion
		// (`x1 <= img->w`) and aborts the whole process. Forcing pure SW
		// decode removes that class of frame entirely. The GL path has no
		// such crop and defaults to VAAPI instead (see hwdecMode).
		{"hwdec", hwdecMode(gl != nil)},
		// Load only the GPU interop that hwdec will use. mpv otherwise probes
		// every one it was built with, and in a headless context cuda,
		// vdpau and drmprime-overlay all fail, the last at error level on
		// every start.
		{"gpu-hwdec-interop", hwdecInterop(gl != nil)},
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
		// filter per file (applyRotation, on MPV_EVENT_FILE_LOADED). This is
		// SW-path only: the GL renderer rotates in its shader, and the lavfi
		// transpose could not take VAAPI GPU frames anyway.
		{"video-rotate", rotate},
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
			C.pv_gl_destroy(gl)
			return nil, err
		}
	}

	if rc := C.mpv_initialize(h); rc < 0 {
		C.mpv_terminate_destroy(h)
		C.pv_gl_destroy(gl)
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
	if gl != nil {
		if rc := C.create_render_ctx_gl(h, gl, &ctx); rc < 0 {
			log.Printf("video: mpv OpenGL render context: %s; using software rendering",
				C.GoString(C.mpv_error_string(rc)))
			C.pv_gl_destroy(gl)
			gl = nil
			ctx = nil
			// The options above were chosen for GL; switch them to the
			// SW-safe values before any file is loaded.
			for _, kv := range [][2]string{{"hwdec", hwdecMode(false)}, {"video-rotate", "no"}} {
				cn, cv := C.CString(kv[0]), C.CString(kv[1])
				C.mpv_set_property_string(h, cn, cv)
				C.free(unsafe.Pointer(cn))
				C.free(unsafe.Pointer(cv))
			}
		}
	}
	if gl == nil {
		if rc := C.create_render_ctx_sw(h, &ctx); rc < 0 {
			p.handle.Delete()
			C.mpv_terminate_destroy(h)
			return nil, fmt.Errorf("mpv_render_context_create: %s", C.GoString(C.mpv_error_string(rc)))
		}
	}
	p.ctx = ctx
	p.gl = gl
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
	var rc C.int
	if p.gl != nil {
		rc = C.render_gl(p.gl, p.ctx, C.int(w), C.int(h), unsafe.Pointer(&p.buf.Pix[0]))
	} else {
		stride := C.size_t(p.buf.Stride)
		rc = C.render_sw(p.ctx, C.int(w), C.int(h),
			stride, unsafe.Pointer(&p.buf.Pix[0]))
	}
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
	ctx, gl := p.ctx, p.gl
	p.ctx, p.gl = nil, nil
	p.mu.Unlock()
	if ctx != nil {
		if gl != nil {
			C.free_render_ctx_gl(gl, ctx)
		} else {
			C.mpv_render_context_free(ctx)
		}
	}
	if p.h != nil {
		C.mpv_terminate_destroy(p.h)
		p.h = nil
	}
	// Last: the decoder's VA display lives on gl's DRM fd, so the fd must
	// outlive mpv itself, not just the render context.
	C.pv_gl_destroy(gl)
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
			if p.gl == nil {
				p.applyRotation()
			}
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
