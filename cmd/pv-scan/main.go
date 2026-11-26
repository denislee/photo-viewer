// Command pv-scan is a headless smoke-test entry point: it scans a directory
// and writes the cache, useful for verifying the pipeline without a display.
// It does no face detection: that is owned by the GUI.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/cliutil"
	"github.com/dns/photo-viewer/internal/scan"
)

const batchSize = 256

// resolveRoot makes -root absolute. The index is keyed by absolute path and
// the GUI always resolves its root, so a relative root here would store rows
// the GUI never matches and hash a different fallback db path (S6-01).
func resolveRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("-root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve -root: %w", err)
	}
	return abs, nil
}

func main() {
	root := flag.String("root", "", "library root to scan (required)")
	verbose := flag.Bool("v", false, "print one line per file (debug firehose) instead of periodic progress")
	flag.Parse()

	absRoot, err := resolveRoot(*root)
	if err != nil {
		log.Fatalf("pv-scan: %v", err)
	}

	cacheDir, dbPath, err := cache.Locate(absRoot, true)
	if err != nil {
		log.Fatalf("pv-scan: cache dir: %v", err)
	}
	fmt.Println("cache dir:", cacheDir)

	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Fatalf("pv-scan: open index: %v", err)
	}

	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		log.Fatalf("pv-scan: thumb store: %v", err)
	}

	// The first SIGINT/SIGTERM cancels ctx: the walk stops, queued thumbnails
	// are skipped, and the batch already walked is still reconciled and the
	// index saved (S-23). A second Ctrl-C force-quits.
	ctx := cliutil.InterruptContext()

	prog := &progress{out: os.Stdout, verbose: *verbose, every: progressEvery}

	// Thumb worker pool, hoisted out of the batch loop so it is created once
	// for the whole run instead of per 256-file batch. Workers generate the
	// thumbnail and record progress.
	workers := store.WarmUpConcurrency()
	thumbJobs := make(chan cache.Entry, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for e := range thumbJobs {
				if ctx.Err() != nil {
					continue // interrupted: drain without generating
				}
				p, perr := store.Path(e)
				prog.record(e.Path, p, perr)
			}
		})
	}

	opts := scan.WalkOptions{
		// Skip re-forking ffprobe for videos whose duration is already
		// stored for the current file version (X5-01).
		KnownDurationMs: idx.KnownDurationMs,
		OnError: func(path string, werr error) {
			log.Printf("pv-scan: walk error %s: %v", path, werr)
		},
	}

	var batch []scan.Result
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		for _, e := range idx.ReconcileBatch(batch) {
			thumbJobs <- e
		}
		batch = batch[:0]
	}

	for r := range scan.WalkWith(ctx, absRoot, opts) {
		batch = append(batch, r)
		if len(batch) >= batchSize {
			flushBatch()
		}
	}
	flushBatch()

	// No more entries: let the thumb workers drain, then finalise the index.
	close(thumbJobs)
	wg.Wait()
	fmt.Println(prog.line())

	if ctx.Err() != nil {
		closeIndex(idx)
		fmt.Println("interrupted: index saved; rerun to pick up where the scan stopped")
		os.Exit(130)
	}
	// Every writer has stopped by now, so Close (and its PRAGMA optimize) runs
	// against a quiet database.
	defer closeIndex(idx)

	fmt.Println("faces: detected by the photo-viewer GUI (People), not pv-scan")
}

// closeIndex closes idx, which also runs PRAGMA optimize.
func closeIndex(idx *cache.Index) {
	if err := idx.Close(); err != nil {
		log.Printf("pv-scan: close index: %v", err)
	}
}
