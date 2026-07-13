// Command pv-scan is a headless smoke-test entry point: it scans a directory
// and writes the cache, useful for verifying the pipeline without a display.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
	"github.com/dns/photo-viewer/internal/scan"
)

const batchSize = 256

func main() {
	root := flag.String("root", "", "library root to scan (required)")
	noFaces := flag.Bool("no-faces", false, "skip face detection even if pv-face-detect is installed")
	verbose := flag.Bool("v", false, "print one line per file (debug firehose) instead of periodic progress")
	flag.Parse()

	if *root == "" {
		log.Fatal("pv-scan: -root is required")
	}

	cacheDir, err := cache.CacheDir(*root)
	if err != nil {
		log.Fatalf("pv-scan: cache dir: %v", err)
	}
	fmt.Println("cache dir:", cacheDir)

	dbPath := cache.IndexPath(*root, cacheDir)
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Fatalf("pv-scan: open index: %v", err)
	}

	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		log.Fatalf("pv-scan: thumb store: %v", err)
	}

	// Start the face pipeline up front so face jobs stream as thumbnails are
	// produced, rather than accumulating a library-sized backlog before any
	// detection begins. Start and SubmitBlocking are both no-ops when the
	// helper is unavailable, so this degrades gracefully.
	pipe := face.NewPipeline(idx, nil)
	faceCtx, faceCancel := context.WithCancel(context.Background())
	defer faceCancel()
	facesActive := !*noFaces && pipe.Enabled()
	if facesActive {
		pipe.Start(faceCtx)
	}

	prog := &progress{out: os.Stdout, verbose: *verbose, every: progressEvery}

	// Thumb worker pool, hoisted out of the batch loop so it is created once
	// for the whole run instead of per 256-file batch. Workers generate the
	// thumbnail, record progress, and stream the face job for each entry.
	workers := store.WarmUpConcurrency()
	thumbJobs := make(chan cache.Entry, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for e := range thumbJobs {
				p, perr := store.Path(e)
				prog.record(e.Path, p, perr)
				if facesActive && perr == nil && p != "" {
					if info, statErr := os.Stat(p); statErr == nil {
						pipe.SubmitBlocking(faceCtx, face.Job{
							Entry:     e,
							ThumbPath: p,
							ThumbMod:  info.ModTime().Unix(),
						})
					}
				}
			}
		})
	}

	opts := scan.WalkOptions{
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

	for r := range scan.WalkWith(context.Background(), *root, opts) {
		batch = append(batch, r)
		if len(batch) >= batchSize {
			flushBatch()
		}
	}
	flushBatch()

	// No more entries: let the thumb workers drain, then finalise the index.
	close(thumbJobs)
	wg.Wait()
	idx.Save()
	fmt.Println(prog.line())

	// Drain any face jobs still queued and wait for the workers to exit.
	pipe.Stop()

	if *noFaces {
		return
	}
	if !pipe.Enabled() {
		fmt.Println("face: pv-face-detect unavailable, skipping detection")
		return
	}
	clusters := idx.AllClusters()
	fmt.Printf("face: %d clusters\n", len(clusters))
}
