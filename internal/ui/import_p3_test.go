package ui

import (
	"fmt"
	"testing"
	"time"
)

// TestImportShowResolvesOffFrame is the U5-08 regression: Show used to run
// importDirsProblem's EvalSymlinks on the frame goroutine, which can block for
// seconds on an autofs mount or a spun-down disk. Show must return promptly,
// show "Checking import folders…", and then land the real inbox/outbox line
// from the background check.
func TestImportShowResolvesOffFrame(t *testing.T) {
	resetConfigForTest(t)
	inbox := t.TempDir()
	outbox := t.TempDir()
	UpdateConfig(func(c *Config) {
		c.InboxDir = inbox
		c.OutboxDir = outbox
	})

	v := NewImportView(func() {})

	start := time.Now()
	v.Show()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Show blocked for %v, want < 200ms (the dir check must run off-frame)", elapsed)
	}

	want := fmt.Sprintf("Inbox: %s   Outbox: %s", inbox, outbox)
	deadline := time.Now().Add(2 * time.Second)
	for {
		v.mu.Lock()
		msg := v.statusMsg
		v.mu.Unlock()
		if msg == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statusMsg = %q, want %q within 2s", msg, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
