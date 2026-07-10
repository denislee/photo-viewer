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

	type detected struct {
		entry cache.Entry
		thumb string
		mtime int64
	}

	// Collect entries in batches and fan out thumb generation.
	var (
		faceJobs []detected
		batch    []scan.Result
	)

	opts := scan.WalkOptions{
		OnError: func(path string, werr error) {
			log.Printf("pv-scan: walk error %s: %v", path, werr)
		},
	}

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		entries := idx.ReconcileBatch(batch)
		batch = batch[:0]

		// Fan out thumbnail generation across WarmUpConcurrency workers.
		workers := store.WarmUpConcurrency()
		type work struct {
			e cache.Entry
		}
		jobs := make(chan work, workers)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for w := range jobs {
					p, perr := store.Path(w.e)
					fmt.Printf("  %s -> %s (err=%v)\n", w.e.Path, p, perr)
					if perr == nil && p != "" {
						if info, statErr := os.Stat(p); statErr == nil {
							mu.Lock()
							faceJobs = append(faceJobs, detected{entry: w.e, thumb: p, mtime: info.ModTime().Unix()})
							mu.Unlock()
						}
					}
				}
			})
		}
		for _, e := range entries {
			jobs <- work{e: e}
		}
		close(jobs)
		wg.Wait()
	}

	for r := range scan.WalkWith(context.Background(), *root, opts) {
		batch = append(batch, r)
		if len(batch) >= batchSize {
			flushBatch()
		}
	}
	flushBatch()
	idx.Save()

	if *noFaces {
		return
	}
	pipe := face.NewPipeline(idx, nil)
	if !pipe.Enabled() {
		fmt.Println("face: pv-face-detect unavailable, skipping detection")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe.Start(ctx)
	for _, j := range faceJobs {
		pipe.SubmitBlocking(ctx, face.Job{Entry: j.entry, ThumbPath: j.thumb, ThumbMod: j.mtime})
	}
	pipe.Stop()

	clusters := idx.AllClusters()
	fmt.Printf("face: %d clusters\n", len(clusters))
}
