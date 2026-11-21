package scan

import (
	"testing"
	"time"
)

// TestSameDateFolderAcceptsBothLayouts covers S6-03: files pv-organize filed
// under YYYY/MM/DD must not look misplaced to the GUI's Organize.
func TestSameDateFolderAcceptsBothLayouts(t *testing.T) {
	d := time.Date(2024, 1, 2, 15, 4, 5, 0, time.Local)
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/lib/2024-01-02/a.jpg", true},
		{"/lib/2024/01/02/a.jpg", true},
		{"/lib/2024-01-03/a.jpg", false},
		{"/lib/2024/01/03/a.jpg", false},
		{"/lib/2023/01/02/a.jpg", false},
		{"/lib/01/02/a.jpg", false},
		{"/lib/misc/a.jpg", false},
		{"a.jpg", false},
	} {
		if got := SameDateFolder(c.path, d); got != c.want {
			t.Errorf("SameDateFolder(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
