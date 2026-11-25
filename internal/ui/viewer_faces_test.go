package ui

import (
	"image"
	"testing"

	"gioui.org/f32"
	"gioui.org/io/key"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestBoxRectMapsIntoDrawnImage guards the face-box geometry: a normalised
// box lands at the same fraction of wherever drawZoomed put the image,
// including a zoomed/panned image that starts off-screen.
func TestBoxRectMapsIntoDrawnImage(t *testing.T) {
	img := f32Rect{min: f32.Pt(100, 50), max: f32.Pt(300, 150)} // 200×100 drawn
	if got, want := boxRect([4]float64{0.5, 0.5, 0.25, 0.5}, img), image.Rect(200, 100, 250, 150); got != want {
		t.Errorf("boxRect = %v, want %v", got, want)
	}
	zoomed := f32Rect{min: f32.Pt(-400, -200), max: f32.Pt(400, 200)}
	if got, want := boxRect([4]float64{0, 0, 0.5, 0.5}, zoomed), image.Rect(-400, -200, 0, 0); got != want {
		t.Errorf("zoomed boxRect = %v, want %v", got, want)
	}
}

// TestViewerFaceKeys guards p (toggle boxes), t (cycle, wrapping) and Esc
// (deselect without closing the viewer); keys without a selection fall
// through to the viewer.
func TestViewerFaceKeys(t *testing.T) {
	c := newTestController(t, t.TempDir(), t.TempDir())
	vf := newViewerFaces(c, nil)
	path := "/lib/a.jpg"
	vf.path, vf.ver = path, c.PeopleVersion()
	vf.faces = []cache.FaceInfo{{Face: cache.Face{ID: 1}}, {Face: cache.Face{ID: 2}}}

	if !vf.handleKey(key.Event{Name: "P"}, path) || !vf.Show {
		t.Fatal("P should show boxes")
	}
	vf.handleKey(key.Event{Name: "T"}, path)
	vf.handleKey(key.Event{Name: "T"}, path)
	if vf.sel != 1 {
		t.Fatalf("two T presses: sel = %d, want 1", vf.sel)
	}
	vf.handleKey(key.Event{Name: "T"}, path)
	if vf.sel != 0 {
		t.Fatalf("T wraps: sel = %d, want 0", vf.sel)
	}
	if !vf.handleKey(key.Event{Name: key.NameEscape}, path) || vf.sel != -1 {
		t.Fatal("Esc should deselect and be consumed")
	}
	if vf.handleKey(key.Event{Name: key.NameEscape}, path) {
		t.Error("Esc with no selection must fall through (closes the viewer)")
	}
	if vf.handleKey(key.Event{Name: "N"}, path) {
		t.Error("N with no selection must fall through")
	}
	vf.handleKey(key.Event{Name: "P"}, path)
	if vf.Show || vf.sel != -1 {
		t.Error("P again should hide boxes and clear the selection")
	}
}
