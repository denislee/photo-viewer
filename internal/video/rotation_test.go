package video

import "testing"

// TestRotationVF pins the demux-rotation → vf mapping. Non-right angles and
// a missing property must map to "" (not be skipped): applyRotation always
// sets vf, so a rotated file's transpose doesn't leak into the next file.
func TestRotationVF(t *testing.T) {
	for _, tc := range []struct{ rot, want string }{
		{"", ""},
		{"0", ""},
		{"90", "lavfi=[transpose=clock]"},
		{"180", "lavfi=[hflip,vflip]"},
		{"270", "lavfi=[transpose=cclock]"},
		{"45", ""},
		{"-90", ""},
	} {
		if got := rotationVF(tc.rot); got != tc.want {
			t.Errorf("rotationVF(%q) = %q, want %q", tc.rot, got, tc.want)
		}
	}
}
