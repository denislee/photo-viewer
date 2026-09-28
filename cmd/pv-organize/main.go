package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/scan"
)

func main() {
	srcDir := flag.String("src", "", "source directory to organize")
	dstDir := flag.String("dst", "", "destination root directory")
	dryRun := flag.Bool("dry-run", false, "show what would be moved without moving")
	flag.Parse()

	if *srcDir == "" || *dstDir == "" {
		fmt.Println("Usage: pv-organize -src <source_dir> -dst <destination_dir> [-dry-run]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if _, err := exec.LookPath("exiftool"); err != nil {
		log.Fatal("Error: exiftool not found on PATH. It is required for metadata extraction.")
	}

	absDst, err := filepath.Abs(*dstDir)
	if err != nil {
		log.Fatalf("Error resolving destination path: %v", err)
	}

	count := 0
	failures := 0
	// claimed records destinations the dry-run planner has already handed out
	// this run so two identically-named sources resolve to distinct names —
	// exactly what the real run's atomic claim does when the first source takes
	// the base name and the second collides onto "_1". Without it the old
	// dry-run printed the same destination twice and lied about the outcome.
	claimed := map[string]bool{}

	// SkipDurationProbe: pv-organize only needs each file's path and date, never
	// a video's duration, so suppress the per-video ffprobe fork that a plain
	// Walk would pay for every clip.
	ctx := interruptContext()
	for res := range scan.WalkWith(ctx, *srcDir, scan.WalkOptions{SkipDurationProbe: true}) {
		if ctx.Err() != nil {
			break
		}
		if res.Type == scan.TypeUnknown {
			continue
		}

		// scan.GetMediaDate rides the shared -stay_open exiftool daemon
		// (≈two orders of magnitude faster than a fresh process per file) and
		// already falls back to the file's mtime when no metadata date exists,
		// so the old per-file exec + manual mtime fallback is gone.
		date := scan.GetMediaDate(res.Path)

		destSubDir := filepath.Join(absDst, date.Format("2006/01/02"))
		baseName := filepath.Base(res.Path)
		destPath := filepath.Join(destSubDir, baseName)

		// Avoid moving a file to itself or into its own subdirectory if src and
		// dst overlap: a file already sitting at its correct destination is
		// left untouched (a rename onto its own path would otherwise be pushed
		// to a needless "_1" suffix).
		absSrc, _ := filepath.Abs(res.Path)
		if absSrc == destPath {
			continue
		}

		if *dryRun {
			// Plan the destination with the SAME name sequence and collision
			// rule the real run uses, treating a path as taken if it exists on
			// disk OR we've already planned to fill it this run. This is what
			// makes the printed plan match a real run file-for-file.
			finalDest, err := planDest(destSubDir, baseName, claimed)
			if err != nil {
				log.Printf("Error planning destination for %s: %v", res.Path, err)
				failures++
				continue
			}
			claimed[finalDest] = true
			fmt.Printf("[Dry Run] %s -> %s\n", res.Path, finalDest)
			count++
			continue
		}

		if err := os.MkdirAll(destSubDir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v", destSubDir, err)
			failures++
			continue
		}

		// Never overwrites and never unlinks the source before the file is
		// durably in place; across filesystems (SD card → library disk, the
		// usual case) it becomes a cancellable fsynced copy.
		finalDest, err := fsutil.MoveUnique(ctx, absSrc, destSubDir, baseName)
		if err != nil {
			if ctx.Err() != nil {
				break // interrupted mid-copy: the move was rolled back, not a failure
			}
			log.Printf("Error moving %s: %v", res.Path, err)
			failures++
			continue
		}
		fmt.Printf("Moving %s -> %s\n", res.Path, finalDest)
		count++
	}

	if ctx.Err() != nil {
		fmt.Printf("\nInterrupted. Moved %d files (%d error(s)); nothing was left half-moved.\n", count, failures)
		os.Exit(130)
	}
	if *dryRun {
		fmt.Printf("\nDry run finished. Would have moved %d files (%d planning error(s)).\n", count, failures)
	} else {
		fmt.Printf("\nFinished. Moved %d files (%d error(s)).\n", count, failures)
	}
	// Mirror pv-export-favorites: any failed operation makes the whole run a
	// failure so scripts and cron jobs don't treat a partial move as success.
	if failures > 0 {
		os.Exit(1)
	}
}

// interruptContext returns a context cancelled by the first SIGINT/SIGTERM, so
// the run stops between (or mid-copy within) files and cleans up its temp
// instead of dying with a partial temp file in the destination (S-23).
// After that first signal the default handling is restored: a second Ctrl-C
// kills the process immediately.
func interruptContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
		fmt.Fprintln(os.Stderr, "\nInterrupted — finishing up (press Ctrl-C again to force quit)…")
	}()
	return ctx
}

// planDest returns the destination a real run would claim for baseName in
// destDir: the same SuffixName sequence MoveUnique walks, with names already
// planned this run (claimed) counted as taken, so -dry-run output matches a
// real run file for file.
func planDest(destDir, baseName string, claimed map[string]bool) (string, error) {
	return fsutil.FreeName(
		func(n int) string { return fsutil.SuffixName(destDir, baseName, n) },
		func(p string) bool { return claimed[p] || fsutil.Taken(p) },
	)
}
