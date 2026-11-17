package ui

import "testing"

// TestFormatDuration pins the millisecond -> "M:SS" / "H:MM:SS" rendering,
// including the round-half-up at 500 ms and the empty result for non-positive
// durations.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, ""},
		{-5, ""},
		{499, "0:00"},
		{500, "0:01"},
		{59_499, "0:59"},
		{61_000, "1:01"},
		{600_000, "10:00"},
		{3_599_000, "59:59"},
		{3_600_000, "1:00:00"},
		{3_661_000, "1:01:01"},
		{36_000_000, "10:00:00"},
		{360_000_000, "100:00:00"},
	}
	for _, c := range cases {
		if got := formatDuration(c.ms); got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// TestFmtDur3Allocs keeps the H:MM:SS path allocation-free apart from the
// returned string (G3-08).
func TestFmtDur3Allocs(t *testing.T) {
	if got, want := fmtDur3(12, 3, 4), "12:03:04"; got != want {
		t.Fatalf("fmtDur3(12, 3, 4) = %q, want %q", got, want)
	}
	if got, want := fmtDur3(0, 5, 6), "0:05:06"; got != want {
		t.Fatalf("fmtDur3(0, 5, 6) = %q, want %q", got, want)
	}
	if n := testing.AllocsPerRun(100, func() { _ = fmtDur3(12, 3, 4) }); n > 1 {
		t.Fatalf("fmtDur3 allocates %.1f times per run, want <= 1", n)
	}
}

// TestDurationTextCacheCapped checks that durationTextCache is bulk-cleared
// once it reaches its cap instead of growing without bound (G3-05).
func TestDurationTextCacheCapped(t *testing.T) {
	old := durationTextCache
	durationTextCache = map[int64]string{}
	t.Cleanup(func() { durationTextCache = old })

	for i := 0; i < 3000; i++ {
		cachedFormatDuration(int64(i+1) * 1000)
	}
	if n := len(durationTextCache); n > durationTextCacheCap {
		t.Fatalf("durationTextCache has %d entries, want <= %d", n, durationTextCacheCap)
	}
	if got, want := cachedFormatDuration(61_000), "1:01"; got != want {
		t.Fatalf("cachedFormatDuration(61000) = %q, want %q", got, want)
	}
}
