package imgfit

import "testing"

func TestWithin(t *testing.T) {
	cases := []struct {
		name         string
		w, h, max    int
		wantW, wantH int
	}{
		{"square no-op fits", 100, 100, 256, 100, 100},
		{"both edges fit", 200, 120, 256, 200, 120},
		{"landscape downscale", 512, 256, 256, 256, 128},
		{"portrait downscale", 256, 512, 256, 128, 256},
		{"square downscale", 1000, 1000, 256, 256, 256},
		{"extreme landscape clamps height to 1", 10000, 20, 256, 256, 1},
		{"extreme portrait clamps width to 1", 20, 10000, 256, 1, 256},
		{"non-positive max returns input", 640, 480, 0, 640, 480},
		{"negative max returns input", 640, 480, -5, 640, 480},
		{"zero input clamps to 1", 0, 0, 256, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotW, gotH := Within(tc.w, tc.h, tc.max)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("Within(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.w, tc.h, tc.max, gotW, gotH, tc.wantW, tc.wantH)
			}
			if gotW < 1 || gotH < 1 {
				t.Errorf("Within(%d, %d, %d) returned a dimension < 1: (%d, %d)",
					tc.w, tc.h, tc.max, gotW, gotH)
			}
		})
	}
}
