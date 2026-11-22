package ui

import (
	"image"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/scan"
)

// viewerMaxZoom bounds the zoom factor, relative to the image fitted to the
// viewer area.
const viewerMaxZoom = 10

// viewerScrollSpan is the scroll range the zoomed image claims per axis, large
// enough that a two-finger scroll is never clamped by Gio.
const viewerScrollSpan = 1 << 20

// viewerZoom is the photo viewer's zoom state: touchpad pinch zooms about the
// pointer, and while zoomed a drag or two-finger scroll pans. It resets on
// every navigation. UI goroutine only.
type viewerZoom struct {
	// scale is the zoom factor over the fitted size; values below 1 (the
	// zero value) mean not zoomed.
	scale float32
	// pan is the image centre's offset from the area centre, in pixels.
	pan f32.Point
	// area and fit are the image area and the image's fitted (unzoomed)
	// size from the last Layout, used to clamp pan.
	area image.Rectangle
	fit  f32.Point
	// hover is the last pointer position inside the viewer, the pinch anchor.
	hover    f32.Point
	hovering bool
	dragging bool
	dragLast f32.Point
}

func (z *viewerZoom) factor() float32 {
	return max(z.scale, 1)
}

// Zoomed reports whether the current photo is zoomed in.
func (v *Viewer) Zoomed() bool {
	return v.zoom.factor() > 1
}

// ResetZoom returns the current photo to its fitted size.
func (v *Viewer) ResetZoom() {
	v.zoom.scale = 1
	v.zoom.pan = f32.Point{}
	v.zoom.dragging = false
}

// zoomable reports whether the viewer shows a still image that can be zoomed.
func (v *Viewer) zoomable() bool {
	if !v.Open || v.Confirming || v.Index < 0 || v.Index >= len(v.entries) {
		return false
	}
	switch v.entries[v.Index].Type {
	case scan.TypePhoto, scan.TypeRAW, scan.TypeHEIC:
		return true
	}
	return false
}

// Pinch applies one touchpad pinch step and reports whether the view changed.
func (v *Viewer) Pinch(e app.PinchEvent) bool {
	if e.Kind != app.PinchUpdate || !v.zoomable() {
		return false
	}
	at := v.zoom.hover
	if !v.zoom.hovering {
		at = layout.FPt(v.zoom.area.Min.Add(v.zoom.area.Max).Div(2))
	}
	before := v.zoom
	v.zoomAt(e.Scale, at)
	v.panBy(e.Pan)
	return v.zoom.scale != before.scale || v.zoom.pan != before.pan
}

// zoomAt multiplies the zoom by factor, keeping the image point under at fixed.
func (v *Viewer) zoomAt(factor float32, at f32.Point) {
	z := &v.zoom
	old := z.factor()
	ns := min(max(old*factor, 1), viewerMaxZoom)
	c := layout.FPt(z.area.Min.Add(z.area.Max)).Mul(0.5)
	p := at.Sub(c)
	z.pan = z.pan.Sub(p).Mul(ns / old).Add(p)
	z.scale = ns
	v.clampPan()
}

func (v *Viewer) panBy(d f32.Point) {
	v.zoom.pan = v.zoom.pan.Add(d)
	v.clampPan()
}

// clampPan keeps the zoomed image covering the area on any axis where it is
// larger than the area, and centred on an axis where it is smaller.
func (v *Viewer) clampPan() {
	z := &v.zoom
	s := z.factor()
	lim := func(drawn float32, span int) float32 {
		return max((drawn-float32(span))/2, 0)
	}
	mx, my := lim(z.fit.X*s, z.area.Dx()), lim(z.fit.Y*s, z.area.Dy())
	z.pan.X = min(max(z.pan.X, -mx), mx)
	z.pan.Y = min(max(z.pan.Y, -my), my)
}

// zoomTarget is the decode target for the current zoom: the area scaled by the
// next power of two at or above the zoom factor (so a pinch doesn't re-decode
// every frame), capped at viewerMaxPreviewSide on the long side.
func (v *Viewer) zoomTarget(area image.Point) image.Point {
	s := v.zoom.factor()
	if s <= 1 || area.X <= 0 || area.Y <= 0 {
		return area
	}
	step := 2
	for float32(step) < s {
		step *= 2
	}
	t := area.Mul(step)
	if long := max(t.X, t.Y); long > viewerMaxPreviewSide {
		t = image.Pt(max(t.X*viewerMaxPreviewSide/long, 1), max(t.Y*viewerMaxPreviewSide/long, 1))
	}
	return t
}

// handleZoomPointer registers the image area for pointer input and applies
// the drag / scroll pans received since the last frame. It runs before the
// image is drawn, so the frame already shows the result.
func (v *Viewer) handleZoomPointer(gtx layout.Context, area image.Rectangle) {
	z := &v.zoom
	zoomed := v.Zoomed()
	span := 0
	if zoomed {
		span = viewerScrollSpan
	}
	for {
		ev, ok := gtx.Event(pointer.Filter{
			Target:  z,
			Kinds:   pointer.Move | pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel | pointer.Scroll | pointer.Leave,
			ScrollX: pointer.ScrollRange{Min: -span, Max: span},
			ScrollY: pointer.ScrollRange{Min: -span, Max: span},
		})
		if !ok {
			break
		}
		pe, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch pe.Kind {
		case pointer.Move:
			z.hover, z.hovering = pe.Position, true
		case pointer.Leave:
			z.hovering = false
		case pointer.Press:
			z.hover, z.hovering = pe.Position, true
			if zoomed && pe.Buttons.Contain(pointer.ButtonPrimary) {
				z.dragging = true
				z.dragLast = pe.Position
			}
		case pointer.Drag:
			z.hover, z.hovering = pe.Position, true
			if z.dragging {
				v.panBy(pe.Position.Sub(z.dragLast))
				z.dragLast = pe.Position
			}
		case pointer.Release, pointer.Cancel:
			z.dragging = false
		case pointer.Scroll:
			if zoomed {
				v.panBy(pe.Scroll.Mul(-1))
			}
		}
	}
	// No offset: pointer positions must share drawZoomed's coordinates.
	cl := clip.Rect(area).Push(gtx.Ops)
	if zoomed {
		if z.dragging {
			pointer.CursorGrabbing.Add(gtx.Ops)
		} else {
			pointer.CursorGrab.Add(gtx.Ops)
		}
	}
	event.Op(gtx.Ops, z)
	cl.Pop()
}

// drawZoomed paints img fitted to area, then scaled by the zoom about the
// area centre and offset by the pan, clipped to area.
func (v *Viewer) drawZoomed(gtx layout.Context, img paint.ImageOp, imgSz image.Point, area image.Rectangle) {
	if imgSz.X <= 0 || imgSz.Y <= 0 || area.Dx() <= 0 || area.Dy() <= 0 {
		return
	}
	fit := min(float32(area.Dx())/float32(imgSz.X), float32(area.Dy())/float32(imgSz.Y))
	v.zoom.fit = layout.FPt(imgSz).Mul(fit)
	v.clampPan()
	s := fit * v.zoom.factor()
	drawn := layout.FPt(imgSz).Mul(s)
	c := layout.FPt(area.Min.Add(area.Max)).Mul(0.5)
	origin := c.Sub(drawn.Mul(0.5)).Add(v.zoom.pan)

	defer clip.Rect(area).Push(gtx.Ops).Pop()
	defer op.Affine(f32.AffineId().Scale(f32.Point{}, f32.Pt(s, s)).Offset(origin)).Push(gtx.Ops).Pop()
	img.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}
