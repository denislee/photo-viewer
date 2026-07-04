package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
	"github.com/dns/photo-viewer/internal/video"
)

// Viewer renders the currently selected entry over the whole window. Open
// is true when the viewer should claim the screen. ShowInfo toggles the
// metadata side panel (bound to 'i').
type Viewer struct {
	Open     bool
	Index    int
	ShowInfo bool

	// Confirming is true while the delete-confirmation modal is up. While
	// true, Enter triggers the deletion and Esc/Ctrl+[ dismiss.
	Confirming bool

	// InTrash is set by the window each frame to mirror whether the active
	// view is the Trash sentinel. When true, the confirm modal copy flips
	// to "Restore this file from trash?" since DeletePath repurposes the
	// keybind as Restore for trash items.
	InTrash bool

	entries []cache.Entry

	loadedPath       string
	loadingPath      string
	loadingCtxCancel context.CancelFunc
	loadedOp         paint.ImageOp
	loadedSz         image.Point

	// recent caches up to viewerRecentCap previously-decoded paint.ImageOps
	// so navigating back to a recently viewed entry avoids re-decoding the
	// original. Ordered MRU→LRU; LRU is dropped when the cap is reached.
	//
	// Touched both from the layout goroutine and from background decode
	// goroutines (main load + neighbour prefetch), so all access goes
	// through recentMu.
	recentMu    sync.Mutex
	recent      []recentImage
	prefetching map[string]bool

	// Decoded dimensions are cached per path. Decoding happens off the UI
	// goroutine so opening the viewer doesn't stutter on big originals.
	dimMu    sync.Mutex
	dimCache map[string]string
	dimAsked map[string]bool

	// Media info (created, camera, lens) is fetched in the background.
	infoMu    sync.Mutex
	infoCache map[string]cachedInfo
	infoAsked map[string]bool

	// player is lazily created the first time the viewer lands on a video.
	// It's kept alive between videos so navigation between clips doesn't
	// pay the libmpv-init cost each time; the player itself is also a
	// no-op when libmpv failed to load on this machine.
	// player is stored atomically: playerOnce.Do initialises it on whichever
	// goroutine first calls Player() (often the preload goroutine), while
	// Close / stopVideoIfRunning read it from the UI thread. An atomic pointer
	// lets those readers observe it without racing the Once's write.
	playerOnce sync.Once
	playerErr  error
	player     atomic.Pointer[video.Player]

	invalidate func()
}

// viewerRecentCap is the size of the recently-decoded paint.ImageOp LRU. Each
// entry is one decoded original, capped to viewerMaxPreviewSide on the long
// side, so the worst case is ~viewerRecentCap × (side² × 4 bytes) of pixel
// memory — for 3 × 4096px that's roughly 200 MB, acceptable for an image
// viewer running interactively.
const viewerRecentCap = 3

// viewerMetaCap is the soft cap on entries in dimCache/infoCache (and their
// "already asked" sidecars). The maps are bulk-cleared when they exceed the
// cap so the viewer's metadata cache can't grow unboundedly as the user
// pages through a large library. Recomputing is cheap (one ExifTool / image
// header read) so a periodic clear is preferable to running a full LRU.
const viewerMetaCap = 1024

// viewerMaxPreviewSide caps the long side of the decoded preview. RAW/HEIC
// originals can exceed 8000 px on one side; rendering them at full res
// creates an enormous GPU texture for no visible benefit on a typical
// display. The slow path is reserved for an explicit zoom mode (TODO).
const viewerMaxPreviewSide = 4096

type recentImage struct {
	path string
	op   paint.ImageOp
	sz   image.Point
}

type cachedInfo struct {
	Created      string
	Camera       string
	Lens         string
	Aperture     string
	ShutterSpeed string
	ISO          string
	FocalLength  string
}

// SetInvalidate wires a callback used to wake the Gio frame loop after the
// background dimension decoder finishes.
func (v *Viewer) SetInvalidate(f func()) { v.invalidate = f }

func (v *Viewer) Show(entries []cache.Entry, idx int) {
	// Copy the slice: the caller hands us the controller's live entries
	// (via Snapshot), whose backing array the grid keeps reading. ConfirmDelete
	// shifts elements in place and the 'F' favorite toggle writes through
	// &v.entries[i]; both would corrupt the array shared with the grid, so the
	// viewer owns its own copy instead.
	v.entries = append([]cache.Entry(nil), entries...)
	v.Index = idx
	v.Open = true
	v.preloadVideoIfNeeded()
	v.prefetchAt(idx + 1)
	v.prefetchAt(idx - 1)
}

func (v *Viewer) Close() {
	v.Open = false
	v.Confirming = false
	v.loadedPath = ""
	v.loadingPath = ""
	if v.loadingCtxCancel != nil {
		v.loadingCtxCancel()
		v.loadingCtxCancel = nil
	}
	v.loadedOp = paint.ImageOp{}
	v.recentMu.Lock()
	v.recent = nil
	v.prefetching = nil
	v.recentMu.Unlock()
	if p := v.player.Load(); p != nil {
		p.Stop()
	}
}

// stopVideoIfRunning halts mpv playback whenever the current entry isn't a
// video. Called from Next/Prev so paging away from a clip releases its
// decoder; the player itself stays warm for the next video.
func (v *Viewer) stopVideoIfRunning() {
	p := v.player.Load()
	if p == nil {
		return
	}
	if v.Index < 0 || v.Index >= len(v.entries) || v.entries[v.Index].Type != scan.TypeVideo {
		p.Stop()
	}
}

// preloadVideoIfNeeded kicks an mpv loadfile for the current entry if it's a
// video, in a goroutine so the UI thread doesn't pay the initial libmpv
// initialisation cost on first call. Subsequent calls are cheap. By the time
// Layout runs and calls playVideo, mpv is already demuxing — first-frame
// latency drops by however long Layout would otherwise have spent waiting.
func (v *Viewer) preloadVideoIfNeeded() {
	if v.Index < 0 || v.Index >= len(v.entries) {
		return
	}
	e := v.entries[v.Index]
	if e.Type != scan.TypeVideo {
		return
	}
	go func(path string) {
		p := v.Player()
		if p == nil {
			return
		}
		if p.Current() == path {
			return
		}
		_ = p.Load(path)
	}(e.Path)
}

// parkLoaded moves the current loadedOp into the MRU position of the recent
// cache if there is one. Called whenever loadedPath is about to be cleared
// (Next/Prev/Delete/Close-ish) so we can rehydrate without re-decoding when
// the user navigates back.
func (v *Viewer) parkLoaded() {
	if v.loadedPath == "" || v.loadedSz == (image.Point{}) {
		return
	}
	v.cachePut(v.loadedPath, v.loadedOp, v.loadedSz)
}

// cachePut inserts (path, op) at the MRU end of v.recent, evicting the
// oldest entry past viewerRecentCap.
func (v *Viewer) cachePut(path string, op paint.ImageOp, sz image.Point) {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for i, r := range v.recent {
		if r.path == path {
			v.recent = append(v.recent[:i], v.recent[i+1:]...)
			break
		}
	}
	v.recent = append([]recentImage{{path: path, op: op, sz: sz}}, v.recent...)
	if len(v.recent) > viewerRecentCap {
		v.recent = v.recent[:viewerRecentCap]
	}
}

// cacheGet returns the cached image op for path and moves the entry to MRU.
func (v *Viewer) cacheGet(path string) (paint.ImageOp, image.Point, bool) {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for i, r := range v.recent {
		if r.path == path {
			if i != 0 {
				v.recent = append([]recentImage{r}, append(v.recent[:i], v.recent[i+1:]...)...)
			}
			return r.op, r.sz, true
		}
	}
	return paint.ImageOp{}, image.Point{}, false
}

// cacheHas reports whether path is in the recent cache without promoting it.
func (v *Viewer) cacheHas(path string) bool {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for _, r := range v.recent {
		if r.path == path {
			return true
		}
	}
	return false
}

// RequestDelete opens the delete-confirmation modal for the current entry.
// Has no effect if the viewer isn't open or there is no current entry.
func (v *Viewer) RequestDelete() {
	if !v.Open || v.Index < 0 || v.Index >= len(v.entries) {
		return
	}
	v.Confirming = true
}

// CancelDelete dismisses the confirmation modal without deleting anything.
func (v *Viewer) CancelDelete() { v.Confirming = false }

// ConfirmDelete invokes deleter for the current entry's path, removes it
// from the local entry list, and advances to the next entry. If the list
// becomes empty the viewer closes.
func (v *Viewer) ConfirmDelete(deleter func(path string) error) {
	if !v.Confirming || v.Index < 0 || v.Index >= len(v.entries) {
		v.Confirming = false
		return
	}
	e := v.entries[v.Index]
	v.Confirming = false
	if err := deleter(e.Path); err != nil {
		return
	}
	v.entries = append(v.entries[:v.Index], v.entries[v.Index+1:]...)
	if len(v.entries) == 0 {
		v.Close()
		return
	}
	if v.Index >= len(v.entries) {
		v.Index = len(v.entries) - 1
	}
	// Drop the deleted entry from the recent cache too — the path no
	// longer exists, and the cache entry would never get evicted otherwise.
	v.recentMu.Lock()
	for i, r := range v.recent {
		if r.path == e.Path {
			v.recent = append(v.recent[:i], v.recent[i+1:]...)
			break
		}
	}
	v.recentMu.Unlock()
	v.cancelLoading()
	v.loadedPath = ""
	v.loadingPath = ""
	v.loadedOp = paint.ImageOp{}
}

func (v *Viewer) cancelLoading() {
	if v.loadingCtxCancel != nil {
		v.loadingCtxCancel()
		v.loadingCtxCancel = nil
	}
}

func (v *Viewer) Next() {
	if v.Index < len(v.entries)-1 {
		v.Index++
		v.parkLoaded()
		v.cancelLoading()
		v.loadedPath = ""
		v.loadingPath = ""
		v.loadedOp = paint.ImageOp{}
		v.stopVideoIfRunning()
		v.preloadVideoIfNeeded()
		v.prefetchAt(v.Index + 1)
	}
}

func (v *Viewer) Prev() {
	if v.Index > 0 {
		v.Index--
		v.parkLoaded()
		v.cancelLoading()
		v.loadedPath = ""
		v.loadingPath = ""
		v.loadedOp = paint.ImageOp{}
		v.stopVideoIfRunning()
		v.preloadVideoIfNeeded()
		v.prefetchAt(v.Index - 1)
	}
}

// Player returns the lazily-initialised libmpv player, or nil if libmpv
// failed to load. The first call initialises the player; subsequent calls
// return the cached instance.
func (v *Viewer) Player() *video.Player {
	v.playerOnce.Do(func() {
		p, err := video.New(v.invalidate)
		v.playerErr = err
		v.player.Store(p)
	})
	return v.player.Load()
}

// playVideo loads the current entry into mpv if it isn't already, renders
// the current frame at the requested size, and returns it wrapped as a
// paint.ImageOp. The mpv-side renderer letterboxes within (w, h) so the
// caller can paint the result at imgArea without further fitting. Returns
// (false) when libmpv is unavailable or no frame is ready yet.
func (v *Viewer) playVideo(e cache.Entry, w, h int) (paint.ImageOp, bool) {
	p := v.Player()
	if p == nil {
		return paint.ImageOp{}, false
	}
	if p.Current() != e.Path {
		if err := p.Load(e.Path); err != nil {
			return paint.ImageOp{}, false
		}
	}
	frame, ok := p.Render(w, h)
	if !ok {
		return paint.ImageOp{}, false
	}
	op := paint.NewImageOp(frame)
	op.Filter = paint.FilterLinear
	return op, true
}

// Layout fills gtx.Constraints with a black background and the active
// image scaled to fit. Videos and unsupported formats fall back to a
// placeholder color. When ShowInfo is true a sidebar panel is laid out on
// the right with the entry's metadata.
func (v *Viewer) Layout(gtx layout.Context, th *Theme, tc *thumbCache) layout.Dimensions {
	size := gtx.Constraints.Max
	rect := image.Rectangle{Max: size}
	clipArea := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{A: 0xff}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	if v.Index < 0 || v.Index >= len(v.entries) {
		clipArea.Pop()
		return layout.Dimensions{Size: size}
	}
	e := v.entries[v.Index]

	imgArea := rect
	infoW := 0
	if v.ShowInfo {
		infoW = gtx.Dp(unit.Dp(300))
		if infoW > size.X/2 {
			infoW = size.X / 2
		}
		imgArea = image.Rectangle{Max: image.Pt(size.X-infoW, size.Y)}
	}

	if e.Type == scan.TypeVideo {
		if op, ok := v.playVideo(e, imgArea.Dx(), imgArea.Dy()); ok {
			// mpv already letterboxed to (Dx, Dy), so we paint at
			// imgArea.Min with the rect's native size.
			off := opOffset(gtx, imgArea.Min)
			op.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
			off.Pop()
		} else if imgOp, imgSz, fb := tc.Get(e); fb {
			// While mpv is still warming up or libmpv is unavailable,
			// show the cached thumbnail so the screen isn't blank.
			drawFitted(gtx, imgOp, imgSz, imgArea)
		}
	} else {
		imgOp, imgSz, ok := v.imageFor(e, tc)
		if ok {
			drawFitted(gtx, imgOp, imgSz, imgArea)
		}
	}

	if v.ShowInfo {
		v.layoutInfoPanel(gtx, th, e, image.Rectangle{
			Min: image.Pt(size.X-infoW, 0),
			Max: image.Pt(size.X, size.Y),
		})
	}

	if v.Confirming {
		drawDeleteConfirm(gtx, th, filepath.Base(e.Path), rect, v.InTrash)
	}

	clipArea.Pop()
	return layout.Dimensions{Size: size}
}

// drawDeleteConfirm dims rect and centers a confirmation box showing
// filename and the available shortcuts. Shared between the viewer and grid.
// When restore is true the prompt asks about restoring an item out of the
// trash instead of deleting it, since the same key binding handles both.
func drawDeleteConfirm(gtx layout.Context, th *Theme, filename string, rect image.Rectangle, restore bool) {
	dimStack := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{A: 0xb0}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	dimStack.Pop()

	boxW := gtx.Dp(unit.Dp(420))
	if boxW > rect.Dx()-gtx.Dp(unit.Dp(40)) {
		boxW = rect.Dx() - gtx.Dp(unit.Dp(40))
	}
	pad := gtx.Dp(unit.Dp(20))

	macro := op.Record(gtx.Ops)
	innerGtx := gtx
	innerGtx.Constraints.Max = image.Pt(boxW-pad*2, rect.Dy())
	innerGtx.Constraints.Min = image.Pt(boxW-pad*2, 0)
	contentDims := layout.Flex{Axis: layout.Vertical, Spacing: layout.SpaceEnd}.Layout(innerGtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			title := "Delete this file?"
			if restore {
				title = "Restore this file from trash?"
			}
			lbl := material.Label(th.Theme, unit.Sp(15), title)
			lbl.Color = th.Foreground
			lbl.Font.Weight = 700
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(13), filename)
			lbl.Color = th.Foreground
			lbl.MaxLines = 2
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			hint := "Enter to delete    Esc / Ctrl+[ to cancel"
			if restore {
				hint = "Enter to restore    Esc / Ctrl+[ to cancel"
			}
			lbl := material.Label(th.Theme, unit.Sp(12), hint)
			lbl.Color = th.Muted
			return lbl.Layout(gtx)
		}),
	)
	contentCall := macro.Stop()

	boxH := contentDims.Size.Y + pad*2
	boxRect := image.Rectangle{
		Min: image.Pt(rect.Min.X+(rect.Dx()-boxW)/2, rect.Min.Y+(rect.Dy()-boxH)/2),
		Max: image.Pt(rect.Min.X+(rect.Dx()-boxW)/2+boxW, rect.Min.Y+(rect.Dy()-boxH)/2+boxH),
	}
	bg := clip.UniformRRect(boxRect, gtx.Dp(unit.Dp(8))).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{R: 0x1c, G: 0x1f, B: 0x26, A: 0xff}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()
	drawBorder(gtx, boxRect, 1, color.NRGBA{R: 0xc0, G: 0x40, B: 0x40, A: 0xff})

	off := op.Offset(image.Pt(boxRect.Min.X+pad, boxRect.Min.Y+pad)).Push(gtx.Ops)
	contentCall.Add(gtx.Ops)
	off.Pop()
}

// layoutInfoPanel draws the metadata sidebar at rect. The panel has its own
// dark background to keep it readable over photo content.
func (v *Viewer) layoutInfoPanel(gtx layout.Context, th *Theme, e cache.Entry, rect image.Rectangle) {
	// Background fill.
	bg := clip.Rect(rect).Push(gtx.Ops)
	paint.ColorOp{Color: color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xee}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	bg.Pop()

	dims := v.dimensionsFor(e)
	info := v.mediaInfoFor(e)

	pad := gtx.Dp(unit.Dp(12))
	innerW := rect.Dx() - pad*2
	if innerW < 80 {
		innerW = 80
	}
	innerH := rect.Dy() - pad*2
	if innerH < 60 {
		innerH = 60
	}
	innerGtx := gtx
	innerGtx.Constraints.Max = image.Pt(innerW, innerH)
	innerGtx.Constraints.Min = image.Pt(0, 0)
	clipArea := clip.Rect(rect).Push(gtx.Ops)
	defer clipArea.Pop()
	off := opOffset(gtx, image.Pt(rect.Min.X+pad, rect.Min.Y+pad))
	defer off.Pop()

	rows := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(14), filepath.Base(e.Path))
			lbl.Color = th.Foreground
			lbl.Font.Weight = 700
			lbl.MaxLines = 3
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
	}
	addRow := func(label, value string) {
		rows = append(rows,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(11), label)
				lbl.Color = color.NRGBA{R: 0x90, G: 0x95, B: 0xa0, A: 0xff}
				lbl.Font.Weight = 700
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12), value)
				lbl.Color = th.Foreground
				lbl.MaxLines = 4
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
		)
	}
	addRow("Path", e.Path)
	addRow("Type", titleCaseGio(e.Type.String()))
	addRow("Size", formatBytesGio(e.Size))
	addRow("Created", info.Created)
	addRow("Modified", e.ModTime.Local().Format("2006-01-02 15:04:05"))
	addRow("Dimensions", dims)
	addRow("Camera", info.Camera)
	addRow("Lens", info.Lens)

	if info.Aperture != "—" || info.ShutterSpeed != "—" || info.ISO != "—" || info.FocalLength != "—" {
		config := fmt.Sprintf("%s  %s  ISO %s  %s",
			fallback(info.Aperture, "—"),
			fallback(info.ShutterSpeed, "—"),
			fallback(info.ISO, "—"),
			fallback(info.FocalLength, "—"))
		addRow("Settings", config)
	}

	if e.Favorite {
		addRow("Favorite", "Yes")
	} else {
		addRow("Favorite", "No")
	}

	layout.Flex{Axis: layout.Vertical}.Layout(innerGtx, rows...)
}

// dimensionsFor returns a "WxH" string for image entries, "—" for video/RAW/
// HEIC, or "…" while a background decode is still running. The decode is
// kicked off lazily once per path.
func (v *Viewer) dimensionsFor(e cache.Entry) string {
	if e.Type == scan.TypeRAW || e.Type == scan.TypeHEIC || e.Type == scan.TypeVideo {
		return "—"
	}
	v.dimMu.Lock()
	if v.dimCache == nil {
		v.dimCache = map[string]string{}
		v.dimAsked = map[string]bool{}
	}
	if cached, ok := v.dimCache[e.Path]; ok {
		v.dimMu.Unlock()
		return cached
	}
	if v.dimAsked[e.Path] {
		v.dimMu.Unlock()
		return "…"
	}
	v.dimAsked[e.Path] = true
	v.dimMu.Unlock()

	go func(path string) {
		dim := decodeDimensions(path)
		v.dimMu.Lock()
		if len(v.dimCache) >= viewerMetaCap {
			v.dimCache = map[string]string{}
			v.dimAsked = map[string]bool{}
		}
		v.dimCache[path] = dim
		v.dimMu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}(e.Path)
	return "…"
}

func decodeDimensions(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "—"
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%d × %d", cfg.Width, cfg.Height)
}

// mediaInfoFor returns the extended metadata for the entry, or empty/placeholder
// values while a background fetch is still running.
func (v *Viewer) mediaInfoFor(e cache.Entry) cachedInfo {
	v.infoMu.Lock()
	if v.infoCache == nil {
		v.infoCache = map[string]cachedInfo{}
		v.infoAsked = map[string]bool{}
	}
	if cached, ok := v.infoCache[e.Path]; ok {
		v.infoMu.Unlock()
		return cached
	}
	if v.infoAsked[e.Path] {
		v.infoMu.Unlock()
		return cachedInfo{Created: "…", Camera: "…", Lens: "…", Aperture: "…", ShutterSpeed: "…", ISO: "…", FocalLength: "…"}
	}
	v.infoAsked[e.Path] = true
	v.infoMu.Unlock()

	go func(path string) {
		info := scan.GetMediaInfo(path)
		c := cachedInfo{
			Created:      info.Created.Local().Format("2006-01-02 15:04:05"),
			Camera:       info.Camera,
			Lens:         info.Lens,
			Aperture:     info.Aperture,
			ShutterSpeed: info.ShutterSpeed,
			ISO:          info.ISO,
			FocalLength:  info.FocalLength,
		}
		if c.Camera == "" {
			c.Camera = "—"
		}
		if c.Lens == "" {
			c.Lens = "—"
		}
		if c.Aperture == "" {
			c.Aperture = "—"
		}
		if c.ShutterSpeed == "" {
			c.ShutterSpeed = "—"
		}
		if c.ISO == "" {
			c.ISO = "—"
		}
		if c.FocalLength == "" {
			c.FocalLength = "—"
		}
		v.infoMu.Lock()
		if len(v.infoCache) >= viewerMetaCap {
			v.infoCache = map[string]cachedInfo{}
			v.infoAsked = map[string]bool{}
		}
		v.infoCache[path] = c
		v.infoMu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}(e.Path)
	return cachedInfo{Created: "…", Camera: "…", Lens: "…", Aperture: "…", ShutterSpeed: "…", ISO: "…", FocalLength: "…"}
}

// opOffset is a thin wrapper around op.Offset so defer Pop() reads cleanly
// next to its push site.
func opOffset(gtx layout.Context, pt image.Point) op.TransformStack {
	return op.Offset(pt).Push(gtx.Ops)
}

func titleCaseGio(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// imageFor decodes the original photo on demand for photo/HEIC/RAW types and
// caches a small LRU of recent decodes; for videos and decode failures it
// falls back to the thumbnail.
func (v *Viewer) imageFor(e cache.Entry, tc *thumbCache) (paint.ImageOp, image.Point, bool) {
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
		if v.loadedPath == e.Path {
			return v.loadedOp, v.loadedSz, true
		}
		// Rehydrate from the recent LRU so back-and-forth navigation is
		// instant. Promote into the live slot so the next frame paints
		// from it without falling back to the thumbnail.
		if op, sz, ok := v.cacheGet(e.Path); ok {
			v.loadedPath = e.Path
			v.loadedOp = op
			v.loadedSz = sz
			v.loadingPath = ""
			return op, sz, true
		}
		if v.loadingPath != e.Path {
			v.loadingPath = e.Path
			v.kickDecode(e)
		}
	}
	// Fall back to the cached thumbnail (always JPEG).
	op, sz, ok := tc.Get(e)
	return op, sz, ok
}

// kickDecode launches a background decode for the given entry,
// then assigns the result to the viewer's loaded* fields and invalidates so
// the next frame paints the full-resolution image.
func (v *Viewer) kickDecode(e cache.Entry) {
	v.cancelLoading()
	ctx, cancel := context.WithCancel(context.Background())
	v.loadingCtxCancel = cancel

	go func() {
		defer cancel()
		op, sz, ok := decodeOriginal(ctx, e)
		if !ok || ctx.Err() != nil {
			return
		}
		// Don't mutate v.loadedPath/Op/Sz from here — those fields are
		// read by the UI goroutine in Layout and writing them off-thread
		// is a data race (string headers and paint.ImageOp aren't atomic
		// to copy). cachePut is mutex-guarded; the next Layout will see
		// the result via imageFor's cacheGet path, which promotes into
		// the live slot on the UI goroutine.
		v.cachePut(e.Path, op, sz)
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
}

// prefetchAt decodes the entry at idx in the background and parks the result
// in the recent cache so a subsequent Next/Prev to it is instant. It does
// not touch loadedPath/Op/Sz — only the LRU. No-op if idx is out of range,
// the entry is already cached, currently loading, or already being
// prefetched.
//
// For video entries we don't decode anything — mpv owns playback — but we
// do warm the OS page cache by streaming the first few MB so that when the
// user navigates to the clip, mpv's demuxer doesn't have to wait on a cold
// disk read for the MOOV atom + first GOP.
func (v *Viewer) prefetchAt(idx int) {
	if idx < 0 || idx >= len(v.entries) {
		return
	}
	e := v.entries[idx]
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
	case scan.TypeVideo:
		v.prefetchVideo(e)
		return
	default:
		return
	}
	if v.loadingPath == e.Path || v.loadedPath == e.Path {
		return
	}
	if v.cacheHas(e.Path) {
		return
	}
	v.recentMu.Lock()
	if v.prefetching == nil {
		v.prefetching = map[string]bool{}
	}
	if v.prefetching[e.Path] {
		v.recentMu.Unlock()
		return
	}
	v.prefetching[e.Path] = true
	v.recentMu.Unlock()

	go func(e cache.Entry) {
		defer func() {
			v.recentMu.Lock()
			delete(v.prefetching, e.Path)
			v.recentMu.Unlock()
		}()
		op, sz, ok := decodeOriginal(context.Background(), e)
		if !ok {
			return
		}
		v.cachePut(e.Path, op, sz)
	}(e)
}

// videoPrefetchBytes is the head-of-file window we read into the OS page
// cache for neighbour videos. Picked so that the MOOV atom of a typical
// MP4/MOV (a few MB) and the first GOP land in cache, without churning RAM
// for files we may never open.
const videoPrefetchBytes = 16 << 20

// prefetchVideo warms the OS page cache for an adjacent video by reading
// its first videoPrefetchBytes. Doesn't touch the player — mpv only loads
// one file at a time — but on a cold cache this is the difference between
// mpv blocking on disk seeks and the first frame appearing instantly.
func (v *Viewer) prefetchVideo(e cache.Entry) {
	v.recentMu.Lock()
	if v.prefetching == nil {
		v.prefetching = map[string]bool{}
	}
	if v.prefetching[e.Path] {
		v.recentMu.Unlock()
		return
	}
	v.prefetching[e.Path] = true
	v.recentMu.Unlock()

	go func(path string) {
		defer func() {
			v.recentMu.Lock()
			delete(v.prefetching, path)
			v.recentMu.Unlock()
		}()
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = io.CopyN(io.Discard, f, videoPrefetchBytes)
	}(e.Path)
}

// decodeOriginal decodes an entry's full-resolution image into a paint.ImageOp,
// downscaled to viewerMaxPreviewSide. Returns (op, size, true) on success or
// the zero value with false on any error / cancellation.
func decodeOriginal(ctx context.Context, e cache.Entry) (paint.ImageOp, image.Point, bool) {
	var img image.Image
	var err error
	switch e.Type {
	case scan.TypeHEIC:
		tmpDir, err2 := os.MkdirTemp("", "photo-viewer-heicview-")
		if err2 != nil {
			return paint.ImageOp{}, image.Point{}, false
		}
		defer os.RemoveAll(tmpDir)
		jpgPath, err2 := thumb.HEICToJPEG(ctx, e.Path, tmpDir)
		if err2 != nil {
			return paint.ImageOp{}, image.Point{}, false
		}
		f, err2 := os.Open(jpgPath)
		if err2 != nil {
			return paint.ImageOp{}, image.Point{}, false
		}
		img, _, err = image.Decode(f)
		f.Close()
	case scan.TypeRAW:
		img, err = thumb.LoadRAWImage(ctx, e.Path)
	case scan.TypePhoto:
		f, err2 := os.Open(e.Path)
		if err2 == nil {
			img, _, err = image.Decode(f)
			f.Close()
		}
	}
	if err != nil || img == nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	if ctx.Err() != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	img = downscalePreview(img, viewerMaxPreviewSide)
	op := paint.NewImageOp(img)
	op.Filter = paint.FilterLinear
	return op, img.Bounds().Size(), true
}

// downscalePreview returns img resized so its long side is at most maxSide.
// Images already within the cap are returned unchanged so the common case
// (already-small JPEGs) avoids an extra allocation and resample.
func downscalePreview(img image.Image, maxSide int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longSide := w
	if h > longSide {
		longSide = h
	}
	if longSide <= maxSide {
		return img
	}
	s := float64(maxSide) / float64(longSide)
	dstW := int(float64(w) * s)
	dstH := int(float64(h) * s)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// dim is a helper that returns layout.Dimensions of the given size.
func dim(sz image.Point) layout.Dimensions { return layout.Dimensions{Size: sz} }

// affineTranslate is currently unused; kept for documentation in case a
// future iteration adds pan/zoom and needs an explicit translation.
func affineTranslate(dx, dy float32) f32.Affine2D {
	return f32.Affine2D{}.Offset(f32.Pt(dx, dy))
}
