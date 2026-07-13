package ui

import "testing"

// TestFormatBytesGio locks the U-16 byte-formatter consolidation: the single
// shared formatter emits raw bytes below 1 KiB and binary (1024-based) units
// with the accurate "iB" suffix above it — the same verdicts the old
// humanBytes/formatBytesGio pair produced, now unified on "KiB".
func TestFormatBytesGio(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024 / 2, "1.5 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := formatBytesGio(c.in); got != c.want {
			t.Errorf("formatBytesGio(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
