package ui

import "testing"

func TestVisibleRange(t *testing.T) {
	tests := []struct {
		name               string
		first, count, n    int
		wantStart, wantEnd int
	}{
		{"midlist pads both sides", 10, 20, 100, 9, 31},
		{"clamped at top", 0, 20, 100, 0, 21},
		{"clamped at bottom", 90, 20, 100, 89, 100},
		{"whole small list", 0, 5, 5, 0, 5},
		{"empty list", 0, 0, 0, 0, 0},
		{"count exceeds remaining", 3, 50, 10, 2, 10},
		{"stale first beyond n collapses", 50, 0, 10, 10, 10},
		{"single visible row", 7, 1, 20, 6, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := visibleRange(tt.first, tt.count, tt.n)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("visibleRange(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.first, tt.count, tt.n, start, end, tt.wantStart, tt.wantEnd)
			}
			// Invariants the drain loops rely on: never negative, never past n,
			// and start never past end (so `for i := start; i < end` is safe).
			if start < 0 || end > tt.n || start > end {
				t.Fatalf("visibleRange(%d, %d, %d) violated bounds: start=%d end=%d n=%d",
					tt.first, tt.count, tt.n, start, end, tt.n)
			}
		})
	}
}
