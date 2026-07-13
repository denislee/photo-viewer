// Package imgfit computes fit-within-a-bounding-box dimensions for image
// resampling. It is pure integer math with no project-internal dependencies,
// so the thumbnail, face-crop, and export-recompression paths can all share a
// single copy instead of each carrying a subtly divergent fitWithin.
package imgfit

// Within returns the dimensions of a w×h image scaled to fit inside a max×max
// box with the aspect ratio preserved and without upscaling: the longer edge
// becomes at most max and the other is scaled proportionally. Both returned
// dimensions are clamped to a minimum of 1.
//
// The clamp matters for extreme aspect ratios: without it the proportional edge
// truncates toward zero (e.g. Within(10000, 20, 256) would compute a height of
// 256*20/10000 == 0), and a 256×0 RGBA makes jpeg.Encode fail or emit a
// degenerate file — so banner scans and strip panoramas would fail the resize
// path entirely. A non-positive max (defensive) returns the input dimensions
// unchanged, still clamped to >= 1.
func Within(w, h, max int) (int, int) {
	if max <= 0 || (w <= max && h <= max) {
		return atLeastOne(w), atLeastOne(h)
	}
	if w >= h {
		return max, atLeastOne(max * h / w)
	}
	return atLeastOne(max * w / h), max
}

func atLeastOne(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
