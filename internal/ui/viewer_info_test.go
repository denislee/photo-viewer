package ui

import (
	"image"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestInfoPanelRowsMemoized covers G3-03: the panel's text is rebuilt only
// when the entry or its metadata changes, not every frame.
func TestInfoPanelRowsMemoized(t *testing.T) {
	th := NewTheme()
	e := cache.Entry{Path: "/lib/clip.mp4", Type: scan.TypeVideo, Size: 1 << 20, ModTime: time.Unix(1700000000, 0)}
	v := &Viewer{}
	// Seed the metadata so no background fetch runs.
	v.infoCache = map[string]cachedInfo{e.Path: {Created: "2024-01-01 00:00:00", Camera: "—", Lens: "—", Aperture: "—", ShutterSpeed: "—", ISO: "—", FocalLength: "—"}}
	v.infoAsked = map[string]bool{}

	frame := func(e cache.Entry) {
		gtx := layout.Context{
			Ops:         new(op.Ops),
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(image.Pt(600, 800)),
		}
		v.layoutInfoPanel(gtx, th, e, image.Rect(300, 0, 600, 800))
	}
	for range 5 {
		frame(e)
	}
	if v.infoBuilds != 1 {
		t.Fatalf("rows built %d times over 5 identical frames, want 1", v.infoBuilds)
	}
	e.Favorite = true
	frame(e)
	frame(e)
	if v.infoBuilds != 2 {
		t.Fatalf("rows built %d times after a favorite toggle, want 2", v.infoBuilds)
	}
	if last := v.infoRows[len(v.infoRows)-1]; last != (infoRow{"Favorite", "Yes"}) {
		t.Fatalf("last row = %v, want Favorite: Yes", last)
	}
}

func TestBuildInfoRowsSettings(t *testing.T) {
	e := cache.Entry{Path: "/lib/a.jpg", Type: scan.TypePhoto}
	none := cachedInfo{Aperture: "—", ShutterSpeed: "—", ISO: "—", FocalLength: "—"}
	for _, r := range buildInfoRows(e, none, "1 × 1") {
		if r.label == "Settings" {
			t.Fatal("Settings row shown with no exposure data")
		}
	}
	some := none
	some.Aperture, some.ISO = "f/2.8", "100"
	rows := buildInfoRows(e, some, "1 × 1")
	found := false
	for _, r := range rows {
		if r.label == "Settings" {
			found = true
			if r.value != "f/2.8  —  ISO 100  —" {
				t.Fatalf("Settings = %q", r.value)
			}
		}
	}
	if !found || rows[0].value != "a.jpg" {
		t.Fatalf("rows = %v", rows)
	}
}
