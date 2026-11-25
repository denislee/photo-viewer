package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dns/photo-viewer/internal/cliutil"
	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/scan"
	"github.com/dns/photo-viewer/internal/tools"
	"github.com/dns/photo-viewer/internal/workpool"
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

	if !tools.Exiftool() {
		log.Fatal("Error: exiftool not found on PATH. It is required for metadata extraction.")
	}

	absDst, err := filepath.Abs(*dstDir)
	if err != nil {
		log.Fatalf("Error resolving destination path: %v", err)
	}

	ctx := cliutil.InterruptContext()
	count, failures := organize(ctx, *srcDir, absDst, *dryRun, scan.ExiftoolPoolSize(), os.Stdout)

	if ctx.Err() != nil {
		if *dryRun {
			fmt.Printf("\nInterrupted. Planned %d moves (%d planning error(s)).\n", count, failures)
		} else {
			fmt.Printf("\nInterrupted. Moved %d files (%d error(s)); nothing was left half-moved.\n", count, failures)
		}
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

// mediaDate resolves a file's date. A var so tests run without exiftool.
var mediaDate = scan.GetMediaDate

// organize plans (dryRun) or performs the moves of every media file under
// srcDir into absDst/YYYY/MM/DD and returns how many files it moved or
// planned and how many failed. Per-file lines go to out.
func organize(ctx context.Context, srcDir, absDst string, dryRun bool, workers int, out io.Writer) (count, failures int) {
	absSrcDir, _ := filepath.Abs(srcDir)

	// claimed records destinations the dry-run planner has already handed out
	// this run so two identically-named sources resolve to distinct names —
	// exactly what the real run's atomic claim does when the first source takes
	// the base name and the second collides onto "_1". Without it the old
	// dry-run printed the same destination twice and lied about the outcome.
	claimed := map[string]bool{}

	var files []string
	err := scan.WalkMediaPaths(ctx, absSrcDir, func(path string, _ scan.MediaType) error {
		files = append(files, path)
		return nil
	})
	if err != nil || ctx.Err() != nil {
		return count, failures
	}

	// The walk emits paths in lexical (WalkDir) order; sorting keeps the
	// printed plan and the collision suffixes deterministic.
	slices.Sort(files)

	// scan.GetMediaDate rides the pooled -stay_open exiftool daemons, so
	// ExiftoolPoolSize concurrent callers resolve ~8x faster than a serial loop
	// (S4-05); each worker writes only its own slot.
	dates := make([]time.Time, len(files))
	// GetMediaDate already falls back to the file's mtime when no metadata date
	// exists, so the old per-file exec + manual mtime fallback is gone.
	workpool.RunIndexed(ctx, len(files), workers, func(i int) {
		dates[i] = mediaDate(files[i])
	})
	if ctx.Err() != nil {
		return count, failures
	}

	for i, path := range files {
		if ctx.Err() != nil {
			break
		}

		destSubDir := filepath.Join(absDst, dates[i].Format("2006/01/02"))
		baseName := filepath.Base(path)
		destPath := filepath.Join(destSubDir, baseName)

		// Avoid moving a file to itself or into its own subdirectory if src and
		// dst overlap: a file already sitting at its correct destination is
		// left untouched (a rename onto its own path would otherwise be pushed
		// to a needless "_1" suffix).
		if path == destPath {
			continue
		}

		if dryRun {
			// Plan the destination with the SAME name sequence and collision
			// rule the real run uses, treating a path as taken if it exists on
			// disk OR we've already planned to fill it this run. This is what
			// makes the printed plan match a real run file-for-file.
			finalDest, err := planDest(destSubDir, baseName, claimed)
			if err != nil {
				log.Printf("Error planning destination for %s: %v", path, err)
				failures++
				continue
			}
			claimed[finalDest] = true
			fmt.Fprintf(out, "[Dry Run] %s -> %s\n", path, finalDest)
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
		finalDest, err := fsutil.MoveUnique(ctx, path, destSubDir, baseName)
		if err != nil {
			if ctx.Err() != nil {
				break // interrupted mid-copy: the move was rolled back, not a failure
			}
			log.Printf("Error moving %s: %v", path, err)
			failures++
			continue
		}
		fmt.Fprintf(out, "Moving %s -> %s\n", path, finalDest)
		count++
	}

	return count, failures
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
