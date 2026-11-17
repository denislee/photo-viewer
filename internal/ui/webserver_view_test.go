package ui

import (
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWebServerStopDoesNotBlock verifies U3-02: handleStop must return at once
// even when the server's Shutdown has to wait out its grace period for an open
// connection, and must report "Stopping…" until the background Stop finishes.
func TestWebServerStopDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	c := newTestController(t, root, filepath.Join(dir, "cache"))
	srv := c.NewWebServer()

	var inv atomic.Int64
	v := NewWebServerView(srv, func() { inv.Add(1) })

	if err := srv.Start("127.0.0.1", 0, ""); err != nil {
		t.Fatal(err)
	}

	// Hold an ACTIVE connection with unfinished headers so Shutdown has to
	// wait for the grace period instead of closing immediately.
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	// Give the server a moment to read the request.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	v.handleStop()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("handleStop blocked for %v, want an immediate return", elapsed)
	}

	v.mu.Lock()
	status := v.status
	v.mu.Unlock()
	if status != "Stopping…" {
		t.Fatalf("status = %q, want %q", status, "Stopping…")
	}

	// A second click while the Stop is in flight must also return immediately.
	start = time.Now()
	v.handleStop()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("second handleStop blocked for %v, want an immediate return", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		status = v.status
		v.mu.Unlock()
		if status == "Stopped." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want %q within 5s", status, "Stopped.")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := inv.Load(); got < 1 {
		t.Fatalf("invalidate called %d times, want at least 1", got)
	}
	if srv.Running() {
		t.Fatal("server still running after background Stop completed")
	}
}
