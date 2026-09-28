package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dns/photo-viewer/internal/fsutil"
)

// ExportSelection copies paths into target on a background goroutine, shown
// (and cancellable) in the process bar. Name collisions — two selected files
// sharing a base name, or a file already in target — get a _N suffix; nothing
// in target is ever replaced (G-17). done (may be nil) runs after the process
// entry is gone, with the number copied and one error per file that wasn't.
func (c *Controller) ExportSelection(paths []string, target string, done func(copied int, errs []error)) {
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var proc *Process
		if c.processes != nil {
			proc = c.processes.Begin(ProcExportSelection, "Export selection", cancel, false)
			proc.SetTotal(int64(len(paths)))
			proc.SetStatus("Copying to " + target)
		}
		copied, errs := exportSelection(ctx, paths, target, func() {
			if proc != nil {
				proc.AddDone(1)
			}
		})
		if proc != nil {
			proc.End()
		}
		if done != nil {
			done(copied, errs)
		}
	}()
}

// exportSelection copies each path into target under its base name (or the
// next free _N variant) with the crash-safe, never-replacing copy, calling
// step after each file. It returns how many were copied and one error per file
// that wasn't; a cancel stops the loop and reports the files left undone.
func exportSelection(ctx context.Context, paths []string, target string, step func()) (int, []error) {
	var copied int
	var errs []error
	for i, src := range paths {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("cancelled: %d file(s) not exported", len(paths)-i))
			break
		}
		if _, err := fsutil.CopyUnique(ctx, src, target, filepath.Base(src)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(src), err))
		} else {
			copied++
		}
		if step != nil {
			step()
		}
	}
	return copied, errs
}

// exportSelectionSummary is the warning shown when a selection export had
// failures: the counts plus the first few errors (the full list goes to the
// log).
func exportSelectionSummary(copied, total int, target string, errs []error) string {
	const maxShown = 5
	var b strings.Builder
	fmt.Fprintf(&b, "Exported %d of %d files to %s.\n\n%d problem(s):", copied, total, target, len(errs))
	for i, err := range errs {
		if i == maxShown {
			fmt.Fprintf(&b, "\n… and %d more (see the log)", len(errs)-maxShown)
			break
		}
		b.WriteString("\n• " + err.Error())
	}
	return b.String()
}
