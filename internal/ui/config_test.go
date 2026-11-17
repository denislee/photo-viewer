package ui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetConfigForTest points the config file at a fresh temp dir, clears the
// in-memory config and both generations, and restores the real file writer on
// cleanup. It returns the path the config will be written to.
func resetConfigForTest(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfgWriteMu.Lock()
	cfgMu.Lock()
	cfg = Config{}
	cfgGen = 0
	cfgSavedGen = 0
	cfgMu.Unlock()
	cfgWriteMu.Unlock()

	t.Cleanup(func() {
		cfgWriteMu.Lock()
		writeConfigFile = writeFileAtomic
		cfgWriteMu.Unlock()
		_ = FlushConfig(5 * time.Second)
	})

	return configPath()
}

// setConfigWriter swaps the config file writer, e.g. to stall or fail writes.
func setConfigWriter(f func(string, []byte) error) {
	cfgWriteMu.Lock()
	writeConfigFile = f
	cfgWriteMu.Unlock()
}

// readConfigFile decodes the config file at p.
func readConfigFile(t *testing.T, p string) Config {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read config %s: %v", p, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("unmarshal config %s: %v", p, err)
	}
	return c
}

// TestUpdateConfigConcurrentUpdatesAllSurvive checks that concurrent
// UpdateConfig calls mutate the live config rather than racing on stale
// copies, so every increment survives into both memory and the file.
func TestUpdateConfigConcurrentUpdatesAllSurvive(t *testing.T) {
	p := resetConfigForTest(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				UpdateConfig(func(c *Config) { c.GridCellDp++ })
			} else {
				UpdateConfig(func(c *Config) { c.SidebarWidthDp++ })
			}
		}(i)
	}
	wg.Wait()

	if err := FlushConfig(5 * time.Second); err != nil {
		t.Fatalf("FlushConfig: %v", err)
	}

	got := readConfigFile(t, p)
	if got.GridCellDp != 50 {
		t.Errorf("file GridCellDp = %d, want 50", got.GridCellDp)
	}
	if got.SidebarWidthDp != 50 {
		t.Errorf("file SidebarWidthDp = %d, want 50", got.SidebarWidthDp)
	}

	mem := GetConfig()
	if mem.GridCellDp != 50 {
		t.Errorf("GetConfig GridCellDp = %d, want 50", mem.GridCellDp)
	}
	if mem.SidebarWidthDp != 50 {
		t.Errorf("GetConfig SidebarWidthDp = %d, want 50", mem.SidebarWidthDp)
	}
}

// TestUpdateConfigNonBlockingWithStalledWriter checks that UpdateConfig never
// waits on the disk: a stuck writer must not stall callers.
func TestUpdateConfigNonBlockingWithStalledWriter(t *testing.T) {
	p := resetConfigForTest(t)

	release := make(chan struct{})
	setConfigWriter(func(p string, d []byte) error {
		<-release
		return writeFileAtomic(p, d)
	})

	start := time.Now()
	for i := 0; i < 20; i++ {
		i := i
		UpdateConfig(func(c *Config) { c.WebServerPort = 1000 + i })
	}
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("20 UpdateConfig calls took %v with a stalled writer, want < 100ms", elapsed)
	}

	if got := GetConfig().WebServerPort; got != 1019 {
		t.Errorf("GetConfig WebServerPort = %d, want 1019", got)
	}

	close(release)
	if err := FlushConfig(5 * time.Second); err != nil {
		t.Fatalf("FlushConfig: %v", err)
	}
	if got := readConfigFile(t, p); got.WebServerPort != 1019 {
		t.Errorf("file WebServerPort = %d, want 1019", got.WebServerPort)
	}
}

// TestFlushConfigTimesOutOnHungWriter checks that FlushConfig gives up on a
// wedged disk instead of blocking the caller indefinitely.
func TestFlushConfigTimesOutOnHungWriter(t *testing.T) {
	resetConfigForTest(t)

	release := make(chan struct{})
	// Closed before this test returns so the cleanup FlushConfig can finish.
	defer close(release)
	setConfigWriter(func(string, []byte) error {
		<-release
		return nil
	})

	UpdateConfig(func(c *Config) { c.GroupByYear = true })

	if err := FlushConfig(50 * time.Millisecond); err == nil {
		t.Fatal("FlushConfig with a hung writer returned nil, want timeout error")
	}
}

// TestPersistConfigFailureKeepsOldFile checks that a failed save leaves the
// previous file intact and that the failed generation is retried later.
func TestPersistConfigFailureKeepsOldFile(t *testing.T) {
	p := resetConfigForTest(t)

	UpdateConfig(func(c *Config) { c.InboxDir = "/old" })
	if err := FlushConfig(5 * time.Second); err != nil {
		t.Fatalf("FlushConfig after first update: %v", err)
	}
	if got := readConfigFile(t, p).InboxDir; got != "/old" {
		t.Fatalf("file InboxDir = %q, want /old", got)
	}

	setConfigWriter(func(string, []byte) error { return errors.New("disk full") })
	UpdateConfig(func(c *Config) { c.InboxDir = "/new" })
	if err := FlushConfig(5 * time.Second); err == nil {
		t.Fatal("FlushConfig with failing writer returned nil, want error")
	}
	if got := readConfigFile(t, p).InboxDir; got != "/old" {
		t.Errorf("file InboxDir after failed save = %q, want /old", got)
	}

	setConfigWriter(writeFileAtomic)
	if err := FlushConfig(5 * time.Second); err != nil {
		t.Fatalf("FlushConfig after restoring writer: %v", err)
	}
	if got := readConfigFile(t, p).InboxDir; got != "/new" {
		t.Errorf("file InboxDir after retry = %q, want /new", got)
	}
}

// TestWriteFileAtomicReplacesAndLeavesNoTemp checks the happy path: the file
// is replaced (including creating parent dirs), no temp file is left behind,
// and the result is world-readable.
func TestWriteFileAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "config.json")

	if err := writeFileAtomic(p, []byte("one")); err != nil {
		t.Fatalf("writeFileAtomic(one): %v", err)
	}
	if err := writeFileAtomic(p, []byte("two")); err != nil {
		t.Fatalf("writeFileAtomic(two): %v", err)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if string(data) != "two" {
		t.Errorf("content = %q, want %q", string(data), "two")
	}

	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1: %v", len(entries), entries)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
}

// TestWriteFileAtomicRenameFailureCleansTemp checks that a failing rename
// (target is a non-empty directory) reports the error without leaving the
// temp file behind.
func TestWriteFileAtomicRenameFailureCleansTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// A non-empty directory can't be replaced by rename.
	if err := os.MkdirAll(filepath.Join(p, "child"), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	if err := writeFileAtomic(p, []byte("x")); err == nil {
		t.Fatal("writeFileAtomic over a non-empty directory returned nil, want error")
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is no longer a directory", p)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (temp file left behind): %v", len(entries), entries)
	}
}
