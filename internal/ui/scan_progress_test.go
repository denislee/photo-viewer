package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScanIntoReportsTotal verifies a scan publishes the number of files it
// will visit (IndexStatus.ScanTotal, and the process-bar total) so indexing
// progress can be shown as done / total rather than a bare running count.
func TestScanIntoReportsTotal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	for _, name := range []string{"a.jpg", "b.png", "sub/c.jpg", "notes.txt"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestController(t, root, filepath.Join(dir, "cache"))
	reg := NewProcessRegistry(func() {})
	c.SetProcessRegistry(reg)

	c.scanInto(context.Background(), root)

	// The count runs alongside the scan and may land just after it returns.
	deadline := time.Now().Add(5 * time.Second)
	for c.IndexStatus().ScanTotal == 0 {
		if time.Now().After(deadline) {
			t.Fatal("ScanTotal never set")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if st := c.IndexStatus(); st.ScanTotal != 3 || st.Batched != 3 {
		t.Fatalf("ScanTotal=%d Batched=%d, want 3 and 3", st.ScanTotal, st.Batched)
	}
}

func TestReconciledString(t *testing.T) {
	for _, tc := range []struct {
		st   IndexStatus
		want string
	}{
		{IndexStatus{Active: true, Batched: 120}, "120 (counting files…)"},
		{IndexStatus{Active: true, Batched: 120, ScanTotal: 400}, "120 / 400 (30%)"},
		{IndexStatus{Batched: 410, ScanTotal: 400}, "410 / 400 (100%)"},
		{IndexStatus{Batched: 7}, "7"},
	} {
		if got := reconciledString(tc.st); got != tc.want {
			t.Errorf("reconciledString(%+v) = %q, want %q", tc.st, got, tc.want)
		}
	}
}
