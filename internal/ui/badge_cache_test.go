package ui

import (
	"image"
	"image/color"
	"strconv"
	"testing"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// resetBadgeCache clears the package-level badge label cache so a test starts
// from a known state and leaves nothing behind for the next test.
func resetBadgeCache(t *testing.T) {
	t.Helper()
	badgeLabels.m = nil
	badgeLabels.ops.Reset()
	t.Cleanup(func() {
		badgeLabels.m = nil
		badgeLabels.ops.Reset()
	})
}

// TestCachedBadgeLabelReuses covers G5-02: a label is laid out once per key and
// replayed from the cache afterwards, and beginBadgeFrame clears the cache once
// it reaches its cap.
func TestCachedBadgeLabelReuses(t *testing.T) {
	resetBadgeCache(t)
	th := NewTheme()
	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(160, 160)),
	}

	var calls int
	record := func(gtx layout.Context) layout.Dimensions {
		calls++
		lbl := material.Label(th.Theme, unit.Sp(16), "★")
		lbl.Color = color.NRGBA{R: 0xff, A: 0xff}
		return lbl.Layout(gtx)
	}

	key := badgeLabelKey{
		text:   "★",
		sp:     unit.Sp(16),
		col:    color.NRGBA{R: 0xff, A: 0xff},
		box:    image.Pt(26, 26),
		metric: gtx.Metric,
	}
	first := cachedBadgeLabel(gtx, key, record)
	if calls != 1 {
		t.Fatalf("record ran %d times on first call, want 1", calls)
	}
	second := cachedBadgeLabel(gtx, key, record)
	if calls != 1 {
		t.Fatalf("record ran %d times on identical key, want 1 (cache hit)", calls)
	}
	if second != first {
		t.Fatalf("cached label = %+v, want %+v", second, first)
	}
	first.call.Add(&ops)

	other := key
	other.text = "✓"
	cachedBadgeLabel(gtx, other, record)
	if calls != 2 {
		t.Fatalf("record ran %d times after a different key, want 2", calls)
	}

	// Fill past the cap: cachedBadgeLabel never resets mid-frame, so the map
	// grows past badgeLabelCap until the next frame's beginBadgeFrame.
	for i := 0; i <= badgeLabelCap; i++ {
		k := key
		k.text = "g" + strconv.Itoa(i)
		cachedBadgeLabel(gtx, k, record)
	}
	if n := len(badgeLabels.m); n <= badgeLabelCap {
		t.Fatalf("badgeLabels has %d entries, want > %d before beginBadgeFrame", n, badgeLabelCap)
	}
	beginBadgeFrame()
	if n := len(badgeLabels.m); n != 0 {
		t.Fatalf("beginBadgeFrame left %d entries, want 0", n)
	}
	// The reset ops are reusable: a label recorded after it is valid.
	var fresh op.Ops
	third := cachedBadgeLabel(gtx, key, record)
	third.call.Add(&fresh)
}

// BenchmarkDrawGlyphBadge measures drawing 300 badges into a reset op.Ops per
// iteration, the dense-grid case G5-02 optimises.
func BenchmarkDrawGlyphBadge(b *testing.B) {
	th := NewTheme()
	badgeLabels.m = nil
	badgeLabels.ops.Reset()

	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(160, 160)),
	}
	cell := image.Rect(0, 0, 160, 160)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ops.Reset()
		beginBadgeFrame()
		for j := 0; j < 300; j++ {
			drawGlyphBadge(gtx, th, cell, cornerBottomRight, "▶", unit.Sp(16), th.Foreground)
		}
	}
}
