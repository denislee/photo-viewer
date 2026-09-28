package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExportSelectionNeverOverwrites is the G-17 regression: the old export
// copied every file to target/<base name>, so two selected files sharing a
// name overwrote each other and a same-named file already in the target was
// replaced. Each must now land on its own _N name, with the occupant intact.
func TestExportSelectionNeverOverwrites(t *testing.T) {
	lib, target := t.TempDir(), t.TempDir()
	var paths []string
	for i, day := range []string{"2024-01-01", "2024-02-02"} {
		dir := filepath.Join(lib, day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "IMG_0001.jpg")
		if err := os.WriteFile(p, []byte(fmt.Sprintf("selected %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if err := os.WriteFile(filepath.Join(target, "IMG_0001.jpg"), []byte("occupant"), 0o644); err != nil {
		t.Fatal(err)
	}

	steps := 0
	copied, errs := exportSelection(context.Background(), paths, target, func() { steps++ })
	if copied != 2 || len(errs) != 0 || steps != 2 {
		t.Fatalf("copied=%d errs=%v steps=%d, want 2, none, 2", copied, errs, steps)
	}
	for name, want := range map[string]string{
		"IMG_0001.jpg":   "occupant",
		"IMG_0001_1.jpg": "selected 0",
		"IMG_0001_2.jpg": "selected 1",
	} {
		if got, err := os.ReadFile(filepath.Join(target, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("export must copy, not move: %v", err)
		}
	}
}

// A failing file is reported (not swallowed, as before) and doesn't stop the
// rest of the selection.
func TestExportSelectionReportsFailures(t *testing.T) {
	lib, target := t.TempDir(), t.TempDir()
	good := filepath.Join(lib, "good.jpg")
	if err := os.WriteFile(good, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(lib, "gone.jpg")

	copied, errs := exportSelection(context.Background(), []string{missing, good}, target, nil)
	if copied != 1 {
		t.Errorf("copied = %d, want 1", copied)
	}
	if len(errs) != 1 || !errors.Is(errs[0], os.ErrNotExist) || !strings.Contains(errs[0].Error(), "gone.jpg") {
		t.Errorf("errs = %v, want one not-exist error naming gone.jpg", errs)
	}
	if _, err := os.Stat(filepath.Join(target, "good.jpg")); err != nil {
		t.Errorf("good.jpg not exported: %v", err)
	}
}

func TestExportSelectionCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copied, errs := exportSelection(ctx, []string{"a.jpg", "b.jpg"}, t.TempDir(), nil)
	if copied != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "2 file(s) not exported") {
		t.Errorf("copied=%d errs=%v, want 0 and one cancel error for 2 files", copied, errs)
	}
}

func TestExportSelectionSummary(t *testing.T) {
	var errs []error
	for i := range 7 {
		errs = append(errs, fmt.Errorf("f%d.jpg: boom", i))
	}
	got := exportSelectionSummary(3, 10, "/mnt/usb", errs)
	for _, want := range []string{"Exported 3 of 10 files to /mnt/usb.", "7 problem(s):", "• f0.jpg: boom", "• f4.jpg: boom", "… and 2 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "f5.jpg") {
		t.Errorf("summary should list only the first 5 errors:\n%s", got)
	}
}

// The controller wrapper runs the export off the calling goroutine and hands
// the outcome to done; with no process registry wired it still works.
func TestControllerExportSelection(t *testing.T) {
	lib, target := t.TempDir(), t.TempDir()
	src := filepath.Join(lib, "clip.mov")
	if err := os.WriteFile(src, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		copied int
		errs   []error
	}
	got := make(chan outcome, 1)
	(&Controller{}).ExportSelection([]string{src}, target, func(copied int, errs []error) {
		got <- outcome{copied, errs}
	})
	select {
	case o := <-got:
		if o.copied != 1 || len(o.errs) != 0 {
			t.Errorf("copied=%d errs=%v, want 1, none", o.copied, o.errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExportSelection never called done")
	}
	if b, err := os.ReadFile(filepath.Join(target, "clip.mov")); err != nil || string(b) != "video" {
		t.Errorf("exported copy = %q, %v", b, err)
	}
}
