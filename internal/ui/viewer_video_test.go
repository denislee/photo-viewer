package ui

import (
	"image"
	"testing"

	"gioui.org/op/paint"
)

// TestViewerCloseDropsVideoFrame covers G4-06: the last video op aliases the
// player's frame buffer, so Close must forget it or the window-sized RGBA
// stays reachable after Stop released it.
func TestViewerCloseDropsVideoFrame(t *testing.T) {
	v := &Viewer{}
	v.lastVideoOp = paint.NewImageOp(image.NewRGBA(image.Rect(0, 0, 4, 4)))
	v.lastVideoPath = "/v/clip.mp4"
	v.lastVideoSize = image.Pt(4, 4)
	v.Close()
	if v.lastVideoOp != (paint.ImageOp{}) || v.lastVideoPath != "" || v.lastVideoSize != (image.Point{}) {
		t.Fatalf("Close kept the video frame: path=%q size=%v", v.lastVideoPath, v.lastVideoSize)
	}
}
