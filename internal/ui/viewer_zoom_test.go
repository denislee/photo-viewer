package ui

import (
	"image"
	"math"
	"testing"

	"gioui.org/app"
	"gioui.org/f32"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

func zoomViewer(typ scan.MediaType) *Viewer {
	v := &Viewer{Open: true, entries: []cache.Entry{{Path: "/a", Type: typ}, {Path: "/b", Type: typ}}}
	v.zoom.area = image.Rect(0, 0, 1000, 500)
	// A 2:1 photo fills the area exactly at scale 1.
	v.zoom.fit = f32.Pt(1000, 500)
	return v
}

func near(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-3 }

func TestPinchZoomsAboutPointer(t *testing.T) {
	v := zoomViewer(scan.TypePhoto)
	v.zoom.hover, v.zoom.hovering = f32.Pt(750, 250), true
	if !v.Pinch(app.PinchEvent{Kind: app.PinchUpdate, Scale: 2}) {
		t.Fatal("pinch did not change the view")
	}
	if !near(v.zoom.scale, 2) {
		t.Fatalf("scale = %v, want 2", v.zoom.scale)
	}
	// The point under the pointer (250 px right of centre) stays put: it
	// moves to 500 px right of the scaled centre, so the centre shifts -250.
	if !near(v.zoom.pan.X, -250) || !near(v.zoom.pan.Y, 0) {
		t.Fatalf("pan = %v, want (-250, 0)", v.zoom.pan)
	}
}

func TestPinchClampsScaleAndPan(t *testing.T) {
	v := zoomViewer(scan.TypePhoto)
	v.Pinch(app.PinchEvent{Kind: app.PinchUpdate, Scale: 0.5})
	if v.Zoomed() || v.zoom.pan != (f32.Point{}) {
		t.Fatalf("zoomed out below fit: scale %v pan %v", v.zoom.scale, v.zoom.pan)
	}
	v.Pinch(app.PinchEvent{Kind: app.PinchUpdate, Scale: 100, Pan: f32.Pt(1e6, -1e6)})
	if !near(v.zoom.scale, viewerMaxZoom) {
		t.Fatalf("scale = %v, want %v", v.zoom.scale, viewerMaxZoom)
	}
	// At 10× the image is 10000×5000: pan is limited to half the overflow.
	if !near(v.zoom.pan.X, 4500) || !near(v.zoom.pan.Y, -2250) {
		t.Fatalf("pan = %v, want (4500, -2250)", v.zoom.pan)
	}
}

func TestPinchIgnoredOutsidePhotos(t *testing.T) {
	for name, v := range map[string]*Viewer{
		"video":      zoomViewer(scan.TypeVideo),
		"closed":     func() *Viewer { v := zoomViewer(scan.TypePhoto); v.Open = false; return v }(),
		"confirming": func() *Viewer { v := zoomViewer(scan.TypePhoto); v.Confirming = true; return v }(),
	} {
		if v.Pinch(app.PinchEvent{Kind: app.PinchUpdate, Scale: 2}) || v.Zoomed() {
			t.Errorf("%s: pinch zoomed", name)
		}
	}
	v := zoomViewer(scan.TypePhoto)
	if v.Pinch(app.PinchEvent{Kind: app.PinchBegin, Scale: 2}) || v.Zoomed() {
		t.Error("PinchBegin zoomed")
	}
}

func TestZoomResetsOnNavigation(t *testing.T) {
	v := zoomViewer(scan.TypePhoto)
	v.Pinch(app.PinchEvent{Kind: app.PinchUpdate, Scale: 3})
	v.Next()
	if v.Zoomed() || v.zoom.pan != (f32.Point{}) {
		t.Fatalf("zoom survived Next: scale %v pan %v", v.zoom.scale, v.zoom.pan)
	}
}

func TestZoomTarget(t *testing.T) {
	v := zoomViewer(scan.TypePhoto)
	area := image.Pt(1000, 500)
	if got := v.zoomTarget(area); got != area {
		t.Fatalf("unzoomed target = %v", got)
	}
	v.zoom.scale = 1.5
	if got := v.zoomTarget(area); got != image.Pt(2000, 1000) {
		t.Fatalf("1.5× target = %v, want 2000x1000", got)
	}
	v.zoom.scale = 9
	if got := v.zoomTarget(area); got != image.Pt(viewerMaxPreviewSide, viewerMaxPreviewSide/2) {
		t.Fatalf("9× target = %v, want capped", got)
	}
}
