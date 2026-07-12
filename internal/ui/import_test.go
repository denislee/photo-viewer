package ui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// errAfterReader yields up to fail good bytes, then fails — simulating a
// truncated read (yanked SD card, ZIP corruption) partway through a copy.
type errAfterReader struct {
	data []byte
	pos  int
	fail int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.fail {
		return 0, errors.New("injected read failure")
	}
	n := copy(p, r.data[r.pos:min(r.fail, len(r.data))])
	r.pos += n
	return n, nil
}

// TestWriteFileDurableSuccess verifies the happy path publishes the full
// content atomically and leaves no .tmp sidecar behind.
func TestWriteFileDurableSuccess(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0001.jpg")
	want := []byte("the full media file contents")

	if err := writeFileDurable(dst, bytes.NewReader(want)); err != nil {
		t.Fatalf("writeFileDurable: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("dst content = %q, want %q", got, want)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp sidecar was left behind: %v", err)
	}
}

// TestWriteFileDurableNoPartialOnError is the crash-safety contract behind U-01:
// a write that fails midway must never leave a truncated file at the final name
// (which the next inbox walk would file into the library as valid media), and
// must clean up its .tmp too.
func TestWriteFileDurableNoPartialOnError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0002.jpg")

	r := &errAfterReader{data: []byte("partial-then-boom-more-bytes"), fail: 8}
	err := writeFileDurable(dst, r)
	if err == nil {
		t.Fatal("expected writeFileDurable to return the injected error, got nil")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("truncated file left at final name %s (stat err: %v)", dst, statErr)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("partial .tmp left behind (stat err: %v)", statErr)
	}
}
