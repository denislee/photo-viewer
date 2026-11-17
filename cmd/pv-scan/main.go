// Command pv-scan is a headless smoke-test entry point: it scans a directory
// and writes the cache, useful for verifying the pipeline without a display.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
	"github.com/dns/photo-viewer/internal/scan"
)

const batchSize = 256

// newFacePipeline builds the face pipeline. It is a var so tests can prove
// -no-faces never constructs one: face.NewPipeline runs the 1–15 s
// pv-face-detect --check probe (S4-01).
var newFacePipeline = func(idx *cache.Index) *face.Pipeline {
	return face.NewPipeline(idx, nil)
}

// startFaces returns nil when noFaces is set, so the helper is never probed.
// Otherwise it builds the pipeline and starts it if the helper works; a
// disabled pipeline is still returned so the caller can report it.
func startFaces(ctx context.Context, idx *cache.Index, noFaces bool) *face.Pipeline {
	if noFaces {
		return nil
	}
	p := newFacePipeline(idx)
	if p.Enabled() {
		p.Start(ctx)
	}
	return p
}

func main() {
	root := flag.String("root", "", "library root to scan (required)")
	noFaces := flag.Bool("no-faces", false, "skip face detection even if pv-face-detect is installed")
	verbose := flag.Bool("v", false, "print one line per file (debug firehose) instead of periodic progress")
	flag.Parse()

	if *root == "" {
		log.Fatal("pv-scan: -root is required")
	}

	cacheDir, dbPath, err := cache.Locate(*root, true)
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

	// Start the face pipeline up front so face jobs stream as thumbnails are
	// produced, rather than accumulating a library-sized backlog before any
	// detection begins. Start and SubmitBlocking are both no-ops when the
	// helper is unavailable, so this degrades gracefully, and -no-faces skips
	// building the pipeline at all so the slow --check probe never runs.
	// The first SIGINT/SIGTERM cancels ctx: the walk stops, queued thumbnails
	// and face jobs are skipped, and the batch already walked is still
	// reconciled and the index saved (S-23). A second Ctrl-C force-quits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()

	faceCtx, faceCancel := context.WithCancel(ctx)
	defer faceCancel()
	pipe := startFaces(faceCtx, idx, *noFaces)
	facesActive := pipe != nil && pipe.Enabled()

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
				if ctx.Err() != nil {
					continue // interrupted: drain without generating
				}
				p, perr := store.Path(e)
				prog.record(e.Path, p, perr)
				// Stat only thumbnails the pipeline would accept
				// (photo/RAW/HEIC); videos are refused anyway.
				if facesActive && perr == nil && p != "" && face.Candidate(e.Type) {
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

	for r := range scan.WalkWith(ctx, *root, opts) {
		batch = append(batch, r)
		if len(batch) >= batchSize {
			flushBatch()
		}
	}
	flushBatch()

	// No more entries: let the thumb workers drain, then finalise the index.
	close(thumbJobs)
	wg.Wait()
	if err := idx.Save(); err != nil {
		log.Printf("pv-scan: save index: %v", err)
	}
	fmt.Println(prog.line())

	// Drain any face jobs still queued and wait for the workers to exit.
	if pipe != nil {
		pipe.Stop()
	}

	if ctx.Err() != nil {
		fmt.Println("interrupted: index saved; rerun to pick up where the scan stopped")
		os.Exit(130)
	}

	if pipe == nil {
		return // -no-faces
	}
	if !pipe.Enabled() {
		fmt.Println("face: pv-face-detect unavailable, skipping detection")
		return
	}
	clusters := idx.AllClusters()
	fmt.Printf("face: %d clusters\n", len(clusters))
}
