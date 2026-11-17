package cache

import "testing"

// TestPathYear pins the YYYY-MM-DD parent-directory parsing: only a
// fully-numeric date directory yields a year, and years below 1900 (or
// non-numeric segments, wrong lengths) fall back to 0.
func TestPathYear(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/a/2024-03-15/x.jpg", 2024},
		{"/a/1900-01-01/x.jpg", 1900},
		{"/a/9999-12-31/x.jpg", 9999},
		{"/a/1899-12-31/x.jpg", 0},
		{"/a/2024-0x-15/x.jpg", 0},
		{"/a/2024-03-1x/x.jpg", 0},
		{"/a/20a4-03-15/x.jpg", 0},
		{"/a/2024_03_15/x.jpg", 0},
		{"/a/2024-03-150/x.jpg", 0},
		{"/a/2024-3-15/x.jpg", 0},
		{"x.jpg", 0},
	} {
		if got := pathYear(tc.path); got != tc.want {
			t.Errorf("pathYear(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}
