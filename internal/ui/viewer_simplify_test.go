package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

func TestSleepCtx(t *testing.T) {
	ctx := context.Background()
	if !sleepCtx(ctx, 0) {
		t.Fatal("sleepCtx with a zero duration should report the wait elapsed")
	}
	if !sleepCtx(ctx, time.Millisecond) {
		t.Fatal("sleepCtx with a short duration should report the wait elapsed")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(cancelled, time.Hour) {
		t.Fatal("sleepCtx should report false for a cancelled context")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("sleepCtx took %v to notice cancellation", elapsed)
	}
}

func TestCacheDropLocked(t *testing.T) {
	v := &Viewer{}
	v.recent = []recentImage{{path: "/a"}, {path: "/b"}, {path: "/c"}}

	v.recentMu.Lock()
	defer v.recentMu.Unlock()

	v.cacheDropLocked("/b")
	if len(v.recent) != 2 || v.recent[0].path != "/a" || v.recent[1].path != "/c" {
		t.Fatalf("after dropping /b, recent = %v; want [/a /c]", recentPaths(v.recent))
	}
	v.cacheDropLocked("/zz")
	if len(v.recent) != 2 || v.recent[0].path != "/a" || v.recent[1].path != "/c" {
		t.Fatalf("dropping an absent path changed recent to %v; want [/a /c]", recentPaths(v.recent))
	}
}

func recentPaths(rs []recentImage) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.path
	}
	return out
}

func TestHEICStageDirUnderCacheDir(t *testing.T) {
	store, err := cache.NewDisplayStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewDisplayStore: %v", err)
	}
	dir, err := heicStageDir(store)
	if err != nil {
		t.Fatalf("heicStageDir: %v", err)
	}
	defer os.RemoveAll(dir)

	if got, want := filepath.Dir(dir), store.CacheDir(); got != want {
		t.Fatalf("staging dir parent = %q; want %q", got, want)
	}
	if base := filepath.Base(dir); !strings.HasPrefix(base, heicStagePrefix) {
		t.Fatalf("staging dir base = %q; want prefix %q", base, heicStagePrefix)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat staging dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("staging path %q is not a directory", dir)
	}
}

func TestMediaInfoPlaceholder(t *testing.T) {
	v := &Viewer{}
	// The first call primes infoAsked and starts the background fetch; the
	// value it returns is the shared placeholder.
	if got := v.mediaInfoFor(cache.Entry{Path: "/nonexistent/x.jpg"}); got != pendingInfo {
		t.Fatalf("first mediaInfoFor = %+v; want pendingInfo %+v", got, pendingInfo)
	}
}

// TestSweepHEICStages: only stale heicview-* directories are reaped.
func TestSweepHEICStages(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk(heicStagePrefix+"1", 2*time.Hour)
	fresh := mk(heicStagePrefix+"2", time.Minute)
	other := mk("thumbs", 2*time.Hour)
	sweepHEICStages(dir, time.Hour, time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale stage dir survived: %v", err)
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
}
