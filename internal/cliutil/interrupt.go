// Package cliutil holds the small pieces the cmd/ binaries share.
package cliutil

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// InterruptContext returns a context cancelled by the first SIGINT/SIGTERM, so
// a run stops between (or mid-copy within) files and cleans up its temp files
// instead of dying with a partial one in the destination (S-23). After that
// first signal the default handling is restored: a second Ctrl-C kills the
// process immediately.
func InterruptContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
		fmt.Fprintln(os.Stderr, "\nInterrupted — finishing up (press Ctrl-C again to force quit)…")
	}()
	return ctx
}
