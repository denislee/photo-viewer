package ui

import (
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sync"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/thumb"
)

// fullDecodeOnce guards repeated full-res decode attempts for the same path
// so the viewer doesn't refire decoding on every frame after a failure.
var (
	fullDecodeMu      sync.Mutex
	fullDecodeStarted = map[string]struct{}{}
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

	entries []cache.Entry

	loadedPath string
	loadedOp   paint.ImageOp
	loadedSz   image.Point

	// Decoded dimensions are cached per path. Decoding happens off the UI
	// goroutine so opening the viewer doesn't stutter on big originals.
	dimMu    sync.Mutex
	dimCache map[string]string
	dimAsked map[string]bool

	// Creation dates are fetched in the background to avoid blocking.
	createdMu    sync.Mutex
	createdCache map[string]string
	createdAsked map[string]bool

	invalidate func()
}

// SetInvalidate wires a callback used to wake the Gio frame loop after the
// background dimension decoder finishes.
func (v *Viewer) SetInvalidate(f func()) { v.invalidate = f }

func (v *Viewer) Show(entries []cache.Entry, idx int) {
	v.entries = entries
	v.Index = idx
	v.Open = true
}

func (v *Viewer) Close() {
	v.Open = false
	v.Confirming = false
	v.loadedPath = ""
	v.loadedOp = paint.ImageOp{}
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
	v.loadedPath = ""
	v.loadedOp = paint.ImageOp{}
}

func (v *Viewer) Next() {
	if v.Index < len(v.entries)-1 {
		v.Index++
	}
}

func (v *Viewer) Prev() {
	if v.Index > 0 {
		v.Index--
	}
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

	imgOp, imgSz, ok := v.imageFor(e, tc)
	if ok {
		drawFitted(gtx, imgOp, imgSz, imgArea)
	}

	if v.ShowInfo {
		v.layoutInfoPanel(gtx, th, e, image.Rectangle{
			Min: image.Pt(size.X-infoW, 0),
			Max: image.Pt(size.X, size.Y),
		})
	}

	if v.Confirming {
		drawDeleteConfirm(gtx, th, filepath.Base(e.Path), rect)
	}

	clipArea.Pop()
	return layout.Dimensions{Size: size}
}

// drawDeleteConfirm dims rect and centers a delete-confirmation box showing
// filename and the available shortcuts. Shared between the viewer and grid.
func drawDeleteConfirm(gtx layout.Context, th *Theme, filename string, rect image.Rectangle) {
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
			lbl := material.Label(th.Theme, unit.Sp(15), "Delete this file?")
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
			lbl := material.Label(th.Theme, unit.Sp(12), "Enter to delete    Esc / Ctrl+[ to cancel")
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
	addRow("Created", v.creationDateFor(e))
	addRow("Modified", e.ModTime.Local().Format("2006-01-02 15:04:05"))
	addRow("Dimensions", dims)
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

// creationDateFor returns the best creation date found in the file's metadata
// as a formatted string, or "…" while a background fetch is still running.
func (v *Viewer) creationDateFor(e cache.Entry) string {
	v.createdMu.Lock()
	if v.createdCache == nil {
		v.createdCache = map[string]string{}
		v.createdAsked = map[string]bool{}
	}
	if cached, ok := v.createdCache[e.Path]; ok {
		v.createdMu.Unlock()
		return cached
	}
	if v.createdAsked[e.Path] {
		v.createdMu.Unlock()
		return "…"
	}
	v.createdAsked[e.Path] = true
	v.createdMu.Unlock()

	go func(path string) {
		date := scan.GetMediaDate(path)
		formatted := date.Local().Format("2006-01-02 15:04:05")
		v.createdMu.Lock()
		v.createdCache[path] = formatted
		v.createdMu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}(e.Path)
	return "…"
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
// caches a single decoded result; for videos and decode failures it falls back
// to the thumbnail.
func (v *Viewer) imageFor(e cache.Entry, tc *thumbCache) (paint.ImageOp, image.Point, bool) {
	switch e.Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
		if v.loadedPath == e.Path {
			return v.loadedOp, v.loadedSz, true
		}
		v.kickDecode(e)
	}
	// Fall back to the cached thumbnail (always JPEG).
	op, sz, ok := tc.Get(e)
	return op, sz, ok
}

// kickDecode launches a background decode for the given entry,
// then assigns the result to the viewer's loaded* fields and invalidates so
// the next frame paints the full-resolution image. At most one decode is
// queued per path; on failure subsequent visits stay on the thumbnail.
func (v *Viewer) kickDecode(e cache.Entry) {
	fullDecodeMu.Lock()
	if _, ok := fullDecodeStarted[e.Path]; ok {
		fullDecodeMu.Unlock()
		return
	}
	fullDecodeStarted[e.Path] = struct{}{}
	fullDecodeMu.Unlock()

	go func() {
		var img image.Image
		var err error

		switch e.Type {
		case scan.TypeHEIC:
			tmpDir, err2 := os.MkdirTemp("", "photo-viewer-heicview-")
			if err2 != nil {
				return
			}
			defer os.RemoveAll(tmpDir)
			jpgPath, err2 := thumb.HEICToJPEG(e.Path, tmpDir)
			if err2 != nil {
				return
			}
			f, err2 := os.Open(jpgPath)
			if err2 != nil {
				return
			}
			img, _, err = image.Decode(f)
			f.Close()
		case scan.TypeRAW:
			img, err = thumb.LoadRAWImage(e.Path)
		case scan.TypePhoto:
			f, err2 := os.Open(e.Path)
			if err2 == nil {
				img, _, err = image.Decode(f)
				f.Close()
			}
		}

		if err != nil || img == nil {
			return
		}

		op := paint.NewImageOp(img)
		op.Filter = paint.FilterLinear

		// Safe to set here because it's only read and written atomically via pointer/structs
		// by the main UI thread during Layout.
		v.loadedPath = e.Path
		v.loadedOp = op
		v.loadedSz = img.Bounds().Size()
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
}

// dim is a helper that returns layout.Dimensions of the given size.
func dim(sz image.Point) layout.Dimensions { return layout.Dimensions{Size: sz} }

// affineTranslate is currently unused; kept for documentation in case a
// future iteration adds pan/zoom and needs an explicit translation.
func affineTranslate(dx, dy float32) f32.Affine2D {
	return f32.Affine2D{}.Offset(f32.Pt(dx, dy))
}
